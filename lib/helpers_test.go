package lib

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDiscord is a processor that answers from a script instead of calling
// Discord, and remembers when each request reached it.
type fakeDiscord struct {
	mu     sync.Mutex
	calls  []fakeCall
	answer func(req *http.Request, body []byte) (int, http.Header, string)
}

type fakeCall struct {
	at     time.Time
	method string
	path   string
}

func (f *fakeDiscord) process(_ context.Context, item *QueueItem) (*http.Response, error) {
	body, _ := io.ReadAll(item.Req.Body)
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{time.Now(), item.Req.Method, item.Req.URL.Path})
	f.mu.Unlock()

	status, header, payload := 200, http.Header{}, ""
	if f.answer != nil {
		status, header, payload = f.answer(item.Req, body)
	}
	w := *item.Res
	for k, v := range header {
		w.Header()[k] = v
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(payload))
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(payload))}, nil
}

func (f *fakeDiscord) call(n int) fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[n]
}

func (f *fakeDiscord) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newTestQueue builds a queue around a fake Discord. NewRequestQueue would
// call Discord to identify the bot, so the queue is assembled by hand.
func newTestQueue(f *fakeDiscord) *RequestQueue {
	return &RequestQueue{
		queues:            map[uint64]*QueueChannel{},
		processor:         f.process,
		globalLockedUntil: new(int64),
		isTokenInvalid:    new(int64),
		bufferSize:        16,
		identifier:        "test",
		queueType:         Bot,
		botLimit:          50,
	}
}

// send routes a request the way the queue manager would, and returns what the
// client received.
func (q *RequestQueue) send(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bot test")
	routingHash, bucket, _ := (&QueueManager{}).GetRequestRoutingInfo(req, "Bot test")
	rec := httptest.NewRecorder()
	var res http.ResponseWriter = rec
	if err := q.Queue(req, &res, bucket, routingHash); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return rec
}
