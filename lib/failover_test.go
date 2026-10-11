package lib

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A peer set aside loses its buckets to the next highest scoring node, which
// is the one hrwOwner gives once the peer has left, and gets them back once
// the time is up.
func TestSetAsidePeerHandsItsBucketsToTheNextOwner(t *testing.T) {
	members := []string{"nirn-a", "nirn-b", "nirn-c"}
	address := map[string]string{"nirn-a": "10.0.0.1:8080", "nirn-b": "10.0.0.2:8080", "nirn-c": "10.0.0.3:8080"}
	var aside asidePeers
	now := time.Now()
	aside.set("10.0.0.2:8080", now)

	for _, k := range keys(3000) {
		got := hrwOwner(k, aside.filter(members, address, now))
		if want := hrwOwner(k, []string{"nirn-a", "nirn-c"}); got != want {
			t.Fatalf("bucket %d went to %s, want %s", k, got, want)
		}
	}
	if kept := aside.filter(members, address, now.Add(setAsideFor+time.Second)); len(kept) != 3 {
		t.Errorf("after the delay %v are tried, want all three", kept)
	}
	aside.clear("10.0.0.2:8080")
	if kept := aside.filter(members, address, now); len(kept) != 3 {
		t.Errorf("after joining again %v are tried, want all three", kept)
	}
}

// A refused connection is told apart from a failure once the request is sent,
// and the body of a relay that never connected can be sent again.
func TestRefusedRelayCanBeSentAgain(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	l.Close()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer srv.Close()

	body := &relayBody{ReadCloser: io.NopCloser(strings.NewReader(`{"content":"hi"}`))}
	req, _ := http.NewRequest("POST", "http://"+closed+"/api/v10/channels/1/messages", body)
	if _, err := http.DefaultClient.Do(req); !isDialError(err) {
		t.Fatalf("refused connection not told apart: %v", err)
	}
	if body.started {
		t.Fatal("body read before the connection was made")
	}

	req, _ = http.NewRequest("POST", srv.URL+"/api/v10/channels/1/messages", body)
	if _, err := http.DefaultClient.Do(req); err != nil || isDialError(err) {
		t.Fatal(err)
	}
	if got != `{"content":"hi"}` {
		t.Errorf("peer received %q", got)
	}
}
