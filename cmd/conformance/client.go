package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// result is one request as it happened.
type result struct {
	Step       string    `json:"step"`
	Method     string    `json:"method"`
	Route      string    `json:"route"`
	Major      string    `json:"major,omitempty"`
	Status     int       `json:"status"`
	At         time.Time `json:"at"`
	LatencyMs  float64   `json:"latency_ms"`
	Bucket     string    `json:"bucket,omitempty"`
	Limit      int       `json:"limit,omitempty"`
	Remaining  int       `json:"remaining"`
	ResetAfter float64   `json:"reset_after,omitempty"`
	Scope      string    `json:"scope,omitempty"`
	Global     bool      `json:"global,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// bucketState is what the last response of a bucket said about it.
type bucketState struct {
	remaining int
	resetAt   time.Time
}

// client sends the scenario's requests one at a time, paced well below every
// limit it learns of.
//
// It never races a limit. Requests are spaced by a fixed interval, and when a
// bucket reports nothing left the next request on it waits for its reset. A
// 429 is recorded as a failure and never retried: through the proxy, any 429
// means the proxy let a request through that Discord refused.
type client struct {
	api     string
	token   string
	http    *http.Client
	spacing time.Duration
	last    time.Time

	// routeBuckets maps a route and its major parameter to the Discord bucket
	// last reported for it, so that routes sharing a bucket share its state.
	routeBuckets map[string]string
	buckets      map[string]*bucketState

	results []result
}

func newClient(api, token string, spacing time.Duration) *client {
	return &client{
		api:          strings.TrimRight(api, "/"),
		token:        token,
		http:         &http.Client{Timeout: 30 * time.Second},
		spacing:      spacing,
		routeBuckets: map[string]string{},
		buckets:      map[string]*bucketState{},
	}
}

// call describes one request of the scenario.
type call struct {
	step   string
	method string
	route  string // template, e.g. /channels/{channel_id}/messages
	path   string // concrete path
	body   any
	// immediate skips the spacing, to follow the previous request at once.
	// The scenario uses it for pairs of requests on one bucket, which is what
	// tells a token bucket from a fixed window, and stays two requests.
	immediate bool
	// reason is sent as X-Audit-Log-Reason, so every action the engine takes
	// is recognisable in the guild's audit log.
	reason string
}

// do sends a request and decodes a JSON answer into out when out is not nil.
// A non-2xx status is returned as an error, after being recorded.
func (c *client) do(k call, out any) error {
	major := majorOf(k.route, k.path)
	c.pace(k, major)

	var body io.Reader
	if k.body != nil {
		raw, err := json.Marshal(k.body)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(k.method, c.api+k.path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/FCAgreatgoals/nirn-proxy, conformance)")
	if k.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	reason := k.reason
	if reason == "" {
		reason = "nirn conformance: " + k.step
	}
	req.Header.Set("X-Audit-Log-Reason", reason)

	start := time.Now()
	res, err := c.http.Do(req)
	c.last = time.Now()
	r := result{Step: k.step, Method: k.method, Route: k.route, Major: major, At: start}
	if err != nil {
		r.Error = err.Error()
		c.results = append(c.results, r)
		return err
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(res.Body)

	r.Status = res.StatusCode
	r.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	c.observe(&r, res.Header)
	c.results = append(c.results, r)

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("%s %s: %d %s", k.method, k.path, res.StatusCode, truncate(string(payload), 200))
	}
	if out != nil && len(payload) > 0 {
		return json.Unmarshal(payload, out)
	}
	return nil
}

// pace waits for the spacing, and for the bucket's reset if it reported
// nothing left.
func (c *client) pace(k call, major string) {
	if !k.immediate {
		if wait := c.spacing - time.Since(c.last); wait > 0 {
			time.Sleep(wait)
		}
	}
	if id, ok := c.routeBuckets[k.method+" "+k.route+" "+major]; ok {
		if b := c.buckets[id]; b != nil && b.remaining == 0 {
			if wait := time.Until(b.resetAt); wait > 0 {
				time.Sleep(wait)
			}
		}
	}
}

// observe reads the rate limit headers into the result and the bucket state.
func (c *client) observe(r *result, h http.Header) {
	r.Bucket = h.Get("X-RateLimit-Bucket")
	r.Scope = h.Get("X-RateLimit-Scope")
	r.Global = h.Get("X-RateLimit-Global") == "true"
	r.Limit, _ = strconv.Atoi(h.Get("X-RateLimit-Limit"))
	r.Remaining = -1
	if v, err := strconv.Atoi(h.Get("X-RateLimit-Remaining")); err == nil {
		r.Remaining = v
	}
	r.ResetAfter, _ = strconv.ParseFloat(h.Get("X-RateLimit-Reset-After"), 64)
	if r.Status == http.StatusTooManyRequests {
		if retry, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && retry > r.ResetAfter {
			r.ResetAfter = retry
		}
	}

	if r.Bucket == "" {
		return
	}
	id := r.Bucket + ":" + r.Major
	c.routeBuckets[r.Method+" "+r.Route+" "+r.Major] = id
	b := c.buckets[id]
	if b == nil {
		b = &bucketState{}
		c.buckets[id] = b
	}
	b.remaining = r.Remaining
	if r.Status == http.StatusTooManyRequests {
		b.remaining = 0
	}
	b.resetAt = r.At.Add(time.Duration(r.ResetAfter * float64(time.Second)))
}

// majorOf extracts the major parameter from a path, given its template.
func majorOf(route, path string) string {
	rParts := strings.Split(strings.Trim(route, "/"), "/")
	pParts := strings.Split(strings.Trim(strings.SplitN(path, "?", 2)[0], "/"), "/")
	if len(rParts) != len(pParts) {
		return ""
	}
	for i, part := range rParts {
		switch part {
		case "{channel_id}", "{guild_id}", "{webhook_id}":
			return pParts[i]
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
