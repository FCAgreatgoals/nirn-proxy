// Command conformance walks a Discord test guild through every route nirn is
// expected to handle, a few requests each, paced well below every limit, and
// reports what each route returned and which rate limit it fell under.
//
// It is a conformance check, not a load test: requests are spread over the
// whole duration, a bucket that reports nothing left is waited out, and a 429
// is recorded and never retried. Run it once directly against Discord and once
// through the proxy, then compare the two reports.
//
//	go run ./cmd/conformance -api https://discord.com/api/v10 -report direct.json ...
//	go run ./cmd/conformance -api http://localhost:8080/api/v10 -report proxied.json ...
//	go run ./cmd/conformance compare direct.json proxied.json
//
// It needs a guild set aside for testing, whose name carries the marker, where
// the bot is an administrator, and four test users who are members of it.
// Everything it creates is named after the marker and removed at the end.
// Kicking and banning take test users out of the guild, so they only run when
// asked, and the run prints an invite for them to come back.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// plannedRequests is roughly how many requests a full run sends. The duration
// is divided by it to space them out.
const plannedRequests = 120

func main() {
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		os.Exit(runCompare(os.Args[2:]))
	}

	api := flag.String("api", "https://discord.com/api/v10", "API root: Discord itself, or the proxy in front of it")
	token := flag.String("token", os.Getenv("TOKEN"), "bot token, without the Bot prefix")
	guild := flag.String("guild", "", "id of the guild set aside for testing")
	users := flag.String("users", "", "comma separated ids of the four test users")
	marker := flag.String("marker", "nirn-conformance", "text the guild name must contain before anything is touched")
	duration := flag.Duration("duration", 10*time.Minute, "how long to spread the run over")
	allowKick := flag.Bool("allow-kick", false, "kick test user 3, who then has to rejoin")
	allowBan := flag.Bool("allow-ban", false, "ban then unban test user 4, who then has to rejoin")
	out := flag.String("report", "conformance.json", "where to write the report")
	flag.Parse()

	ids := splitIDs(*users)
	switch {
	case *token == "" || *guild == "":
		fail("-token and -guild are required")
	case len(ids) != 4:
		fail(fmt.Sprintf("-users needs four test user ids, got %d", len(ids)))
	case *marker == "":
		fail("-marker cannot be empty: it is what keeps the run off a real guild")
	}

	spacing := *duration / plannedRequests
	if spacing < 250*time.Millisecond {
		spacing = 250 * time.Millisecond
	}

	c := newClient(*api, strings.TrimPrefix(*token, "Bot "), spacing)
	s := &scenario{c: c, guild: *guild, users: ids, marker: *marker, allowKick: *allowKick, allowBan: *allowBan}
	r := &report{API: *api, Started: time.Now(), Spacing: spacing.String()}

	runErr := s.run()
	if runErr == nil {
		r.Invite = s.rejoinInvite()
	}
	r.Finished = time.Now()
	r.Results = c.results
	r.Failures = s.failures
	r.Skipped = s.skipped
	r.Buckets = modelBuckets(c.results)

	if raw, err := json.MarshalIndent(r, "", "  "); err == nil {
		_ = os.WriteFile(*out, raw, 0o644)
	}
	r.print(os.Stdout)
	if runErr != nil {
		fail(runErr.Error())
	}
	if len(r.Failures) > 0 || len(r.tooMany()) > 0 {
		os.Exit(1)
	}
}

func runCompare(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: conformance compare direct.json proxied.json")
		return 2
	}
	direct, err := loadReport(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	proxied, err := loadReport(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if compare(os.Stdout, direct, proxied) {
		return 0
	}
	return 1
}

func splitIDs(s string) []string {
	var ids []string
	for _, id := range strings.Split(s, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "conformance:", msg)
	os.Exit(2)
}
