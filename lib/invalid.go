package lib

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
)

// Discord restricts an IP address for a while once it makes 10,000 invalid
// requests in ten minutes. An invalid request is a 401, a 403 or a 429, except
// 429s scoped "shared". The restriction takes every bot behind the proxy down
// with it, and it cannot be seen coming from any single response.
//
// The documentation asks large applications to "consider logging and tracking
// the rate of invalid requests to avoid reaching this hard limit"
// (/developers/topics/rate-limits#invalid-request-limit-aka-cloudflare-bans).
// @discordjs/rest counts them for the same reason. This is that count, per
// node, since the limit applies per IP.

const (
	invalidRequestLimit  = 10_000
	invalidRequestWindow = 10 // minutes, one slot each
	// invalidRequestWarnFrom is where warnings start. Below it the count is
	// exposed as a metric only, so ordinary 403s do not flood the logs.
	invalidRequestWarnFrom = invalidRequestLimit / 2
)

var (
	InvalidRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nirn_proxy_invalid_requests",
		Help: "Responses Discord counts against the invalid request limit (401, 403, and 429 not scoped shared)",
	}, []string{"status"})

	InvalidRequestsWindow = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "nirn_proxy_invalid_requests_window",
		Help: "Invalid requests over the last ten minutes, against Discord's limit of 10,000 per IP",
	})
)

// invalidRequestTracker counts invalid requests over a sliding ten minute
// window, one slot per minute.
type invalidRequestTracker struct {
	mu      sync.Mutex
	counts  [invalidRequestWindow]int64
	minutes [invalidRequestWindow]int64
	// warned is the last thousand reported, so each one is logged once.
	warned int64
}

var invalidRequests invalidRequestTracker

// countsAsInvalid tells whether Discord counts a response against the limit.
func countsAsInvalid(status int, scope string) bool {
	switch status {
	case 401, 403:
		return true
	case 429:
		return scope != "shared"
	}
	return false
}

// record adds an invalid request and returns the count over the window.
func (t *invalidRequestTracker) record(now time.Time) int64 {
	minute := now.Unix() / 60
	slot := minute % invalidRequestWindow

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.minutes[slot] != minute {
		t.minutes[slot] = minute
		t.counts[slot] = 0
	}
	t.counts[slot]++

	var total int64
	for i := range t.counts {
		if minute-t.minutes[i] < invalidRequestWindow {
			total += t.counts[i]
		}
	}
	return total
}

// count returns the number of invalid requests over the window, without
// recording one.
func (t *invalidRequestTracker) count(now time.Time) int64 {
	minute := now.Unix() / 60

	t.mu.Lock()
	defer t.mu.Unlock()

	var total int64
	for i := range t.counts {
		if minute-t.minutes[i] < invalidRequestWindow {
			total += t.counts[i]
		}
	}
	return total
}

// shouldWarn tells whether a count crosses a thousand not yet reported. The
// mark follows the count back down, so a later climb is reported again.
func (t *invalidRequestTracker) shouldWarn(total int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	thousand := total / 1000
	if thousand < t.warned {
		t.warned = thousand
	}
	if total < invalidRequestWarnFrom || thousand <= t.warned {
		return false
	}
	t.warned = thousand
	return true
}

// trackInvalidRequest records a response if Discord counts it as invalid.
func trackInvalidRequest(status int, scope, identifier, path string) {
	if !countsAsInvalid(status, scope) {
		return
	}
	InvalidRequests.WithLabelValues(strconv.Itoa(status)).Inc()

	total := invalidRequests.record(time.Now())
	InvalidRequestsWindow.Set(float64(total))

	if invalidRequests.shouldWarn(total) {
		logger.WithFields(logrus.Fields{
			"invalidRequests": total,
			"limit":           invalidRequestLimit,
			"identifier":      identifier,
			"lastPath":        path,
			"lastStatus":      status,
		}).Warn("Approaching Discord's invalid request limit, this IP risks a temporary ban")
	}
}
