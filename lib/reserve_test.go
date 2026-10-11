package lib

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestParseReserve(t *testing.T) {
	for raw, want := range map[string]int{"": noReserve, "0": noReserve, " 30 ": 30, "100": 100} {
		h := http.Header{}
		h.Set(reserveHeader, raw)
		if got, err := parseReserve(h); err != nil || got != want {
			t.Errorf("%q = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"-1", "101", "30%", "0.5", "abc"} {
		h := http.Header{}
		h.Set(reserveHeader, raw)
		if _, err := parseReserve(h); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

// What a reserve keeps back, on the small buckets most routes have and on the
// global limit.
func TestAdmits(t *testing.T) {
	cases := []struct {
		capacity, remaining int64
		reserve             int
		want                bool
	}{
		{5, 3, 30, true},    // two left after it: 40 % of five, over the 30 % kept
		{5, 2, 30, false},   // one left would be under 30 %
		{5, 4, 70, false},   // 70 % of five keeps four, rounded up: only a full bucket
		{5, 5, 70, true},    //
		{5, 5, 100, true},   // a full bucket always admits
		{1, 1, 30, true},    // a one request bucket: only when full
		{1, 0, 30, false},   //
		{50, 16, 30, true},  // global: fifteen kept for the bot's own traffic
		{50, 15, 30, false}, //
		{50, 1, noReserve, true},
	}
	for _, c := range cases {
		if got := admits(c.capacity, c.remaining, c.reserve); got != c.want {
			t.Errorf("admits(%d, %d, %d) = %v", c.capacity, c.remaining, c.reserve, got)
		}
	}
}

// sendReserved sends like send, with a reserve, and returns once answered.
func (q *RequestQueue) sendReserved(t *testing.T, method, path, reserve string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bot test")
	if reserve != "" {
		req.Header.Set(reserveHeader, reserve)
	}
	routingHash, bucket, _ := (&QueueManager{}).GetRequestRoutingInfo(req, "Bot test")
	var res http.ResponseWriter = httptest.NewRecorder()
	if err := q.Queue(req, &res, bucket, routingHash); err != nil {
		t.Errorf("%s %s: %v", method, path, err)
	}
}

// A bucket answering with this many left out of five, back in full after reset.
func bucketOfFive(remaining *int, reset string) func(*http.Request, []byte) (int, http.Header, string) {
	var mu sync.Mutex
	return func(*http.Request, []byte) (int, http.Header, string) {
		mu.Lock()
		defer mu.Unlock()
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "5")
		h.Set("X-RateLimit-Remaining", itoa(*remaining))
		h.Set("X-RateLimit-Reset-After", reset)
		if *remaining > 0 {
			*remaining--
		}
		return 200, h, "{}"
	}
}

func itoa(n int) string { return string(rune('0' + n)) }

const reserveRoute = "/api/v10/guilds/203039963636301824/members/203039963636301825"

// A held back request waits aside: the ordinary request queued after it
// reaches Discord first, and the held one goes once the bucket is whole again.
func TestHeldRequestLetsOrdinaryTrafficPass(t *testing.T) {
	remaining := 2
	fake := &fakeDiscord{answer: bucketOfFive(&remaining, "0.400")}
	q := newTestQueue(fake)

	q.send(t, "GET", reserveRoute, "") // answered with 2 left of 5
	start := time.Now()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.sendReserved(t, "GET", reserveRoute, "30")
	}()
	time.Sleep(50 * time.Millisecond) // the held request is queued first
	q.send(t, "GET", reserveRoute, "")
	ordinary := time.Since(start)
	wg.Wait()

	if fake.count() != 3 {
		t.Fatalf("%d requests reached Discord, want 3", fake.count())
	}
	if ordinary > 200*time.Millisecond {
		t.Errorf("the ordinary request waited %v behind the held one", ordinary)
	}
	if held := fake.call(2).at.Sub(start); held < 350*time.Millisecond {
		t.Errorf("the held request went after %v, before the bucket was whole again", held)
	}
}

// Before any answer the bucket's size is unknown: the first request goes, held
// back or not, and teaches the proxy.
func TestReserveOnUnknownBucketGoesAtOnce(t *testing.T) {
	remaining := 4
	fake := &fakeDiscord{answer: bucketOfFive(&remaining, "5")}
	q := newTestQueue(fake)
	start := time.Now()
	q.sendReserved(t, "GET", reserveRoute, "70")
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("first request held %v", d)
	}
}

// A client that gives up while its request is held back is answered at once,
// and the queue goes on.
func TestHeldRequestLeavesWhenItsClientGivesUp(t *testing.T) {
	remaining := 1
	fake := &fakeDiscord{answer: bucketOfFive(&remaining, "10")}
	q := newTestQueue(fake)
	q.send(t, "GET", reserveRoute, "") // 1 left of 5, whole again in 10 s

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", reserveRoute, nil).WithContext(ctx)
	req.Header.Set(reserveHeader, "30")
	routingHash, bucket, _ := (&QueueManager{}).GetRequestRoutingInfo(req, "Bot test")
	var res http.ResponseWriter = httptest.NewRecorder()
	start := time.Now()
	if err := q.Queue(req, &res, bucket, routingHash); err == nil {
		t.Fatal("held request answered by Discord")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("gave up after %v", d)
	}
	if fake.count() != 1 {
		t.Errorf("%d requests reached Discord", fake.count())
	}
}

// The global limit keeps its share back from reserved requests, never from
// ordinary ones.
func TestGlobalWindowKeepsTheReserve(t *testing.T) {
	g := newGlobalWindow(10, time.Hour)
	now := time.Now()
	bulk := 0
	for {
		ok, _ := g.take(30, now)
		if !ok {
			break
		}
		bulk++
	}
	ordinary := 0
	for {
		ok, _ := g.take(noReserve, now)
		if !ok {
			break
		}
		ordinary++
	}
	if bulk != 7 || ordinary != 3 {
		t.Errorf("reserved took %d, ordinary %d; want 7 and 3", bulk, ordinary)
	}
	if ok, wait := g.take(noReserve, now); ok || wait <= 0 {
		t.Errorf("empty window: ok %v, wait %v", ok, wait)
	}
	if ok, _ := g.take(30, now.Add(2*time.Hour)); !ok {
		t.Error("a new window refused a reserved request")
	}
}

// Discord never sees the header.
func TestReserveHeaderNeverReachesDiscord(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(204)
	}))
	defer srv.Close()
	pointAt(t, srv)

	req := httptest.NewRequest("GET", "/api/v10/users/@me", nil)
	req.Header.Set(reserveHeader, "30")
	var res http.ResponseWriter = httptest.NewRecorder()
	if _, err := ProcessRequest(context.Background(), &QueueItem{Req: req, Res: &res}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Get(reserveHeader) != "" {
		t.Errorf("Discord received %q", got.Get(reserveHeader))
	}
}

// A malformed reserve is refused before anything is queued.
func TestMalformedReserveIsRefused(t *testing.T) {
	m := NewQueueManager(16, 16)
	req := httptest.NewRequest("GET", "/api/v10/users/@me", nil)
	req.Header.Set("Authorization", "Bot test")
	req.Header.Set(reserveHeader, "high")
	rec := httptest.NewRecorder()
	m.DiscordRequestHandler(rec, req)
	if rec.Code != 400 {
		t.Errorf("status %d, want 400", rec.Code)
	}
}
