package lib

import (
	"context"
	"encoding/json"
	"errors"
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
	req.Header.Set(maxWaitHeader, "60")
	var res http.ResponseWriter = httptest.NewRecorder()
	if _, err := ProcessRequest(context.Background(), &QueueItem{Req: req, Res: &res}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Get(reserveHeader) != "" || got.Get(maxWaitHeader) != "" {
		t.Errorf("Discord received %q and %q", got.Get(reserveHeader), got.Get(maxWaitHeader))
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

	req = httptest.NewRequest("GET", "/api/v10/users/@me", nil)
	req.Header.Set("Authorization", "Bot test")
	req.Header.Set(reserveHeader, "30")
	req.Header.Set(maxWaitHeader, "soon")
	rec = httptest.NewRecorder()
	m.DiscordRequestHandler(rec, req)
	if rec.Code != 400 {
		t.Errorf("max wait: status %d, want 400", rec.Code)
	}
}

// No window of Discord's, wherever it begins, sees more than the limit: a
// request leaves the proxy's window one second after it went, not when a
// window of the proxy's own clock closes.
func TestGlobalWindowSlides(t *testing.T) {
	g := newGlobalWindow(5, time.Second)
	t0 := time.Now()
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	if ok, _ := g.take(noReserve, at(0)); !ok {
		t.Fatal("first request refused")
	}
	for i := 0; i < 4; i++ {
		if ok, _ := g.take(noReserve, at(900)); !ok {
			t.Fatalf("request %d at 900 ms refused", i+2)
		}
	}
	// The first has left the window, the four at 900 ms have not.
	if ok, _ := g.take(noReserve, at(1050)); !ok {
		t.Fatal("refused once the first had left the window")
	}
	ok, wait := g.take(noReserve, at(1100))
	if ok {
		t.Fatal("six requests within one second")
	}
	if want := 800 * time.Millisecond; wait != want {
		t.Errorf("wait %v, want %v", wait, want)
	}
	if ok, _ := g.take(noReserve, at(1900)); !ok {
		t.Error("refused once the 900 ms requests had left the window")
	}
}

// A held back request waits for the reserve's share to come back, not merely
// for one request to leave.
func TestGlobalWindowReserveWait(t *testing.T) {
	g := newGlobalWindow(10, time.Second)
	t0 := time.Now()
	for i := 0; i < 10; i++ {
		g.take(noReserve, t0.Add(time.Duration(i)*100*time.Millisecond))
	}
	// Reserve 30 keeps three: four must leave, the fourth went at 300 ms.
	ok, wait := g.take(30, t0.Add(950*time.Millisecond))
	if ok || wait != 350*time.Millisecond {
		t.Errorf("ok %v, wait %v; want a 350 ms wait", ok, wait)
	}
}

// A request whose bucket is whole again only past the hold limit is answered
// at once, with a 429 saying when to come back, and never reaches Discord.
func TestReserveHeldPastTheLimitIsRefusedAtOnce(t *testing.T) {
	defer SetMaxReserveHold(MaxReserveHold())
	SetMaxReserveHold(5 * time.Second)

	remaining := 1
	fake := &fakeDiscord{answer: bucketOfFive(&remaining, "30")}
	q := newTestQueue(fake)
	q.send(t, "GET", reserveRoute, "") // 1 left of 5, whole again in 30 s

	req := httptest.NewRequest("GET", reserveRoute, nil)
	req.Header.Set(reserveHeader, "30")
	routingHash, bucket, _ := (&QueueManager{}).GetRequestRoutingInfo(req, "Bot test")
	rec := httptest.NewRecorder()
	var res http.ResponseWriter = rec
	start := time.Now()
	if err := q.Queue(req, &res, bucket, routingHash); err == nil {
		t.Fatal("no error for a refused request")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("refused after %v", d)
	}
	if rec.Code != 429 || rec.Header().Get("generated-by-proxy") != "true" {
		t.Errorf("status %d, generated-by-proxy %q", rec.Code, rec.Header().Get("generated-by-proxy"))
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After %q, want 30", got)
	}
	if fake.count() != 1 {
		t.Errorf("%d requests reached Discord", fake.count())
	}

	// Within the limit, the same wait holds the request instead.
	SetMaxReserveHold(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res = httptest.NewRecorder()
	held := httptest.NewRequest("GET", reserveRoute, nil).WithContext(ctx)
	held.Header.Set(reserveHeader, "30")
	if err := q.Queue(held, &res, bucket, routingHash); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("within the limit: %v, want the request held until its client gave up", err)
	}
}

// The proxy's own 429 asks for the wait it means, in each of the places a
// client may read it.
func TestProxy429CarriesItsWait(t *testing.T) {
	rec := httptest.NewRecorder()
	var res http.ResponseWriter = rec
	writeProxy429(&res, 2500*time.Millisecond, "held")
	if got := rec.Header().Get("X-RateLimit-Reset-After"); got != "2.500" {
		t.Errorf("Reset-After %q", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After %q, want it rounded up to 3", got)
	}
	var body struct {
		RetryAfter float64 `json:"retry_after"`
		Global     bool    `json:"global"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.RetryAfter != 2.5 || body.Global {
		t.Errorf("body %s (%v)", rec.Body.String(), err)
	}
}

func TestParseMaxWait(t *testing.T) {
	defer SetMaxReserveHold(MaxReserveHold())
	SetMaxReserveHold(time.Hour)
	for raw, want := range map[string]time.Duration{"": time.Hour, "60": time.Minute, " 0.5 ": 500 * time.Millisecond, "86400": time.Hour} {
		h := http.Header{}
		h.Set(maxWaitHeader, raw)
		if got, err := parseMaxWait(h); err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "-1", "abc", "NaN", "Inf", "60s"} {
		h := http.Header{}
		h.Set(maxWaitHeader, raw)
		if _, err := parseMaxWait(h); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

// queueHeld queues a request with a reserve and a max wait in the
// background; the returned channel gives what Queue returned, and the
// recorder what the client received.
func (q *RequestQueue) queueHeld(ctx context.Context, path, reserve, maxWait string) (chan error, *httptest.ResponseRecorder) {
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	req.Header.Set(reserveHeader, reserve)
	if maxWait != "" {
		req.Header.Set(maxWaitHeader, maxWait)
	}
	routingHash, bucket, _ := (&QueueManager{}).GetRequestRoutingInfo(req, "Bot test")
	rec := httptest.NewRecorder()
	var res http.ResponseWriter = rec
	done := make(chan error, 1)
	go func() { done <- q.Queue(req, &res, bucket, routingHash) }()
	return done, rec
}

// Among held requests the lowest reserve goes first, whatever the order they
// came in: a ban held at 30 does not wait behind a mass unban held at 70.
func TestLowestReserveGoesFirst(t *testing.T) {
	remaining := 1
	fake := &fakeDiscord{answer: bucketOfFive(&remaining, "0.300")}
	q := newTestQueue(fake)
	q.send(t, "GET", reserveRoute, "") // 1 left of 5: neither may go

	const unban, ban = reserveRoute + "1", reserveRoute + "2"
	ctx := context.Background()
	unbanDone, _ := q.queueHeld(ctx, unban, "70", "")
	time.Sleep(50 * time.Millisecond)
	banDone, _ := q.queueHeld(ctx, ban, "30", "")
	<-banDone
	<-unbanDone

	if fake.count() != 3 || fake.call(1).path != ban || fake.call(2).path != unban {
		t.Errorf("order reaching Discord: %v", fake.calls)
	}
}

// Every held request is watched for its max wait, not only the first: one
// queued behind a held request is answered at once when its bucket stays
// short past its max wait.
func TestMaxWaitWatchesEveryHeldRequest(t *testing.T) {
	remaining := 1
	fake := &fakeDiscord{answer: bucketOfFive(&remaining, "30")}
	q := newTestQueue(fake)
	q.send(t, "GET", reserveRoute, "") // 1 left of 5, whole again in 30 s

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone, _ := q.queueHeld(ctx, reserveRoute, "30", "")
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	shortDone, rec := q.queueHeld(context.Background(), reserveRoute, "70", "5")
	select {
	case <-shortDone:
	case <-time.After(time.Second):
		t.Fatal("the request with a 5 s max wait is still held")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("answered after %v", d)
	}
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	select {
	case err := <-firstDone:
		t.Fatalf("the request without a max wait left: %v", err)
	default:
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Errorf("first request: %v", err)
	}
	if fake.count() != 1 {
		t.Errorf("%d requests reached Discord", fake.count())
	}
}

// A client that gives up on a request held behind others is answered at
// once, the others staying held.
func TestHeldRequestBehindAnotherLeavesWhenItsClientGivesUp(t *testing.T) {
	remaining := 1
	fake := &fakeDiscord{answer: bucketOfFive(&remaining, "30")}
	q := newTestQueue(fake)
	q.send(t, "GET", reserveRoute, "")

	keep, cancelKeep := context.WithCancel(context.Background())
	defer cancelKeep()
	keptDone, _ := q.queueHeld(keep, reserveRoute, "30", "")
	time.Sleep(50 * time.Millisecond)
	leave, cancelLeave := context.WithCancel(context.Background())
	leftDone, _ := q.queueHeld(leave, reserveRoute, "70", "")
	time.Sleep(50 * time.Millisecond)
	cancelLeave()
	select {
	case err := <-leftDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("left with %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("still held after its client gave up")
	}
	select {
	case err := <-keptDone:
		t.Errorf("the other request left too: %v", err)
	default:
	}
}
