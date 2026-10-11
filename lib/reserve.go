package lib

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A client running background work (mass bans, role syncs) marks each of its
// requests with X-RateLimit-Reserve: the share of capacity, in percent, to
// leave to the bot's other traffic. Such a request only goes out while more
// than that share remains, both on the bot's global limit and on its route's
// bucket; otherwise it waits. The client picks the share per request, the
// proxy has no level of its own. A request without the header behaves as it
// always has, and the header can only hold a request back, never move it ahead.

const reserveHeader = "X-RateLimit-Reserve"

// noReserve marks ordinary traffic.
const noReserve = -1

var errBadReserve = errors.New(reserveHeader + " must be an integer from 0 to 100")

// parseReserve reads the reserve a request asks for. Zero is ordinary traffic.
func parseReserve(h http.Header) (int, error) {
	raw := strings.TrimSpace(h.Get(reserveHeader))
	if raw == "" {
		return noReserve, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 100 {
		return noReserve, errBadReserve
	}
	if n == 0 {
		return noReserve, nil
	}
	return n, nil
}

// reserved is how many of capacity a reserve keeps back, never less than one:
// a share that rounds to nothing would reserve nothing at all.
func reserved(capacity int64, reserve int) int64 {
	return int64(math.Ceil(float64(capacity) * float64(reserve) / 100))
}

// admits says whether a request with this reserve may take one of remaining
// out of capacity. A full bucket always admits, so that background work goes
// through whenever the other traffic is quiet, even on a bucket too small to
// keep its share back.
func admits(capacity, remaining int64, reserve int) bool {
	if reserve == noReserve || remaining >= capacity {
		return true
	}
	return remaining-1 >= reserved(capacity, reserve)
}

// bucketState is what the last answer of a bucket said about it.
type bucketState struct {
	known     bool
	limit     int64
	remaining int64
	full      time.Time // when the bucket is whole again
}

func (s *bucketState) observe(limit, remaining int64, resetAfter time.Duration, now time.Time) {
	if limit <= 0 {
		// No rate limit headers: nothing learned about the bucket.
		return
	}
	s.known, s.limit, s.remaining, s.full = true, limit, remaining, now.Add(resetAfter)
}

// reserveWait is how long a request with this reserve must wait before the
// bucket admits it. A bucket never answered yet admits at once: the first
// request is what teaches the proxy its size. Past its reset a bucket is taken
// as whole again; Discord's reset is when it is full, for token buckets too.
func (s *bucketState) reserveWait(reserve int, now time.Time) time.Duration {
	if reserve == noReserve || !s.known || !now.Before(s.full) {
		return 0
	}
	if admits(s.limit, s.remaining, reserve) {
		return 0
	}
	return s.full.Sub(now)
}

// next hands the bucket's goroutine its next request: ordinary traffic first,
// then held back requests once the bucket leaves them room. A held request
// waits aside, so the ordinary ones behind it are never stuck in its wake.
// It returns false once the queue is closed.
func (ch *QueueChannel) next(state *bucketState) (*QueueItem, bool) {
	for {
		select {
		case item, ok := <-ch.ch:
			return item, ok
		default:
		}

		if ch.pending == nil {
			select {
			case item, ok := <-ch.ch:
				return item, ok
			case item, ok := <-ch.bulk:
				if !ok {
					return nil, false
				}
				ch.pending, ch.pendingHeld = item, false
			}
		}

		wait := state.reserveWait(ch.pending.reserve, time.Now())
		if wait <= 0 {
			item := ch.pending
			ch.pending = nil
			return item, true
		}
		if !ch.pendingHeld {
			ch.pendingHeld = true
			ReserveHeld.WithLabelValues("bucket").Inc()
		}
		timer := time.NewTimer(wait)
		select {
		case item, ok := <-ch.ch:
			timer.Stop()
			return item, ok
		case <-timer.C:
		case <-ch.pending.Req.Context().Done():
			timer.Stop()
			ch.pending.errChan <- ch.pending.Req.Context().Err()
			ch.pending = nil
		}
	}
}

// globalWindow is a bot's global limit: capacity requests per window, the
// whole of it back at each new window. It replaces leakybucket for the global
// limit, whose Add cannot refuse a request on what would remain after it.
type globalWindow struct {
	mu        sync.Mutex
	capacity  int64
	remaining int64
	reset     time.Time
	window    time.Duration
}

func newGlobalWindow(capacity uint, window time.Duration) *globalWindow {
	return &globalWindow{capacity: int64(capacity), remaining: int64(capacity), reset: time.Now().Add(window), window: window}
}

// take spends one request of the window if the reserve allows it, and
// otherwise says how long to wait before trying again.
func (g *globalWindow) take(reserve int, now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.After(g.reset) {
		g.reset = now.Add(g.window)
		g.remaining = g.capacity
	}
	if g.remaining > 0 && admits(g.capacity, g.remaining, reserve) {
		g.remaining--
		return true, 0
	}
	return false, g.reset.Sub(now)
}

// dropHeld answers the requests still held back once the queue is closed, so
// that none of their clients waits forever.
func (ch *QueueChannel) dropHeld() {
	drop := func(item *QueueItem) {
		Generate429(item.Res)
		item.errChan <- errors.New("queue closed while the request was held by its reserve, sending 429 for client to retry")
	}
	if ch.pending != nil {
		drop(ch.pending)
		ch.pending = nil
	}
	for item := range ch.bulk {
		drop(item)
	}
}
