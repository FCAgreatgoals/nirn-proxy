package lib

import (
	"context"
	"sync/atomic"
	"time"
)

// A global 429 means every request made with this token will be refused until
// Retry-After has passed, whatever its route. Upstream recorded the deadline in
// globalLockedUntil and never read it back, so requests kept flowing straight
// into the global limit, each one another invalid request counted towards the
// 10,000 that get the IP banned.

// lockGlobal extends the global lock to until. It never shortens a lock: two
// global 429s racing each other must leave the later deadline in place.
//
// Upstream wrote the lock with a compare-and-swap from zero and nothing ever
// reset it, so only the first global 429 of a queue's lifetime would have
// counted even once the lock was read.
func lockGlobal(lockedUntil *int64, until time.Time) bool {
	deadline := until.UnixNano()
	for {
		current := atomic.LoadInt64(lockedUntil)
		if deadline <= current {
			return false
		}
		if atomic.CompareAndSwapInt64(lockedUntil, current, deadline) {
			return true
		}
	}
}

// waitGlobal holds a request while a global lock is in force, or until the
// client gives up.
func waitGlobal(ctx context.Context, lockedUntil *int64) error {
	wait := time.Until(time.Unix(0, atomic.LoadInt64(lockedUntil)))
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
