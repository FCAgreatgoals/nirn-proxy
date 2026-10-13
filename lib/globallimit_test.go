package lib

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// By default the global limit is the documented 50 per second, and working it
// out costs no request: /gateway/bot, which the detection would call, is
// limited to 2 requests per 5 seconds and the bot's shards need them.
func TestGlobalLimitDefaultsToTheDocumentedValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the global limit cost a request to %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	pointAt(t, srv)

	user := &BotUserResponse{Id: "203039963636301824"}
	if limit, err := GetBotGlobalLimit("Bot x", user); err != nil || limit != 50 {
		t.Errorf("default limit = %d (%v), want 50", limit, err)
	}

	// A bot whose limit Discord raised says so explicitly.
	globalOverrideMap[user.Id] = 120
	defer delete(globalOverrideMap, user.Id)
	if limit, _ := GetBotGlobalLimit("Bot x", user); limit != 120 {
		t.Errorf("overridden limit = %d, want 120", limit)
	}
}

// The global limit counts requests as they leave for Discord, not as they
// arrive: requests that waited out a global lock in their bucket's queue
// never reach Discord more than the limit in a second.
func TestGlobalLimitCountsRequestsAsTheyLeave(t *testing.T) {
	fake := &fakeDiscord{answer: func(*http.Request, []byte) (int, http.Header, string) { return 204, http.Header{}, "" }}
	q := newTestQueue(fake)
	q.user = &BotUserResponse{Id: "203039963636301824"}
	q.botLimit = 5
	m := NewQueueManager(16, 16)
	m.bindGlobal(q)
	m.queues["Bot x"] = q

	// A global 429 a moment ago: everything waits, and keeps arriving.
	*q.globalLockedUntil = time.Now().Add(1500 * time.Millisecond).UnixNano()
	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/api/v10/channels/203039963636301824", nil)
			req.Header.Set("Authorization", "Bot x")
			m.DiscordRequestHandler(httptest.NewRecorder(), req)
		}()
		time.Sleep(100 * time.Millisecond)
	}
	wg.Wait()

	for i := 0; i+5 < fake.count(); i++ {
		if gap := fake.call(i + 5).at.Sub(fake.call(i).at); gap < time.Second {
			t.Fatalf("requests %d to %d reached Discord within %v, over 5 a second", i+1, i+6, gap)
		}
	}
}
