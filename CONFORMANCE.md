# Conformance engine

`cmd/conformance` walks a Discord test guild through every route the proxy is
expected to handle, a few requests each, and reports what each route answered
and which rate limit it fell under. Run it once straight against Discord and
once through the proxy, then compare: the proxy passes when every step answers
the same and nothing reached a 429.

It is a conformance check, not a load test. A full run sends about 110
requests spread over the whole duration, ten minutes by default. A bucket that
reports nothing left is waited out before the next request on it, and a 429 is
recorded as a failure and never retried.

## What it needs

- A guild set aside for testing, whose name contains the marker
  (`nirn-conformance` by default). The run refuses to touch any other guild.
- The bot as an administrator of that guild.
- Four test users who are members of it.

## What it does, in order

1. **Preflight.** Identifies the bot and the application, checks the marker and
   the administrator permission, and that the four test users are members.
   Nothing is created before all of this holds.
2. **Setup.** Three roles, a category, two text channels and a voice channel,
   all named after the marker, and a permission overwrite.
3. **Messages.** Sending, reading, editing, reactions (including the keycap
   emoji `#️⃣`, which upstream truncated), pins, threads, deleting and bulk
   deleting.
4. **Channel.** Slowmode on and off back to back, then a single rename: renames
   sit behind a sublimit of a few per ten minutes.
5. **Invites, webhooks, guild reads, members, automod, emojis, guild commands.**
   Guild commands only: global ones would reach every guild the bot is in.
6. **Moderation, last.** A timeout, lifted at once. Kicking test user 3 and
   banning then unbanning test user 4 only run with `-allow-kick` and
   `-allow-ban`, since both take the user out of the guild. The run then prints
   an invite for them to come back.
7. **Cleanup,** always, even after failures: everything the run created is
   deleted.

Every request carries an `X-Audit-Log-Reason` naming the step, so the run is
readable in the guild's audit log.

## Running it

```sh
# Straight against Discord
go run ./cmd/conformance -token "$TOKEN" -guild 123 -users 1,2,3,4 \
  -duration 10m -allow-kick -allow-ban -report direct.json

# Through the proxy
go run ./cmd/conformance -api http://localhost:8080/api/v10 -token "$TOKEN" \
  -guild 123 -users 1,2,3,4 -duration 10m -allow-kick -allow-ban -report proxied.json

# Side by side
go run ./cmd/conformance compare direct.json proxied.json
```

Kicked and banned users have to rejoin between the two runs.

## What the report says about rate limits

Every bucket met is listed with its limit, its window, and its model:

- **token bucket**: `X-RateLimit-Reset-After` grows with each request, because it
  announces the time until the bucket is full again. The window is the limit
  times the time one token takes to come back.
- **fixed window**: the instant of the next refill stays put across requests.
- **unknown**: no two requests of the run fell within one window.

A few steps send the same request twice in a row on purpose (listing messages,
toggling slowmode, adding then removing a role): two requests on one bucket are
enough to tell the models apart. Buckets Discord shares between several routes
are flagged, and so are long windows such as role creation, two hundred and
fifty per forty-eight hours.

## Against a simulator

Pointed at a Discord simulator, directly and through the proxy with
`DISCORD_URL`, the engine exercises the whole chain without a real guild. It
found two bugs in the simulator it was first run against: bucket headers that
carried the channel id, and limits shared by every bot of the process.
