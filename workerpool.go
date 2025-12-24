package workerpool

import (
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/aradilov/ringbuffer"
)

var (
	ErrTimeout       = fmt.Errorf("timeout")
	ErrNoFreeWorkers = fmt.Errorf("no free workers")
)

// New initializes and starts a new WorkerPool with the specified maximum concurrency and returns its reference.
func New(concurrency int) *WorkerPool {
	wp := &WorkerPool{
		WorkerFunc: func(cb func()) error {
			cb()
			return nil
		},
		MaxWorkersCount:       concurrency,
		MaxIdleWorkerDuration: 15 * time.Minute,
		LogAllErrors:          true,
	}
	wp.Start()
	return wp
}

// Stats represents the metrics and operational statistics of a worker pool
type Stats struct {
	Submitted     uint64
	WorkersCount  int32
	NoFreeWorkers uint64
	ReleaseFailed uint64
	FreeQFailed   uint64
	Timeout       uint64
}

// WorkerPool is a concurrent worker pool that efficiently handles tasks using a bounded number of workers.
type WorkerPool struct {
	WorkerFunc func(cb func()) error

	MaxWorkersCount       int
	LogAllErrors          bool
	MaxIdleWorkerDuration time.Duration

	submitted     atomic.Uint64
	noFreeWorkers atomic.Uint64
	releaseFailed atomic.Uint64
	freeQFailed   atomic.Uint64
	timeout       atomic.Uint64

	stopCh   chan struct{}
	mustStop atomic.Bool

	// active workers count (for observability)
	workersCount atomic.Int32

	// index registries (both bounded, CAS-based)
	readyQ *ringbuffer.MPMC[int] // idle workers
	freeQ  *ringbuffer.MPMC[int] // available indices for creating workers

	workers []workerSlot

	timers timerPool
}

type workerSlot struct {
	ch      chan *workTask
	lastUse atomic.Int64 // unix nano; meaningful when idle/queued
}

var workerChanCap = func() int {
	if runtime.GOMAXPROCS(0) == 1 {
		return 0
	}
	return 1
}()

// Stats returns a snapshot of the current worker pool metrics, including submitted and various operational statistics.
func (wp *WorkerPool) Stats() Stats {
	return Stats{
		Submitted:     wp.submitted.Load(),
		WorkersCount:  wp.workersCount.Load(),
		NoFreeWorkers: wp.noFreeWorkers.Load(),
		ReleaseFailed: wp.releaseFailed.Load(),
		FreeQFailed:   wp.freeQFailed.Load(),
		Timeout:       wp.timeout.Load(),
	}
}

func (wp *WorkerPool) Start() {
	if wp.stopCh != nil {
		panic("BUG: WorkerPool already started")
	}
	if wp.MaxWorkersCount == 0 || (wp.MaxWorkersCount&(wp.MaxWorkersCount-1)) != 0 {
		panic("BUG: MaxWorkersCount must be power of 2 and > 0")
	}

	wp.stopCh = make(chan struct{})

	wp.readyQ = ringbuffer.NewMPMC[int](uint64(wp.MaxWorkersCount))
	wp.freeQ = ringbuffer.NewMPMC[int](uint64(wp.MaxWorkersCount))
	wp.workers = make([]workerSlot, wp.MaxWorkersCount)

	// All indices are initially free to create workers.
	for i := 0; i < wp.MaxWorkersCount; i++ {
		if !wp.freeQ.Enqueue(i) {
			panic("unreached: freeQ init")
		}
	}

	stopCh := wp.stopCh
	go func() {
		t := time.NewTicker(wp.getMaxIdleWorkerDuration())
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				wp.clean()
			}
		}
	}()
}

func (wp *WorkerPool) Stop() {
	if wp.stopCh == nil {
		panic("BUG: WorkerPool wasn't started")
	}
	// signal stop
	wp.mustStop.Store(true)
	close(wp.stopCh)
	wp.stopCh = nil

	// Stop all idle workers by draining readyQ and sending nil.
	for {
		idx, ok := wp.readyQ.Dequeue()
		if !ok {
			break
		}
		// idx is idle (by contract: only idle workers are in readyQ)
		// send nil to stop worker
		wp.workers[idx].ch <- nil
	}
	// Busy workers will stop after finishing and calling status().
}

// Do schedules the given callback function `cb` to be executed by a worker within the specified deadline duration.
// Returns an error if no workers are available or if the deadline expires before the task is completed.
func (wp *WorkerPool) Do(cb func(), timeout time.Duration) error {
	wp.submitted.Add(1)

	slot := wp.getSlot()
	if slot == nil {
		wp.noFreeWorkers.Add(1)
		return ErrNoFreeWorkers
	}

	task := acquireTask()
	task.wait = true
	task.cb = cb
	task.status.Store(StatusQueued)
	slot.ch <- task

	timer := wp.timers.Get(timeout)
	defer wp.timers.Put(timer)

	select {
	case err := <-task.done:
		for {
			st := task.status.Load()
			if st == StatusDone {
				if task.status.CompareAndSwap(StatusDone, StatusReleased) {
					releaseTask(task)
					return err
				}
				continue
			}
			// the worker has not yet had time to set Done after send
			runtime.Gosched()
		}

		return err
	case <-timer.C:
		wp.timeout.Add(1)
	slowpath:
		st := task.status.Load()
		switch st {
		case StatusQueued, StatusProgress:
			if !task.status.CompareAndSwap(st, StatusReleased) {
				goto slowpath
			}
			return ErrTimeout

		case StatusDone:
			if task.status.CompareAndSwap(StatusDone, StatusReleased) {
				releaseTask(task)
			} else {
				panic("worker pool invariant violation")
			}

			return ErrTimeout

		default:
			panic(fmt.Sprintf("BUG: unexpected task status: %d", st))
		}

	}
}

// Serve schedules a task represented by the callback `cb` to be executed by a worker in the pool.
// Returns true if the task was successfully scheduled, false if no workers are available.
func (wp *WorkerPool) Serve(cb func()) bool {
	wp.submitted.Add(1)

	slot := wp.getSlot()
	if slot == nil {
		wp.noFreeWorkers.Add(1)
		return false
	}

	task := acquireTask()
	task.cb = cb
	task.status.Store(StatusQueued)
	slot.ch <- task
	return true
}

func (wp *WorkerPool) getSlot() *workerSlot {
	// Fast path: reuse an idle worker
	if idx, ok := wp.readyQ.Dequeue(); ok {
		return &wp.workers[idx]
	}

	// No idle workers -> try to create a new one (bounded by freeQ)
	idx, ok := wp.freeQ.Dequeue()
	if !ok {
		return nil // reached MaxWorkersCount
	}

	// Initialize slot lazily
	slot := &wp.workers[idx]
	if slot.ch == nil {
		slot.ch = make(chan *workTask, workerChanCap)
	}
	wp.workersCount.Add(1)

	go wp.workerFunc(idx)
	return slot
}

func (wp *WorkerPool) release(idx int) bool {
	if wp.mustStop.Load() {
		return false
	}

	var failed int
	// Return to idle registry
	for !wp.readyQ.Enqueue(idx) {
		failed++
		// should not happen if logic is correct, but keep it safe
		runtime.Gosched()
	}
	if failed > 0 {
		wp.releaseFailed.Add(uint64(failed))
	}

	return true
}

// workerFunc is executed by each worker in the pool, processing tasks from a dedicated channel until termination.
func (wp *WorkerPool) workerFunc(idx int) {
	slot := &wp.workers[idx]
	var err error

	for task := range slot.ch {
		if task == nil {
			break
		}

		// releaseTask can only be called in workerFunc or Do in Done status
		// so it's safe to move between statuses without worrying about race cond
		if task.status.CompareAndSwap(StatusQueued, StatusProgress) {
			if !task.wait {
				err = wp.WorkerFunc(task.cb)
				task.status.Store(StatusDone)
				releaseTask(task)
			} else {
				err = wp.WorkerFunc(task.cb)
				select {
				case task.done <- err:
				default:
				}

				if !task.status.CompareAndSwap(StatusProgress, StatusDone) {
					if task.status.CompareAndSwap(StatusReleased, StatusDone) {
						releaseTask(task)
					} else {
						panic(fmt.Sprintf("worker pool invariant violation: %d", task.status.Load()))
					}
				}

			}

			if err != nil && wp.LogAllErrors {
				log.Printf("worker error: %s", err)
			}
		}

		slot.lastUse.Store(time.Now().UnixNano())
		if !wp.release(idx) {
			break
		}

	}

	// worker exits: mark index as free for future creations
	wp.workersCount.Add(-1)
	var failed int
	for !wp.freeQ.Enqueue(idx) {
		failed++
		runtime.Gosched()
	}
	if failed > 0 {
		wp.freeQFailed.Add(uint64(failed))
	}

}

func (wp *WorkerPool) clean() {
	maxIdle := wp.getMaxIdleWorkerDuration()
	if maxIdle <= 0 {
		return
	}
	now := time.Now().UnixNano()
	deadline := int64(maxIdle)

	// Approximate cleanup:
	// Drain up to MaxWorkersCount items from readyQ, stop stale, requeue fresh.
	// This is lock-free, but order is not strictly LRU anymore.
	for i := 0; i < wp.MaxWorkersCount; i++ {
		idx, ok := wp.readyQ.Dequeue()
		if !ok {
			return
		}
		last := wp.workers[idx].lastUse.Load()
		if last != 0 && (now-last) > deadline {
			// stop stale idle worker
			wp.workers[idx].ch <- nil
			continue
		}
		// still fresh -> put back to readyQ
		var failed int
		for !wp.readyQ.Enqueue(idx) {
			failed++
			runtime.Gosched()
		}
		if failed > 0 {
			wp.freeQFailed.Add(uint64(failed))
		}

	}
}

func (wp *WorkerPool) getMaxIdleWorkerDuration() time.Duration {
	if wp.MaxIdleWorkerDuration <= 0 {
		return 10 * time.Second
	}
	return wp.MaxIdleWorkerDuration
}
