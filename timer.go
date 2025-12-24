package workerpool

import (
	"sync"
	"time"
)

// timerPool reuses *time.Timer safely.
// Core rule: before Reset or Put, always Stop() and drain C if needed.
type timerPool struct {
	p sync.Pool // stores *time.Timer
}

func (tp *timerPool) Get(d time.Duration) *time.Timer {
	if d <= 0 {
		// For non-positive timeouts, caller should usually short-circuit.
		// But keep behavior consistent: create a timer that fires immediately.
		d = 0
	}

	if v := tp.p.Get(); v != nil {
		t := v.(*time.Timer)
		// Extra safety: ensure it's stopped & drained even if Put() was misused somewhere.
		stopAndDrainTimer(t)
		t.Reset(d)
		return t
	}
	return time.NewTimer(d)
}

func (tp *timerPool) Put(t *time.Timer) {
	if t == nil {
		return
	}
	stopAndDrainTimer(t)
	tp.p.Put(t)
}

// stopAndDrainTimer makes timer safe for reuse.
//   - If Stop() returns false, the timer has already fired (or is firing),
//     so C might contain a value. Drain it non-blocking.
func stopAndDrainTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
