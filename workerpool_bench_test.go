package workerpool

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func nextPow2(n int) int {

	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16

	n++
	return n
}

func newBenchPool(maxWorkers int, wf func(cb func()) error) *WorkerPool {
	wp := &WorkerPool{
		MaxWorkersCount:       maxWorkers,
		MaxIdleWorkerDuration: time.Second,
		LogAllErrors:          false,
		WorkerFunc:            wf,
	}
	wp.Start()
	return wp
}

func BenchmarkServe_NoWait_Parallel(b *testing.B) {
	max := nextPow2(runtime.GOMAXPROCS(0) * 2)

	wp := newBenchPool(max, func(cb func()) error {
		cb()
		return nil
	})
	defer wp.Stop()

	_ = wp.Serve(func() {})

	var sink atomic.Uint64
	cb := func() { sink.Add(1) }

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = wp.Serve(cb)
		}
	})
	_ = sink.Load()
}

func BenchmarkServe_NoWait_Serial(b *testing.B) {
	max := nextPow2(runtime.GOMAXPROCS(0) * 2)

	wp := newBenchPool(max, func(cb func()) error {
		cb()
		return nil
	})
	defer wp.Stop()

	var sink atomic.Uint64
	cb := func() { sink.Add(1) }

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wp.Serve(cb)
	}
	_ = sink.Load()
}

func BenchmarkDo_Wait_Parallel(b *testing.B) {
	max := nextPow2(runtime.GOMAXPROCS(0) * 2)

	wp := newBenchPool(max, func(cb func()) error {
		cb()
		return nil
	})
	defer wp.Stop()

	var sink, noFreeWorkers, timeout atomic.Uint64
	cb := func() { sink.Add(1) }

	deadline := 2 * time.Second

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			switch wp.Do(cb, deadline) {
			case ErrNoFreeWorkers:
				noFreeWorkers.Add(1)
			case ErrTimeout:
				timeout.Add(1)
			}
		}
	})
	_ = sink.Load()
	//res := sink.Load()
	//b.Logf("sink.Load()=%d for %d, noFreeWorkers %d, timeout = %d", res, b.N, noFreeWorkers.Load(), timeout.Load())
}

func BenchmarkDo_Wait_Serial(b *testing.B) {
	max := nextPow2(runtime.GOMAXPROCS(0) * 2)

	wp := newBenchPool(max, func(cb func()) error {
		cb()
		return nil
	})
	defer wp.Stop()

	var sink atomic.Uint64
	cb := func() { sink.Add(1) }

	deadline := 2 * time.Second

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wp.Do(cb, deadline)
	}
	_ = sink.Load()
}

func BenchmarkMixed_90Serve_10Do_Parallel(b *testing.B) {
	max := nextPow2(runtime.GOMAXPROCS(0) * 2)

	wp := newBenchPool(max, func(cb func()) error {
		cb()
		return nil
	})
	defer wp.Stop()

	var sink atomic.Uint64
	cb := func() { sink.Add(1) }

	deadline := 2 * time.Second

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := 0
		for pb.Next() {
			local++
			if local%10 == 0 {
				_ = wp.Do(cb, deadline)
			} else {
				_ = wp.Serve(cb)
			}
		}
	})
	_ = sink.Load()
}
