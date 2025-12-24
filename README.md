# workerpool

A high-performance Go worker pool designed for very low overhead under high QPS.
It uses **lock-free bounded queues** to manage worker availability and creates worker goroutines **lazily** up to a fixed limit.

This project is intentionally small and “mechanical”: it aims to be fast, predictable, and easy to reason about in production.

---

## Key features

- **Bounded concurrency** via `MaxWorkersCount` (must be a power of two).
- **Lock-free worker registry** (no global mutex on the hot path) using two MPMC ring buffers:
    - `readyQ`: indices of **idle** workers
    - `freeQ`: indices available to **create** new workers lazily
- **Per-worker task channel** (reduced contention vs a single shared channel).
- Two APIs:
    - `Serve(cb)` – fire-and-forget scheduling
    - `Do(cb, deadline)` – schedule and wait for completion with a deadline
- **Idle worker cleanup**: periodically stops workers that have been idle longer than `MaxIdleWorkerDuration`.
- Basic **metrics** (`Stats()`) for observability.

---

## How it works

### Data structures

Each worker is represented by a `workerSlot`:

- `ch chan *workTask` – a dedicated channel for tasks for this worker.
- `lastUse atomic.Int64` – unix nano timestamp used by the idle-cleaner.

The pool also maintains:

- `readyQ` – bounded lock-free MPMC queue holding indices of **idle** workers.
- `freeQ` – bounded lock-free MPMC queue holding indices that may be used to **spawn** new workers.
- `workersCount` – atomic counter of currently running workers.

The ring buffers come from `github.com/aradilov/ringbuffer` and are used to avoid mutex contention under load.

### Scheduling: `getSlot()`

When a task is submitted:

1. **Fast path:** try to dequeue an idle worker index from `readyQ`.
2. If none are idle, try to dequeue an index from `freeQ` and **spawn a new worker** (bounded by `MaxWorkersCount`).
3. If both queues are empty, the pool is saturated and you get `ErrNoFreeWorkers`.

### Worker loop

A worker goroutine reads from its own channel:

- `nil` task is treated as a stop signal.
- A normal task:
    - transitions through internal statuses (`Queued` → `Progress` → `Ready`)
    - executes `WorkerFunc(cb)` (default calls `cb()` and returns `nil`)
    - either:
        - releases the task back to the internal pool (fire-and-forget), or
        - signals completion to the waiting `Do()` via `task.done` (wait-task)

After finishing a task, the worker tries to re-register itself as idle by enqueuing its index into `readyQ`.
If the pool is stopping, the worker exits instead of re-registering.

### Task states (current code)

`workTask.status` is an `atomic.Uint64` with the following meanings:

- `StatusQueued` – task has been queued to a worker
- `StatusProgress` – worker started executing it
- `StatusCanceled` – `Do()` timed out and requested cancellation
- `StatusDone` – worker finished executing it
- `StatusFree` – reusable (stored back into `sync.Pool`)

> Note: The code uses non-blocking send to `task.done` (buffer=1) for wait-tasks, and uses status transitions to decide who releases the task back to the pool.

---

## Public API

### Create and start a pool

```go
wp := &workerpool.WorkerPool{
    MaxWorkersCount:       1024,              // must be power of 2
    MaxIdleWorkerDuration: 30 * time.Second,  // idle worker TTL
    LogAllErrors:          true,              // log WorkerFunc errors
    WorkerFunc: func(cb func()) error {
        cb()
        return nil
    },
}
wp.Start()
defer wp.Stop()
```

There is also a convenience constructor:

```go
wp := workerpool.New() // creates defaults + Start()
defer wp.Stop()
```

### Fire-and-forget

```go
ok := wp.Serve(func() {
    // do work
})
if !ok {
    // pool is saturated (no free workers)
}
```

### Wait for completion with deadline

```go
err := wp.Do(func() {
    // do work
}, 250*time.Millisecond)

switch err {
case nil:
    // completed
case workerpool.ErrTimeout:
    // deadline expired (cancellation requested)
case workerpool.ErrNoFreeWorkers:
    // pool saturated at submit time
default:
    // WorkerFunc returned an error (if you use a non-default WorkerFunc)
}
```

---

## Metrics / Stats

```go
s := wp.Stats()

// s.Submitted     - how many tasks were submitted
// s.WorkersCount  - current number of running workers
// s.NoFreeWorkers - saturation counter
// s.ReleaseFailed - failures to re-enqueue to readyQ (should be near-zero)
// s.FreeQFailed   - failures to enqueue to freeQ on worker exit (should be near-zero)
```

---

## Tuning notes

### `workerChanCap`

The worker channel capacity is chosen automatically:

- `GOMAXPROCS == 1` → capacity `0` (unbuffered)
- otherwise → capacity `1`

This small buffer can reduce scheduling stalls without increasing memory footprint much.

### Cleanup cadence

The pool runs a ticker that calls `clean()` every `MaxIdleWorkerDuration` (or the default if unset).
`clean()` drains up to `MaxWorkersCount` indices from `readyQ` and stops workers whose idle time exceeds the configured duration.

Because this is lock-free, it is an **approximate** LRU: it is good enough for keeping the pool from growing “forever”.

---

## Stop semantics

`Stop()`:

- sets an internal `mustStop` flag
- closes `stopCh` (stops the cleaner goroutine)
- drains `readyQ` and sends `nil` to stop **idle** workers
- busy workers stop after completing their current task (they don’t re-register as idle)

**Important:** the pool is **not** designed to be restarted after `Stop()`.
Do not call `Serve/Do` after stopping the pool.

---

## Error handling and panics

The default `WorkerFunc` just calls `cb()` and returns `nil`.
If you want:

- panic protection (`recover`)
- structured error handling
- tracing / metrics around each job

wrap them in a custom `WorkerFunc`.

Example with panic protection:

```go
wp.WorkerFunc = func(cb func()) (err error) {
    defer func() {
        if r := recover(); r != nil {
            err = fmt.Errorf("panic: %v", r)
        }
    }()
    cb()
    return nil
}
```

---

## Limitations / known trade-offs

- `Do()` uses a status machine + a completion channel to coordinate timeout/cancel.
  This design aims to keep the hot path minimal, but it is more complex than a naive “context + select”.
- Cleanup and idle ordering are approximate (not strict LRU).
- The pool is intentionally minimal: no priorities, no per-task context, no retries, etc.

---

## License

MIT (or your preferred license).
