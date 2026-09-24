package lib

import (
	"encoding/base64"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MajorUnknown = "unk"
	MajorChannels = "channels"
	MajorGuilds = "guilds"
	MajorWebhooks = "webhooks"
	MajorInvites = "invites"
	MajorInteractions = "interactions"
)

func IsSnowflake(str string) bool {
	l := len(str)
	if l < 17 || l > 20 {
		return false
	}
	for _, d := range str {
		if d < '0' || d > '9' {
			return false
		}
	}
	return true
}

func IsNumericInput(str string) bool {
	for _, d := range str {
		if d < '0' || d > '9' {
			return false
		}
	}
	return true
}

func GetMetricsPath(route string) string {
	route = GetOptimisticBucketPath(route, "")
	var path = ""
	parts := strings.Split(route, "/")

	if strings.HasPrefix(route, "/invite/!") {
		return "/invite/!"
	}

	for _, part := range parts {
		if part == "" { continue }
		if IsNumericInput(part) {
			path += "/!"
		} else {
			path += "/" + part
		}
	}

	if !utf8.ValidString(path) {
		logger.Warn("Non utf-8 path detected, Prometheus only supports utf-8, invalid runes will be replaced with @ in metrics. Path: " + path)
		path = strings.ToValidUTF8(path, "@")
	}

	return path
}

func GetOptimisticBucketPath(url string, method string) string {
	bucket := strings.Builder{}
	bucket.WriteByte('/')
	cleanUrl := strings.SplitN(url, "?", 2)[0]
	if strings.HasPrefix(cleanUrl, "/api/v") {
		cleanUrl = strings.ReplaceAll(cleanUrl, "/api/v", "")
		l := len(cleanUrl)
		i := strings.Index(cleanUrl, "/")
		cleanUrl = cleanUrl[i+1:l]
	} else {
		// Handle unversioned endpoints
		cleanUrl = strings.ReplaceAll(cleanUrl, "/api/", "")
	}

	parts := strings.Split(cleanUrl, "/")
	numParts := len(parts)

	if numParts <= 1 {
		return cleanUrl
	}

	currMajor := MajorUnknown
	// ! stands for any replaceable id
	switch parts[0] {
	case MajorChannels:
		// Every channel keeps its own queue, /channels/{id} itself included.
		// Upstream merged that route across all channels on the grounds that
		// Discord returns the same bucket for every id. It does, but the
		// bucket names the rule, not the counter: channel_id is a major
		// parameter, so each channel counts separately. Merged, locking a
		// hundred channels during a raid went through one queue, and one
		// channel running out of budget put all the others to sleep.
		bucket.WriteString(MajorChannels)
		bucket.WriteByte('/')
		bucket.WriteString(parts[1])
		currMajor = MajorChannels
	case MajorInvites:
		bucket.WriteString(MajorInvites)
		bucket.WriteString("/!")
		currMajor = MajorInvites
	case MajorGuilds:
		// guild_id is a major parameter, so /guilds/{id}/channels counts per
		// guild like everything else under it. Upstream merged it across
		// guilds for the same reason it merged /channels/{id}.
		fallthrough
	case MajorInteractions:
		if numParts == 4 && parts[3] == "callback" {
			return "/" + MajorInteractions + "/" + parts[1] + "/!/callback"
		}
		fallthrough
	case MajorWebhooks:
		fallthrough
	default:
		bucket.WriteString(parts[0])
		bucket.WriteByte('/')
		bucket.WriteString(parts[1])
		currMajor = parts[0]
	}

	if numParts == 2 {
		return bucket.String()
	}

	// At this point, the major + id part is already accounted for
	// In this loop, we only need to strip all remaining snowflakes, emoji names and webhook tokens(optional)
	tail := parts[2:]
	for idx, part := range tail {
		if IsSnowflake(part) {
			// Deleting a message older than 14 days, or newer than 10 seconds,
			// falls under its own bucket. Upstream compared parts[idx-1], an
			// index into the whole path, while looping over its tail: it read
			// the wrong segment and the rule never fired. It also wrote nothing
			// for a message in between, which would have merged the delete
			// into the queue of GET and POST on the channel's messages.
			if currMajor == MajorChannels && method == "DELETE" && idx == len(tail)-1 && idx > 0 && tail[idx-1] == "messages" {
				createdAt, _ := GetSnowflakeCreatedAt(part)
				switch {
				case createdAt.Before(time.Now().Add(-14 * 24 * time.Hour)):
					bucket.WriteString("/!14dmsg")
				case createdAt.After(time.Now().Add(-10 * time.Second)):
					bucket.WriteString("/!10smsg")
				default:
					bucket.WriteString("/!")
				}
				continue
			}
			bucket.WriteString("/!")
		} else {
			if currMajor == MajorChannels && part == "reactions" {
				// reaction put/delete fall under a different bucket from other reaction endpoints
				if method == "PUT" || method == "DELETE" {
					bucket.WriteString("/reactions/!modify")
					break
				}
				//All other reaction endpoints falls under the same bucket, so it's irrelevant if the user
				//is passing userid, emoji, etc.
				bucket.WriteString("/reactions/!/!")
				//Reactions can only be followed by emoji/userid combo, since we don't care, break
				break
			}

			// Strip webhook tokens, or extract interaction ID
			if len(part) >= 64 {
				// aW50ZXJhY3Rpb246 is base64 for "interaction:"
				if !strings.HasPrefix(part, "aW50ZXJhY3Rpb246") {
					bucket.WriteString("/!")
					continue
				}

				var interactionId string

				// fix padding
				if i := len(part) % 4; i != 0 {
					part += strings.Repeat("=", 4-i)
				}

				decodedPart, err := base64.StdEncoding.DecodeString(part)
				if err != nil {
					interactionId = "Unknown"
				} else {
					interactionId = strings.Split(string(decodedPart), ":")[1]
				}
			
				bucket.WriteByte('/')
				bucket.WriteString(interactionId)
				continue
			}


			// Strip webhook tokens and interaction tokens
			if (currMajor == MajorWebhooks || currMajor == MajorInteractions) && len(part) >= 64 {
				bucket.WriteString("/!")
				continue
			}
			bucket.WriteByte('/')
			bucket.WriteString(part)
		}
	}

	return bucket.String()
}