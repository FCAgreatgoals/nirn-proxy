package main

import (
	"math"
	"testing"
	"time"
)

// sequence builds the results of consecutive requests on one bucket, at the
// given offsets, with the remaining and reset-after Discord answered.
func sequence(route string, limit int, offsets, resets []float64, remaining []int) []result {
	start := time.Unix(1_800_000_000, 0)
	out := make([]result, len(offsets))
	for i := range offsets {
		out[i] = result{
			Method: "PATCH", Route: route, Bucket: route, Limit: limit,
			Remaining: remaining[i], ResetAfter: resets[i],
			At: start.Add(time.Duration(offsets[i] * float64(time.Second))),
		}
	}
	return out
}

// The three sequences are real answers from Discord, taken from a capture.
func TestModelBucketsOnCapturedSequences(t *testing.T) {
	cases := []struct {
		name    string
		results []result
		model   string
		window  float64
	}{
		{
			"command edits, a token bucket",
			sequence("/commands", 5, []float64{0, 0.22, 0.41, 0.60, 0.77}, []float64{4.000, 7.821, 11.624, 15.421, 19.246}, []int{4, 3, 2, 1, 0}),
			"token bucket", 20,
		},
		{
			"bot nickname, a fixed window",
			sequence("/members/@me", 20, []float64{0, 7.025}, []float64{300, 292.569}, []int{19, 18}),
			"fixed window", 300,
		},
		{
			// A run that starts on a bucket already in use has no request that
			// opened it, as when the bot kept working between two runs.
			"role creation, joined half way through",
			sequence("/roles", 250, []float64{0, 0.25, 0.5}, []float64{2738.944, 3429.892, 4120.841}, []int{246, 245, 244}),
			"token bucket", 172800,
		},
		{
			"bot nickname, joined half way through",
			sequence("/members/@me", 20, []float64{0, 0.254}, []float64{269.667, 269.414}, []int{17, 16}),
			"fixed window", 269.667,
		},
		{
			"role creation, two hundred and fifty per forty-eight hours",
			sequence("/roles", 250, []float64{0, 165.48}, []float64{691.2, 1216.942}, []int{249, 248}),
			"token bucket", 172800,
		},
	}
	for _, c := range cases {
		got := modelBuckets(c.results)
		if len(got) != 1 {
			t.Fatalf("%s: %d buckets, want 1", c.name, len(got))
		}
		if got[0].Model != c.model {
			t.Errorf("%s: model %q, want %q", c.name, got[0].Model, c.model)
		}
		if math.Abs(got[0].WindowSeconds-c.window)/c.window > 0.01 {
			t.Errorf("%s: window %.1fs, want %.1fs", c.name, got[0].WindowSeconds, c.window)
		}
	}
}
