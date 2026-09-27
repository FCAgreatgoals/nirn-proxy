package lib

import "time"

// resetMargin is added to X-RateLimit-Reset-After before sending into an
// emptied bucket again.
//
// Discord does not always reopen a bucket at the instant it announces. A
// bucketmap run against it (github.com/FCAgreatgoals/bucketmap) found it on
// buckets of one request: a request sent 5.001 s after a Reset-After of 5 was
// refused, with 0.3 s more to wait, and the reaction bucket, one request every
// quarter second, did the same. Sleeping exactly Reset-After, as upstream
// does, walks into that 429. discordgo and arikawa add a quarter second for
// the same reason; the measured lag asks for a little more.
const resetMargin = 400 * time.Millisecond
