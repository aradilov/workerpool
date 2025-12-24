package workerpool

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestPool(t *testing.T, maxWorkers int, wf func(cb func()) error) *WorkerPool {
	t.Helper()

	wp := &WorkerPool{
		MaxWorkersCount:       maxWorkers,
		MaxIdleWorkerDuration: time.Second,
		LogAllErrors:          false,
		WorkerFunc:            wf,
	}
	wp.Start()
	t.Cleanup(func() {
		defer func() { _ = recover() }()
		wp.Stop()
	})
	return wp
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		runtime.Gosched()
		time.Sleep(1 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestStart_PanicsIfMaxWorkersNotPowerOfTwo(t *testing.T) {
	wp := &WorkerPool{
		MaxWorkersCount: 3,
		WorkerFunc: func(cb func()) error {
			cb()
			return nil
		},
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for MaxWorkersCount not power of two")
		}
	}()
	wp.Start()
}

func TestServe_ExecutesCallback(t *testing.T) {
	var ran atomic.Bool

	wp := newTestPool(t, 2, func(cb func()) error {
		cb()
		return nil
	})

	ok := wp.Serve(func() {
		ran.Store(true)
	})
	if !ok {
		t.Fatalf("Serve returned false unexpectedly")
	}

	waitUntil(t, 500*time.Millisecond, func() bool { return ran.Load() })
}

func TestDo_ReturnsNilOnSuccess(t *testing.T) {
	wp := newTestPool(t, 2, func(cb func()) error {
		cb()
		return nil
	})

	var ran atomic.Bool
	err := wp.Do(func() { ran.Store(true) }, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if !ran.Load() {
		t.Fatalf("callback was not executed")
	}
}

func TestDo_PropagatesWorkerError(t *testing.T) {
	want := errors.New("boom")

	wp := newTestPool(t, 2, func(cb func()) error {
		cb()
		return want
	})

	err := wp.Do(func() {}, 500*time.Millisecond)
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

func TestNoFreeWorkers_WhenMaxReached(t *testing.T) {

	block := make(chan struct{})
	started := make(chan struct{})

	wp := newTestPool(t, 1, func(cb func()) error {
		close(started)
		<-block
		cb()
		return nil
	})

	ok := wp.Serve(func() {})
	if !ok {
		t.Fatalf("first Serve should succeed")
	}
	<-started

	ok2 := wp.Serve(func() {})
	if ok2 {
		t.Fatalf("second Serve must fail (no free workers)")
	}

	st := wp.Stats()
	if st.NoFreeWorkers == 0 {
		t.Fatalf("expected NoFreeWorkers > 0, got %+v", st)
	}

	close(block)
}

func TestDo_TimeoutDoesNotBreakPool_AndTaskIsEventuallyReleased(t *testing.T) {
	delay := time.Second
	wp := newTestPool(t, 1, func(cb func()) error {
		time.Sleep(delay)
		cb()
		return nil
	})

	if err := wp.Do(func() {}, 10*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}

	var ran atomic.Bool
	if err := wp.Do(func() {}, 10*time.Millisecond); !errors.Is(err, ErrNoFreeWorkers) {
		t.Fatalf("expected ErrNoFreeWorkers, got %v", err)
	}

	time.Sleep(delay)
	if err := wp.Do(func() { ran.Store(true) }, delay*2); nil != err {
		t.Fatalf("expected nil, got %v", err)
	}

	if !ran.Load() {
		t.Fatalf("second callback did not run")
	}
}

func TestStop_StopsIdleWorkers(t *testing.T) {
	wp := newTestPool(t, 2, func(cb func()) error {
		cb()
		return nil
	})

	done := make(chan struct{})
	ok := wp.Serve(func() { close(done) })
	if !ok {
		t.Fatalf("Serve failed unexpectedly")
	}
	<-done

	waitUntil(t, 500*time.Millisecond, func() bool { return wp.Stats().WorkersCount == 1 })

	wp.Stop()

	waitUntil(t, 1*time.Second, func() bool { return wp.Stats().WorkersCount == 0 })
}

func TestClean_StopsStaleIdleWorkers(t *testing.T) {
	wp := &WorkerPool{
		MaxWorkersCount:       2,
		MaxIdleWorkerDuration: 20 * time.Millisecond,
		LogAllErrors:          false,
		WorkerFunc: func(cb func()) error {
			cb()
			return nil
		},
	}
	wp.Start()
	t.Cleanup(func() {
		defer func() { _ = recover() }()
		wp.Stop()
	})

	done := make(chan struct{})
	if !wp.Serve(func() { close(done) }) {
		t.Fatalf("Serve failed unexpectedly")
	}
	<-done

	waitUntil(t, 500*time.Millisecond, func() bool { return wp.Stats().WorkersCount == 1 })

	idx, ok := wp.readyQ.Dequeue()
	if !ok {
		t.Fatalf("expected an idle worker in readyQ")
	}
	wp.workers[idx].lastUse.Store(time.Now().Add(-10 * wp.MaxIdleWorkerDuration).UnixNano())
	for !wp.readyQ.Enqueue(idx) {
		runtime.Gosched()
	}

	wp.clean()

	waitUntil(t, 1*time.Second, func() bool { return wp.Stats().WorkersCount == 0 })
}

func TestStats_SubmittedIncrements(t *testing.T) {
	wp := newTestPool(t, 2, func(cb func()) error { cb(); return nil })

	var wg sync.WaitGroup
	wg.Add(2)

	if !wp.Serve(func() { wg.Done() }) {
		t.Fatalf("Serve failed unexpectedly")
	}
	go func() {
		defer wg.Done()
		_ = wp.Do(func() {}, 500*time.Millisecond)
	}()

	wg.Wait()

	st := wp.Stats()
	if st.Submitted < 2 {
		t.Fatalf("expected Submitted>=2, got %+v", st)
	}
}
