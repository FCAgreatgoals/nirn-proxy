package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

type report struct {
	API      string        `json:"api"`
	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished"`
	Spacing  string        `json:"spacing"`
	Results  []result      `json:"results"`
	Failures []string      `json:"failures"`
	Skipped  []string      `json:"skipped"`
	Invite   string        `json:"rejoin_invite,omitempty"`
	Buckets  []bucketModel `json:"buckets"`
}

// bucketModel is what the run learned about one Discord bucket.
type bucketModel struct {
	Bucket string   `json:"bucket"`
	Routes []string `json:"routes"`
	Limit  int      `json:"limit"`
	// Model is "token bucket", "fixed window", or "unknown" when no two
	// requests of the run fell within one window of each other.
	Model string `json:"model"`
	// WindowSeconds is the time the bucket takes to refill fully.
	WindowSeconds float64 `json:"window_seconds"`
	Pairs         int     `json:"pairs"`
}

// modelBuckets classifies each bucket from consecutive requests on it.
//
// X-RateLimit-Reset-After is the time until the bucket is full again, so the
// instant it names stays put across the requests of a fixed window and moves
// forward by a constant step on a token bucket. Two requests close together are
// enough to tell them apart.
func modelBuckets(results []result) []bucketModel {
	type key struct{ bucket, major string }
	samples := map[key][]result{}
	routes := map[string]map[string]bool{}
	for _, r := range results {
		if r.Bucket == "" || r.Remaining < 0 {
			continue
		}
		k := key{r.Bucket, r.Major}
		samples[k] = append(samples[k], r)
		if routes[r.Bucket] == nil {
			routes[r.Bucket] = map[string]bool{}
		}
		routes[r.Bucket][r.Method+" "+r.Route] = true
	}

	byBucket := map[string]*bucketModel{}
	for k, rs := range samples {
		m := byBucket[k.bucket]
		if m == nil {
			m = &bucketModel{Bucket: k.bucket, Model: "unknown"}
			for route := range routes[k.bucket] {
				m.Routes = append(m.Routes, route)
			}
			sort.Strings(m.Routes)
			byBucket[k.bucket] = m
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].At.Before(rs[j].At) })

		// The reference is the reset-after of a request that opened the
		// bucket: on a token bucket it is the time one token takes to come
		// back, on a fixed window the window itself. Later requests are no
		// reference, since a token bucket's reset-after grows with each one.
		// A run can start on a bucket already in use, with no opening
		// request; the step between two requests then stands in for it.
		reference := 0.0
		for _, r := range rs {
			if r.Limit > m.Limit {
				m.Limit = r.Limit
			}
			if r.Remaining == r.Limit-1 && r.ResetAfter > reference {
				reference = r.ResetAfter
			}
		}

		for i := 1; i < len(rs); i++ {
			prev, r := rs[i-1], rs[i]
			gap := r.At.Sub(prev.At).Seconds()
			if r.Remaining != prev.Remaining-1 || gap >= prev.ResetAfter || prev.ResetAfter <= 0 {
				continue
			}
			// How far the instant of the next full refill moved: nothing on a
			// fixed window, one token's worth on a token bucket. The tolerance
			// absorbs the network time folded into the two timestamps.
			step := (gap + r.ResetAfter) - prev.ResetAfter
			m.Pairs++
			if math.Abs(step) <= 0.05*prev.ResetAfter+0.05 {
				m.Model = "fixed window"
				continue
			}
			m.Model = "token bucket"
			perToken := reference
			if perToken == 0 {
				perToken = step
			}
			m.WindowSeconds = perToken * float64(m.Limit)
		}

		if m.Model != "token bucket" {
			window := reference
			if window == 0 {
				for _, r := range rs {
					window = math.Max(window, r.ResetAfter)
				}
			}
			m.WindowSeconds = math.Max(m.WindowSeconds, window)
		}
	}

	out := make([]bucketModel, 0, len(byBucket))
	for _, m := range byBucket {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Routes[0] < out[j].Routes[0] })
	return out
}

func (r *report) tooMany() []result {
	var out []result
	for _, res := range r.Results {
		if res.Status == 429 {
			out = append(out, res)
		}
	}
	return out
}

func (r *report) print(w io.Writer) {
	statuses := map[int]int{}
	for _, res := range r.Results {
		statuses[res.Status]++
	}
	codes := make([]int, 0, len(statuses))
	for code := range statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)

	fmt.Fprintf(w, "\n%d requests to %s in %s (one every %s)\n", len(r.Results), r.API, r.Finished.Sub(r.Started).Round(time.Second), r.Spacing)
	for _, code := range codes {
		fmt.Fprintf(w, "  %3d  x%d\n", code, statuses[code])
	}

	fmt.Fprintf(w, "\nBuckets met\n")
	for _, b := range r.Buckets {
		shared := ""
		if len(b.Routes) > 1 {
			shared = fmt.Sprintf("  shared by %d routes", len(b.Routes))
		}
		fmt.Fprintf(w, "  %-12s %4d per %-9s %s%s\n", b.Model, b.Limit, humanSeconds(b.WindowSeconds), b.Routes[0], shared)
	}

	if tm := r.tooMany(); len(tm) > 0 {
		fmt.Fprintf(w, "\n429 received, %d: each is a request that should never have been sent\n", len(tm))
		for _, res := range tm {
			fmt.Fprintf(w, "  %s  %s %s  scope=%s\n", res.Step, res.Method, res.Route, res.Scope)
		}
	}
	if len(r.Failures) > 0 {
		fmt.Fprintf(w, "\nFailed steps, %d\n", len(r.Failures))
		for _, f := range r.Failures {
			fmt.Fprintf(w, "  %s\n", f)
		}
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintf(w, "\nSkipped after an earlier failure: %s\n", strings.Join(r.Skipped, ", "))
	}
	if r.Invite != "" {
		fmt.Fprintf(w, "\nKicked or banned test users can rejoin with %s\n", r.Invite)
	}
}

func humanSeconds(s float64) string {
	switch {
	case s >= 3600:
		return fmt.Sprintf("%.1fh", s/3600)
	case s >= 60:
		return fmt.Sprintf("%.1fmin", s/60)
	default:
		return fmt.Sprintf("%.3fs", s)
	}
}

func loadReport(path string) (*report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r report
	return &r, json.Unmarshal(raw, &r)
}

// compare puts a direct run and a run through the proxy side by side. The
// proxy passes when every step answers the same, and nothing reached a 429.
func compare(w io.Writer, direct, proxied *report) bool {
	status := func(r *report) map[string]int {
		out := map[string]int{}
		for _, res := range r.Results {
			out[res.Step] = res.Status
		}
		return out
	}
	a, b := status(direct), status(proxied)

	var differ []string
	for step, code := range a {
		if other, ok := b[step]; ok && other != code {
			differ = append(differ, fmt.Sprintf("  %s: %d direct, %d through the proxy", step, code, other))
		}
	}
	sort.Strings(differ)

	fmt.Fprintf(w, "direct  %s: %d requests, %d 429\n", direct.API, len(direct.Results), len(direct.tooMany()))
	fmt.Fprintf(w, "proxied %s: %d requests, %d 429\n", proxied.API, len(proxied.Results), len(proxied.tooMany()))
	fmt.Fprintf(w, "median latency: %.0f ms direct, %.0f ms through the proxy\n", medianLatency(direct), medianLatency(proxied))
	if len(differ) > 0 {
		fmt.Fprintf(w, "\nsteps that answered differently\n%s\n", strings.Join(differ, "\n"))
	}
	ok := len(differ) == 0 && len(proxied.tooMany()) == 0
	if ok {
		fmt.Fprintln(w, "\nsame answers on every step, and no 429 through the proxy")
	}
	return ok
}

func medianLatency(r *report) float64 {
	if len(r.Results) == 0 {
		return 0
	}
	values := make([]float64, 0, len(r.Results))
	for _, res := range r.Results {
		values = append(values, res.LatencyMs)
	}
	sort.Float64s(values)
	return values[len(values)/2]
}
