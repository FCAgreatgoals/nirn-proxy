# Fixes over upstream nirn-proxy

This branch starts from the last upstream commit (`germanoeich/nirn-proxy`,
`97b3251`, archived in March 2026) and fixes what the forks found wrong, one
bug at a time, each with a test that fails without the fix.

The forks were used as a catalogue of known bugs, not as code to merge. Every
entry below was checked against upstream itself: several fork fixes only
repair code the fork had changed, and are listed at the end as not applicable.

Evidence comes from three places: the upstream code, Discord's documentation,
and a capture of real Discord responses taken through a transparent proxy.

## Confirmed in upstream

| # | Bug | Evidence | Found in | Status |
|---|---|---|---|---|
| 1 | Does not build with Go 1.23+: the pinned `golang.org/x/net` uses a `go:linkname` newer toolchains reject | `link: invalid reference to syscall.recvmsg` | every stale fork | fixed: `x/net` v0.58, the newest that still builds with Go 1.25 |
| 2 | The global lock is dead code: `globalLockedUntil` is written after a global 429 and never read, so requests keep flowing into a global limit; it was also written with a compare-and-swap from zero that nothing reset | `grep globalLockedUntil` | davfsa | fixed, tested |
| 3 | Any 404 under `/webhooks/` locks the whole route as Unknown Webhook, including a deleted webhook *message* (10008), so later requests get a fake 404 until the queue is swept | code; Discord only asks to stop on an unknown webhook | bsian03, Melonly (test) | fixed, tested |
| 4 | The upstream URL is rebuilt from the decoded path: a keycap emoji `#️⃣` turns its `#` into a fragment and the reaction request reaches Discord truncated to `.../reactions/`. The hop between cluster nodes had the same bug, and read the request before checking it had been built | reproduced | bsian03 (partial fix) | fixed, tested end to end |
| 5 | Every `/channels/{id}` request, all methods, shares one queue across all channels: locking many channels during a raid serializes them, and one exhausted channel sleeps them all | code; `channel_id` is a major parameter | DraftBot, LorittaBot, TicketsBot | fixed, tested |
| 6 | `/guilds/{id}/channels` shares one queue across all guilds | code; `guild_id` is a major parameter | DraftBot, TicketsBot | fixed, tested |
| 7 | Within one interaction, the followup route (`POST /webhooks/{id}/{token}`) and the original response (`.../messages/@original`) run in separate queues, although Discord counts them together | capture: the four routes share one `X-RateLimit-Bucket` | davfsa, Melonly (through bucket learning) | fixed: one queue per interaction for its webhook routes, tested |
| 8 | Interaction endpoints wait on and spend the global limit, which Discord documents they are exempt from | docs, "Global Rate Limit" | Melonly | fixed, tested |
| 9 | A channel rename or topic change shares its queue with every other channel edit, and after its sublimit 429 only the bucket's short reset was slept, so each following rename went back into the sublimit | @discordjs/rest `hasSublimit` | none | fixed, tested |
| 10 | Invalid requests (401, 403, non-shared 429) are not tracked, although 10,000 in ten minutes gets the IP banned | docs, "Invalid Request Limit" | Melonly | fixed: metrics and warnings (Melonly also stops sending at 9,500) |
| 11 | `strings.SplitN(url, "?", 1)` never splits, so `isInteraction` inspects the query string too and a long query value exempts a request from the 401 lock | Go semantics | Melonly | fixed, tested |
| 12 | The global limit is inferred from `max_concurrency` (500, or 25 per unit), a heuristic Discord does not document, at the cost of a `/gateway/bot` request (2 per 5 seconds) | docs: 50/s unless raised by support; upstream planned to drop it | Melonly | fixed: off by default, `BOT_RATELIMIT_OVERRIDES` for raised limits |
| 13 | The Discord address is hardcoded, so the proxy cannot be pointed at a simulator | code | WelcomerTeam | fixed: `DISCORD_URL`, tested |
| 14 | The rule that gives deletes of messages older than 14 days (and newer than 10 seconds) their own queue never fires: it indexes the full path while looping over its tail, so it compares against the wrong segment | reproduced: a 30 day old message lands in `/messages/!` | davfsa, Melonly | fixed, tested |
| 15 | The 429 the proxy makes up itself sets `x-ratelimit-after`, a header Discord never sends, instead of `X-RateLimit-Reset-After` | code | none | fixed, tested |
| 16 | A node advertises whatever address memberlist guesses, which inside a container can be unreachable from the other nodes | code | DraftBot (first IPv4), PluralKit (configured host) | fixed: `CLUSTER_ADVERTISE_ADDR`, empty keeps upstream behaviour, `auto` is DraftBot's guess, a host is resolved as PluralKit does |

Upstream queues ignore the HTTP method, which makes them coarser than Discord's buckets (`GET` and `POST` on a channel's messages share a queue). That only ever waits more than needed, never sends into a limit, so it is left as is. For the same reason, adding then removing a member role is already coordinated: both land in the same queue.

## Rate limit models

Discord runs some buckets as token buckets and others as fixed windows, which
a capture of real traffic shows: on a token bucket `X-RateLimit-Reset-After`
grows with each request, because it announces the time until the bucket is
full again, not the end of a window.

Upstream sleeps that value once nothing remains. It can only ever wait too
long, never send into the limit, whichever model the bucket follows, and
`TestTokenBucketIsNeverSentIntoTheLimit` replays the captured sequence to keep
it that way. davfsa went further and models token buckets to send again as soon
as one token is back. That is not ported: its detection had no tests, and a
client built on @discordjs/rest waits for the full refill itself anyway, so the
proxy being quicker would not reach it.

## To confirm before changing anything

| Question | Why it is open |
|---|---|
| Do all `GET /users/{id}` share one counter? | No major parameter, so the docs suggest yes, and Melonly assumes it. The capture has no two users within one window. The probe can settle it. |
| Do reactions need a pause after each modify? | DraftBot and PluralKit sleep 250 ms, taken from eris. No capture supports it yet. |
| Does editing a message older than an hour have its own bucket? | davfsa queues it apart (`!1hmsg`) without saying why. The only edit captured was of a fresh message. |

## Fork fixes, one by one

### DraftBot

| Commit | Outcome |
|---|---|
| Use different bucket if it is a PATCH request | covered by #5, which keeps every channel apart, not only PATCH |
| fix: correct global bucket for guilds channels | covered by #6 |
| fix: Add custom AdvertiseAddr for Docker case | #16, configurable rather than always on |
| Fix rate limit issues with reactions | not ported: a 250 ms sleep with no capture behind it, see above |
| ci: update CI to auto publish to registry, ci: update versions | not applicable: DraftBot's own registry |

### davfsa

| Commit | Outcome |
|---|---|
| Fix handling of global ratelimits | #2 |
| properly calculate message delete bucket path | #14 |
| do not merge callback endpoints | already the case upstream |
| Implement sliding window ratelimiting, and its follow-ups | not ported, see "Rate limit models" |
| tracking in transit requests, ALLOW_CONCURRENT_REQUESTS | not ported: a @discordjs/rest client sends one request per bucket at a time |
| `!1hmsg` for edits of messages older than an hour | open question, see above |
| Get scope from correct header, Improve parsing of headers | fixed code davfsa itself had changed; upstream reads both headers correctly |

## Not applicable to upstream

These fix code a fork itself introduced.

- Unbuffered signal channel: upstream already buffers it (davfsa regressed).
- Interaction callbacks merged into one queue: upstream keeps the interaction id (davfsa merged, then reverted).
- Callback local routing matching no path: the routing was added by davfsa.
- Header scope read from `x-ratelimit-global`: an intermediate davfsa state.
- Webhook tokens stripped from interaction routes: upstream already keys them by interaction id, decoded from the token.
