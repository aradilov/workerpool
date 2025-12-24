package workerpool

import (
	"sync"
	"sync/atomic"
)

const (
	StatusFree     = 0
	StatusQueued   = 1
	StatusProgress = 2
	StatusDone     = 3
	StatusReleased = 4
)

// workTask represents a unit of work, encapsulating a callback function, a wait flag, and a completion notification channel.
type workTask struct {
	cb     func()
	wait   bool
	done   chan error
	status atomic.Uint64
}

var workPool sync.Pool

func acquireTask() *workTask {
	if task := workPool.Get(); nil != task {
		return task.(*workTask)
	}
	return &workTask{
		done: make(chan error, 1),
	}
}

func releaseTask(task *workTask) {
	select {
	case <-task.done:
	default:
		break
	}
	task.wait = false
	task.cb = nil
	workPool.Put(task)
}
