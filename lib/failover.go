package lib

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/sirupsen/logrus"
)

// A peer that refuses a connection is down or restarting. memberlist takes
// seconds to declare it gone, and until then every request whose bucket it
// owns fails, and so does every request of a bot whose global limit it holds.
// The node that meets the refusal sets the peer aside at once and goes to the
// next highest scoring node. Every node that meets it lands on the same one,
// so a bucket keeps a single owner.

// setAsideFor is how long a refusing peer stays aside unless it joins again;
// memberlist declares a dead node gone well before.
const setAsideFor = 10 * time.Second

type asidePeers struct {
	mu    sync.Mutex
	until map[string]time.Time // proxy address → when the peer is tried again
}

func (a *asidePeers) set(addr string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.until == nil {
		a.until = map[string]time.Time{}
	}
	a.until[addr] = now.Add(setAsideFor)
}

func (a *asidePeers) clear(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.until, addr)
}

// filter drops from members those whose address is aside.
func (a *asidePeers) filter(members []string, address map[string]string, now time.Time) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.until) == 0 {
		return members
	}
	kept := make([]string, 0, len(members))
	for _, name := range members {
		if until, ok := a.until[address[name]]; ok && now.Before(until) {
			continue
		}
		kept = append(kept, name)
	}
	return kept
}

// isDialError says whether a request never reached its peer.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func (m *QueueManager) setAside(addr string, logEntry *logrus.Entry) {
	m.aside.set(addr, time.Now())
	logEntry.WithField("peer", addr).Warn("Peer refused the connection, setting it aside")
}

func nodeProxyAddr(node *memberlist.Node) string {
	return node.Addr.String() + ":" + string(node.Meta)
}

// relayBody lets a relay that never reached its peer be sent again: the
// transport closes a request's body even when it could not connect, and reads
// nothing of it before it has.
type relayBody struct {
	io.ReadCloser
	started bool
}

func (b *relayBody) Read(p []byte) (int, error) {
	b.started = true
	return b.ReadCloser.Read(p)
}

func (b *relayBody) Close() error { return nil }
