package lib

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
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

// maxWaitHeader caps, in seconds, how long a request may be held by its
// reserve, below MAX_RESERVE_HOLD: a client that gives up on a request after
// a minute asks for its 429 before then, rather than finding nothing.
const maxWaitHeader = "X-RateLimit-Reserve-Max-Wait"

// noReserve marks ordinary traffic.
const noReserve = -1

var errBadReserve = errors.New(reserveHeader + " must be an integer from 0 to 100")

var errBadMaxWait = errors.New(maxWaitHeader + " must be a number of seconds above 0")

// maxReserveHold is the longest a request may be held back. One that would
// wait longer is answered at once with a 429 saying when its bucket is whole
// again, for the client to retry then, rather than holding its connection
// open for hours. MAX_RESERVE_HOLD sets it.
var maxReserveHold = 6 * time.Hour

// SetMaxReserveHold changes how long a request may be held back.
func SetMaxReserveHold(d time.Duration) {
	maxReserveHold = d
}

// MaxReserveHold is how long a request may be held back.
func MaxReserveHold() time.Duration {
	return maxReserveHold
}

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

// parseMaxWait reads how long a request may be held by its reserve: what it
// asks for, never more than MAX_RESERVE_HOLD.
func parseMaxWait(h http.Header) (time.Duration, error) {
	raw := strings.TrimSpace(h.Get(maxWaitHeader))
	if raw == "" {
		return maxReserveHold, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || !(seconds > 0) || math.IsInf(seconds, 0) {
		return 0, errBadMaxWait
	}
	return min(time.Duration(seconds*float64(time.Second)), maxReserveHold), nil
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
// then held back requests once the bucket leaves them room. Held requests wait
// aside, so the ordinary ones behind them are never stuck in their wake, and
// the lowest reserve goes first among them, the oldest first for equal ones:
// a request that leaves less to others is the more urgent. Every held request
// is watched, not only the first: each is answered its 429 as soon as its
// bucket is known to stay short past its max wait, and leaves when its client
// gives up. It returns false once the queue is closed.
func (ch *QueueChannel) next(state *bucketState) (*QueueItem, bool) {
	for {
		select {
		case item, ok := <-ch.ch:
			return item, ok
		default:
		}
		ch.collect()

		now := time.Now()
		ch.settle(state, now)
		if len(ch.held) == 0 {
			select {
			case item, ok := <-ch.ch:
				return item, ok
			case item, ok := <-ch.bulk:
				ch.hold(item, ok)
			}
			continue
		}

		// Whether a request may go only grows as its reserve shrinks: if
		// the lowest one cannot go, none can.
		first := ch.held[0]
		wait := state.reserveWait(first.reserve, now)
		if wait <= 0 {
			ch.release(0)
			return first, true
		}
		for _, item := range ch.held {
			if !item.counted {
				item.counted = true
				ReserveHeld.WithLabelValues("bucket").Inc()
			}
		}
		timer := time.NewTimer(wait)
		select {
		case item, ok := <-ch.ch:
			timer.Stop()
			return item, ok
		case item, ok := <-ch.bulk:
			timer.Stop()
			ch.hold(item, ok)
		case <-ch.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// collect moves the requests waiting in the bulk lane among the held ones.
func (ch *QueueChannel) collect() {
	for {
		select {
		case item, ok := <-ch.bulk:
			if !ch.hold(item, ok) {
				return
			}
		default:
			return
		}
	}
}

// hold takes a request from the bulk lane, and says whether there was one.
// The held requests stay sorted by reserve, then by arrival.
func (ch *QueueChannel) hold(item *QueueItem, ok bool) bool {
	if !ok {
		// Closed with the queue: nothing more will come.
		ch.bulk = nil
		return false
	}
	wake := ch.wake
	item.unwatch = context.AfterFunc(item.Req.Context(), func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	i := sort.Search(len(ch.held), func(i int) bool { return ch.held[i].reserve > item.reserve })
	ch.held = append(ch.held, nil)
	copy(ch.held[i+1:], ch.held[i:])
	ch.held[i] = item
	return true
}

// release takes the i-th held request out.
func (ch *QueueChannel) release(i int) *QueueItem {
	item := ch.held[i]
	item.unwatch()
	ch.held = append(ch.held[:i], ch.held[i+1:]...)
	return item
}

// settle answers the held requests that must not wait any longer: those whose
// client gave up, and those whose bucket stays short past their max wait.
func (ch *QueueChannel) settle(state *bucketState, now time.Time) {
	for i := 0; i < len(ch.held); {
		item := ch.held[i]
		if err := item.Req.Context().Err(); err != nil {
			ch.release(i).errChan <- err
			continue
		}
		if wait := state.reserveWait(item.reserve, now); wait > 0 && now.Add(wait).Sub(item.queued) > item.maxWait {
			refuseHeld(ch.release(i), wait)
			continue
		}
		i++
	}
}

// globalWindow is a bot's global limit: at most capacity requests in any
// window. Discord counts a window from a request's arrival, so a window the
// proxy opened on its own clock lets the end of one and the start of the next
// reach Discord inside a single second. The window slides instead: a request
// goes only if fewer than capacity went out in the window before it, whenever
// Discord's own window began. It replaces leakybucket for the global limit,
// whose Add cannot refuse a request on what would remain after it.
type globalWindow struct {
	mu       sync.Mutex
	capacity int64
	window   time.Duration
	// sent is a ring of the times the last requests went out, oldest at head.
	sent []time.Time
	head int
	n    int
}

func newGlobalWindow(capacity uint, window time.Duration) *globalWindow {
	if capacity < 1 {
		capacity = 1
	}
	return &globalWindow{capacity: int64(capacity), window: window, sent: make([]time.Time, capacity)}
}

// at is the i-th oldest request still in the window.
func (g *globalWindow) at(i int) time.Time {
	return g.sent[(g.head+i)%len(g.sent)]
}

// take spends one request of the window if the reserve allows it, and
// otherwise says how long to wait before trying again.
func (g *globalWindow) take(reserve int, now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.n > 0 && !g.at(0).After(now.Add(-g.window)) {
		g.head = (g.head + 1) % len(g.sent)
		g.n--
	}
	if remaining := g.capacity - int64(g.n); remaining > 0 && admits(g.capacity, remaining, reserve) {
		g.sent[(g.head+g.n)%len(g.sent)] = now
		g.n++
		return true, 0
	}
	// Wait for enough of the oldest to leave the window: down to one free
	// for ordinary traffic, to the reserve's share and one for held back
	// requests, or to an empty window when the share leaves nothing.
	keep := g.capacity - 1
	if reserve != noReserve {
		keep -= reserved(g.capacity, reserve)
	}
	if keep < 0 {
		keep = 0
	}
	leave := g.n - int(keep)
	return false, g.at(leave - 1).Add(g.window).Sub(now)
}

// refuseHeld answers a request that would be held past maxReserveHold with
// a 429 telling its client when the bucket is whole again.
func refuseHeld(item *QueueItem, wait time.Duration) {
	ReserveRefused.Inc()
	writeProxy429(item.Res, wait, "Held by "+reserveHeader+" for longer than its max wait.")
	item.errChan <- fmt.Errorf("would be held by its reserve %v more, past its %v max wait, sending 429 for client to retry", wait.Round(time.Millisecond), item.maxWait)
}

// dropHeld answers the requests still held back once the queue is closed, so
// that none of their clients waits forever.
func (ch *QueueChannel) dropHeld() {
	drop := func(item *QueueItem) {
		Generate429(item.Res)
		item.errChan <- errors.New("queue closed while the request was held by its reserve, sending 429 for client to retry")
	}
	for len(ch.held) > 0 {
		drop(ch.release(0))
	}
	if ch.bulk != nil {
		for item := range ch.bulk {
			drop(item)
		}
	}
}
