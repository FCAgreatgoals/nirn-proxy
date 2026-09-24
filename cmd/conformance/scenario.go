package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// administrator is the ADMINISTRATOR permission bit.
const administrator = 1 << 3

// tinyPNG is a 1x1 transparent PNG, enough to create an emoji.
const tinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// keycap is the #️⃣ emoji, percent-encoded as a client sends it. Upstream nirn
// truncated reactions with it, which is why it is exercised here.
const keycap = "%23%EF%B8%8F%E2%83%A3"

type object struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
	Code  string `json:"code"`
}

type scenario struct {
	c         *client
	guild     string
	users     []string
	marker    string
	allowKick bool
	allowBan  bool

	botID, appID  string
	rejoinChannel string

	roles    []string
	category string
	text     string
	text2    string
	voice    string
	threads  []string
	messages []string
	webhook  object

	failures []string
	skipped  []string
}

// errSkip marks a step that could not run because an earlier one failed.
var errSkip = errors.New("skipped")

// step runs one scenario step and records its outcome. Steps keep going after
// a failure: one broken route should not hide the state of all the others.
func (s *scenario) step(name string, fn func() error) {
	switch err := fn(); {
	case err == nil:
	case errors.Is(err, errSkip):
		s.skipped = append(s.skipped, name)
	default:
		s.failures = append(s.failures, fmt.Sprintf("%s: %v", name, err))
	}
}

func (s *scenario) need(values ...string) error {
	for _, v := range values {
		if v == "" {
			return errSkip
		}
	}
	return nil
}

func (s *scenario) do(step, method, route, path string, body, out any) error {
	return s.c.do(call{step: step, method: method, route: route, path: path, body: body}, out)
}

// pair sends the same request twice in a row, the second without spacing.
// Two consecutive requests on one bucket are what tells a token bucket from a
// fixed window, and two requests stay far below any limit.
func (s *scenario) pair(step, method, route, path string, body any) error {
	if err := s.do(step+" (1/2)", method, route, path, body, nil); err != nil {
		return err
	}
	return s.c.do(call{step: step + " (2/2)", method: method, route: route, path: path, body: body, immediate: true}, nil)
}

func (s *scenario) run() error {
	if err := s.preflight(); err != nil {
		return err
	}
	defer s.cleanup()

	s.setup()
	s.exerciseMessages()
	s.exerciseChannel()
	s.exerciseInvites()
	s.exerciseWebhooks()
	s.exerciseGuildReads()
	s.exerciseMembers()
	s.exerciseAutomod()
	s.exerciseEmojis()
	s.exerciseCommands()
	s.moderate()
	return nil
}

// preflight checks the guild is a test guild the bot administers, and refuses
// to go further otherwise. It is the only step that can stop the run.
func (s *scenario) preflight() error {
	var me object
	if err := s.do("identify the bot", "GET", "/users/@me", "/users/@me", nil, &me); err != nil {
		return err
	}
	s.botID = me.ID
	var app object
	if err := s.do("identify the application", "GET", "/applications/@me", "/applications/@me", nil, &app); err != nil {
		return err
	}
	s.appID = app.ID
	s.step("read gateway", func() error { return s.do("read gateway", "GET", "/gateway/bot", "/gateway/bot", nil, nil) })

	var guild object
	if err := s.do("read guild", "GET", "/guilds/{guild_id}", "/guilds/"+s.guild, nil, &guild); err != nil {
		return err
	}
	if s.marker == "" || !strings.Contains(guild.Name, s.marker) {
		return fmt.Errorf("guild %q does not carry the marker %q in its name: refusing to touch it", guild.Name, s.marker)
	}

	var roles []struct {
		ID          string `json:"id"`
		Permissions string `json:"permissions"`
	}
	if err := s.do("read roles", "GET", "/guilds/{guild_id}/roles", "/guilds/"+s.guild+"/roles", nil, &roles); err != nil {
		return err
	}
	var bot struct {
		Roles []string `json:"roles"`
	}
	if err := s.do("read bot member", "GET", "/guilds/{guild_id}/members/{user_id}", "/guilds/"+s.guild+"/members/"+s.botID, nil, &bot); err != nil {
		return err
	}
	if !hasAdministrator(roles, append(bot.Roles, s.guild)) {
		return errors.New("the bot is not an administrator of the guild")
	}

	for i, user := range s.users {
		name := fmt.Sprintf("read test user %d", i+1)
		if err := s.do(name, "GET", "/guilds/{guild_id}/members/{user_id}", "/guilds/"+s.guild+"/members/"+user, nil, nil); err != nil {
			return fmt.Errorf("test user %d (%s) is not a member of the guild: %w", i+1, user, err)
		}
	}

	var channels []struct {
		ID   string `json:"id"`
		Type int    `json:"type"`
	}
	if err := s.do("list channels", "GET", "/guilds/{guild_id}/channels", "/guilds/"+s.guild+"/channels", nil, &channels); err != nil {
		return err
	}
	for _, ch := range channels {
		if ch.Type == 0 {
			// A channel the engine did not create, so the invite it prints for
			// expelled test users survives the cleanup.
			s.rejoinChannel = ch.ID
			break
		}
	}
	return nil
}

func hasAdministrator(roles []struct {
	ID          string `json:"id"`
	Permissions string `json:"permissions"`
}, held []string) bool {
	for _, role := range roles {
		for _, id := range held {
			if role.ID != id {
				continue
			}
			if perms, err := strconv.ParseUint(role.Permissions, 10, 64); err == nil && perms&administrator != 0 {
				return true
			}
		}
	}
	return false
}

// setup builds what the scenario works on: roles, a category, channels and a
// permission overwrite. Everything is named after the marker.
func (s *scenario) setup() {
	g := s.guild
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("create role %d", i)
		s.step(name, func() error {
			var role object
			err := s.do(name, "POST", "/guilds/{guild_id}/roles", "/guilds/"+g+"/roles", map[string]any{"name": fmt.Sprintf("%s-role-%d", s.marker, i)}, &role)
			if err == nil {
				s.roles = append(s.roles, role.ID)
			}
			return err
		})
	}
	s.step("edit role", func() error {
		if len(s.roles) == 0 {
			return errSkip
		}
		return s.do("edit role", "PATCH", "/guilds/{guild_id}/roles/{role_id}", "/guilds/"+g+"/roles/"+s.roles[0], map[string]any{"mentionable": true}, nil)
	})

	create := func(step string, body map[string]any, into *string) {
		s.step(step, func() error {
			var ch object
			err := s.do(step, "POST", "/guilds/{guild_id}/channels", "/guilds/"+g+"/channels", body, &ch)
			*into = ch.ID
			return err
		})
	}
	create("create category", map[string]any{"name": s.marker, "type": 4}, &s.category)
	create("create text channel", map[string]any{"name": s.marker + "-text", "type": 0, "parent_id": nilIfEmpty(s.category)}, &s.text)
	create("create second text channel", map[string]any{"name": s.marker + "-text-2", "type": 0, "parent_id": nilIfEmpty(s.category)}, &s.text2)
	create("create voice channel", map[string]any{"name": s.marker + "-voice", "type": 2, "parent_id": nilIfEmpty(s.category)}, &s.voice)

	s.step("set permission overwrite", func() error {
		if len(s.roles) == 0 || s.text2 == "" {
			return errSkip
		}
		return s.do("set permission overwrite", "PUT", "/channels/{channel_id}/permissions/{overwrite_id}", "/channels/"+s.text2+"/permissions/"+s.roles[0], map[string]any{"type": 0, "allow": "1024", "deny": "0"}, nil)
	})
	s.step("remove permission overwrite", func() error {
		if len(s.roles) == 0 || s.text2 == "" {
			return errSkip
		}
		return s.do("remove permission overwrite", "DELETE", "/channels/{channel_id}/permissions/{overwrite_id}", "/channels/"+s.text2+"/permissions/"+s.roles[0], nil, nil)
	})
}

func nilIfEmpty(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func (s *scenario) exerciseMessages() {
	ch := s.text
	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("send message %d", i)
		s.step(name, func() error {
			if err := s.need(ch); err != nil {
				return err
			}
			var m object
			err := s.do(name, "POST", "/channels/{channel_id}/messages", "/channels/"+ch+"/messages", map[string]any{"content": fmt.Sprintf("nirn conformance message %d", i)}, &m)
			if err == nil {
				s.messages = append(s.messages, m.ID)
			}
			return err
		})
	}
	msg := func(i int) string {
		if i < len(s.messages) {
			return s.messages[i]
		}
		return ""
	}

	s.step("list messages", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		return s.pair("list messages", "GET", "/channels/{channel_id}/messages", "/channels/"+ch+"/messages?limit=10", nil)
	})
	s.step("read message", func() error {
		if err := s.need(ch, msg(0)); err != nil {
			return err
		}
		return s.do("read message", "GET", "/channels/{channel_id}/messages/{message_id}", "/channels/"+ch+"/messages/"+msg(0), nil, nil)
	})
	s.step("edit message", func() error {
		if err := s.need(ch, msg(0)); err != nil {
			return err
		}
		return s.do("edit message", "PATCH", "/channels/{channel_id}/messages/{message_id}", "/channels/"+ch+"/messages/"+msg(0), map[string]any{"content": "nirn conformance message 1, edited"}, nil)
	})

	reactions := "/channels/" + ch + "/messages/" + msg(0) + "/reactions/"
	for _, r := range []struct{ step, emoji string }{{"react", url.PathEscape("👍")}, {"react with a keycap emoji", keycap}} {
		s.step(r.step, func() error {
			if err := s.need(ch, msg(0)); err != nil {
				return err
			}
			return s.do(r.step, "PUT", "/channels/{channel_id}/messages/{message_id}/reactions/{emoji}/@me", reactions+r.emoji+"/@me", nil, nil)
		})
	}
	s.step("list reactions", func() error {
		if err := s.need(ch, msg(0)); err != nil {
			return err
		}
		return s.do("list reactions", "GET", "/channels/{channel_id}/messages/{message_id}/reactions/{emoji}", reactions+url.PathEscape("👍"), nil, nil)
	})
	s.step("remove reaction", func() error {
		if err := s.need(ch, msg(0)); err != nil {
			return err
		}
		return s.do("remove reaction", "DELETE", "/channels/{channel_id}/messages/{message_id}/reactions/{emoji}/@me", reactions+keycap+"/@me", nil, nil)
	})

	pin := "/channels/" + ch + "/messages/pins/"
	s.step("pin message", func() error {
		if err := s.need(ch, msg(1)); err != nil {
			return err
		}
		return s.do("pin message", "PUT", "/channels/{channel_id}/messages/pins/{message_id}", pin+msg(1), nil, nil)
	})
	s.step("list pins", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		return s.do("list pins", "GET", "/channels/{channel_id}/messages/pins", "/channels/"+ch+"/messages/pins", nil, nil)
	})
	s.step("unpin message", func() error {
		if err := s.need(ch, msg(1)); err != nil {
			return err
		}
		return s.do("unpin message", "DELETE", "/channels/{channel_id}/messages/pins/{message_id}", pin+msg(1), nil, nil)
	})

	s.step("start thread from message", func() error {
		if err := s.need(ch, msg(2)); err != nil {
			return err
		}
		var t object
		err := s.do("start thread from message", "POST", "/channels/{channel_id}/messages/{message_id}/threads", "/channels/"+ch+"/messages/"+msg(2)+"/threads", map[string]any{"name": s.marker + "-thread"}, &t)
		if err == nil {
			s.threads = append(s.threads, t.ID)
		}
		return err
	})
	s.step("start thread", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		var t object
		err := s.do("start thread", "POST", "/channels/{channel_id}/threads", "/channels/"+ch+"/threads", map[string]any{"name": s.marker + "-thread-2", "type": 11}, &t)
		if err == nil {
			s.threads = append(s.threads, t.ID)
		}
		return err
	})
	s.step("join and leave thread", func() error {
		if len(s.threads) == 0 {
			return errSkip
		}
		t := s.threads[len(s.threads)-1]
		if err := s.do("join thread", "PUT", "/channels/{channel_id}/thread-members/@me", "/channels/"+t+"/thread-members/@me", nil, nil); err != nil {
			return err
		}
		return s.do("leave thread", "DELETE", "/channels/{channel_id}/thread-members/@me", "/channels/"+t+"/thread-members/@me", nil, nil)
	})
	s.step("list active threads", func() error {
		return s.do("list active threads", "GET", "/guilds/{guild_id}/threads/active", "/guilds/"+s.guild+"/threads/active", nil, nil)
	})

	s.step("delete message", func() error {
		if err := s.need(ch, msg(5)); err != nil {
			return err
		}
		return s.do("delete message", "DELETE", "/channels/{channel_id}/messages/{message_id}", "/channels/"+ch+"/messages/"+msg(5), nil, nil)
	})
	s.step("bulk delete messages", func() error {
		if err := s.need(ch, msg(3), msg(4)); err != nil {
			return err
		}
		return s.do("bulk delete messages", "POST", "/channels/{channel_id}/messages/bulk-delete", "/channels/"+ch+"/messages/bulk-delete", map[string]any{"messages": []string{msg(3), msg(4)}}, nil)
	})
}

// exerciseChannel edits the text channel: slowmode twice in a row, to observe
// the channel bucket's model, then a single rename, which nirn queues apart.
// One rename only: renames sit behind a sublimit of a few per ten minutes.
func (s *scenario) exerciseChannel() {
	ch := s.text
	s.step("read channel", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		return s.do("read channel", "GET", "/channels/{channel_id}", "/channels/"+ch, nil, nil)
	})
	s.step("toggle slowmode", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		if err := s.do("enable slowmode", "PATCH", "/channels/{channel_id}", "/channels/"+ch, map[string]any{"rate_limit_per_user": 10}, nil); err != nil {
			return err
		}
		return s.c.do(call{step: "disable slowmode", method: "PATCH", route: "/channels/{channel_id}", path: "/channels/" + ch, body: map[string]any{"rate_limit_per_user": 0}, immediate: true}, nil)
	})
	s.step("rename channel", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		return s.do("rename channel", "PATCH", "/channels/{channel_id}", "/channels/"+ch, map[string]any{"name": s.marker + "-renamed"}, nil)
	})
}

func (s *scenario) exerciseInvites() {
	ch := s.text
	var invite object
	s.step("create invite", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		return s.do("create invite", "POST", "/channels/{channel_id}/invites", "/channels/"+ch+"/invites", map[string]any{"max_age": 3600, "unique": true}, &invite)
	})
	s.step("list channel invites", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		return s.do("list channel invites", "GET", "/channels/{channel_id}/invites", "/channels/"+ch+"/invites", nil, nil)
	})
	s.step("list guild invites", func() error {
		return s.do("list guild invites", "GET", "/guilds/{guild_id}/invites", "/guilds/"+s.guild+"/invites", nil, nil)
	})
	s.step("read invite", func() error {
		if err := s.need(invite.Code); err != nil {
			return err
		}
		return s.do("read invite", "GET", "/invites/{code}", "/invites/"+invite.Code, nil, nil)
	})
	s.step("delete invite", func() error {
		if err := s.need(invite.Code); err != nil {
			return err
		}
		return s.do("delete invite", "DELETE", "/invites/{code}", "/invites/"+invite.Code, nil, nil)
	})
}

func (s *scenario) exerciseWebhooks() {
	ch := s.text
	s.step("create webhook", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		return s.do("create webhook", "POST", "/channels/{channel_id}/webhooks", "/channels/"+ch+"/webhooks", map[string]any{"name": s.marker}, &s.webhook)
	})
	s.step("read webhook", func() error {
		if err := s.need(s.webhook.ID); err != nil {
			return err
		}
		return s.do("read webhook", "GET", "/webhooks/{webhook_id}", "/webhooks/"+s.webhook.ID, nil, nil)
	})
	s.step("list channel webhooks", func() error {
		if err := s.need(ch); err != nil {
			return err
		}
		return s.do("list channel webhooks", "GET", "/channels/{channel_id}/webhooks", "/channels/"+ch+"/webhooks", nil, nil)
	})
	s.step("list guild webhooks", func() error {
		return s.do("list guild webhooks", "GET", "/guilds/{guild_id}/webhooks", "/guilds/"+s.guild+"/webhooks", nil, nil)
	})

	base := "/webhooks/" + s.webhook.ID + "/" + s.webhook.Token
	var sent object
	s.step("execute webhook", func() error {
		if err := s.need(s.webhook.ID, s.webhook.Token); err != nil {
			return err
		}
		return s.do("execute webhook", "POST", "/webhooks/{webhook_id}/{webhook_token}", base+"?wait=true", map[string]any{"content": "nirn conformance webhook message"}, &sent)
	})
	msgRoute := "/webhooks/{webhook_id}/{webhook_token}/messages/{message_id}"
	for _, m := range []struct {
		step, method string
		body         any
	}{
		{"read webhook message", "GET", nil},
		{"edit webhook message", "PATCH", map[string]any{"content": "nirn conformance webhook message, edited"}},
		{"delete webhook message", "DELETE", nil},
	} {
		s.step(m.step, func() error {
			if err := s.need(sent.ID); err != nil {
				return err
			}
			return s.do(m.step, m.method, msgRoute, base+"/messages/"+sent.ID, m.body, nil)
		})
	}
}

func (s *scenario) exerciseGuildReads() {
	g := "/guilds/" + s.guild
	reads := []struct{ step, route, path string }{
		{"list members", "/guilds/{guild_id}/members", g + "/members?limit=10"},
		{"search members", "/guilds/{guild_id}/members/search", g + "/members/search?query=" + url.QueryEscape("t") + "&limit=5"},
		{"list bans", "/guilds/{guild_id}/bans", g + "/bans?limit=10"},
		{"read audit log", "/guilds/{guild_id}/audit-logs", g + "/audit-logs?limit=5"},
		{"read onboarding", "/guilds/{guild_id}/onboarding", g + "/onboarding"},
		{"list roles", "/guilds/{guild_id}/roles", g + "/roles"},
	}
	for _, r := range reads {
		s.step(r.step, func() error { return s.do(r.step, "GET", r.route, r.path, nil, nil) })
	}
	s.step("read user", func() error {
		if len(s.users) == 0 {
			return errSkip
		}
		return s.do("read user", "GET", "/users/{user_id}", "/users/"+s.users[0], nil, nil)
	})
}

// exerciseMembers adds then removes a role on every test user back to back:
// the two share one bucket, which the pair makes visible.
func (s *scenario) exerciseMembers() {
	g := "/guilds/" + s.guild + "/members/"
	for i, user := range s.users {
		name := fmt.Sprintf("toggle role on test user %d", i+1)
		s.step(name, func() error {
			if len(s.roles) == 0 {
				return errSkip
			}
			path := g + user + "/roles/" + s.roles[0]
			route := "/guilds/{guild_id}/members/{user_id}/roles/{role_id}"
			if err := s.do(name+" (add)", "PUT", route, path, nil, nil); err != nil {
				return err
			}
			return s.c.do(call{step: name + " (remove)", method: "DELETE", route: route, path: path, immediate: true}, nil)
		})
	}
	s.step("nickname test user 1", func() error {
		if len(s.users) == 0 {
			return errSkip
		}
		route := "/guilds/{guild_id}/members/{user_id}"
		if err := s.do("set nickname", "PATCH", route, g+s.users[0], map[string]any{"nick": s.marker}, nil); err != nil {
			return err
		}
		return s.do("clear nickname", "PATCH", route, g+s.users[0], map[string]any{"nick": nil}, nil)
	})
	s.step("nickname the bot", func() error {
		route := "/guilds/{guild_id}/members/@me"
		if err := s.do("set bot nickname", "PATCH", route, g+"@me", map[string]any{"nick": s.marker}, nil); err != nil {
			return err
		}
		return s.do("clear bot nickname", "PATCH", route, g+"@me", map[string]any{"nick": nil}, nil)
	})
}

func (s *scenario) exerciseAutomod() {
	base := "/guilds/" + s.guild + "/auto-moderation/rules"
	var rule object
	s.step("create automod rule", func() error {
		return s.do("create automod rule", "POST", "/guilds/{guild_id}/auto-moderation/rules", base, map[string]any{
			"name": s.marker, "event_type": 1, "trigger_type": 1, "enabled": false,
			"trigger_metadata": map[string]any{"keyword_filter": []string{"nirnconformancekeyword"}},
			"actions":          []map[string]any{{"type": 1}},
		}, &rule)
	})
	s.step("list automod rules", func() error {
		return s.do("list automod rules", "GET", "/guilds/{guild_id}/auto-moderation/rules", base, nil, nil)
	})
	one := "/guilds/{guild_id}/auto-moderation/rules/{rule_id}"
	s.step("read automod rule", func() error {
		if err := s.need(rule.ID); err != nil {
			return err
		}
		return s.do("read automod rule", "GET", one, base+"/"+rule.ID, nil, nil)
	})
	s.step("edit automod rule", func() error {
		if err := s.need(rule.ID); err != nil {
			return err
		}
		return s.do("edit automod rule", "PATCH", one, base+"/"+rule.ID, map[string]any{"name": s.marker + "-edited"}, nil)
	})
	s.step("delete automod rule", func() error {
		if err := s.need(rule.ID); err != nil {
			return err
		}
		return s.do("delete automod rule", "DELETE", one, base+"/"+rule.ID, nil, nil)
	})
}

// exerciseEmojis covers the routes Discord warns about: emoji routes are
// limited per guild outside the usual conventions, and their headers may be
// inaccurate.
func (s *scenario) exerciseEmojis() {
	base := "/guilds/" + s.guild + "/emojis"
	var emoji object
	s.step("create emoji", func() error {
		return s.do("create emoji", "POST", "/guilds/{guild_id}/emojis", base, map[string]any{"name": "nirn_conformance", "image": tinyPNG}, &emoji)
	})
	s.step("list emojis", func() error { return s.do("list emojis", "GET", "/guilds/{guild_id}/emojis", base, nil, nil) })
	one := "/guilds/{guild_id}/emojis/{emoji_id}"
	s.step("read emoji", func() error {
		if err := s.need(emoji.ID); err != nil {
			return err
		}
		return s.do("read emoji", "GET", one, base+"/"+emoji.ID, nil, nil)
	})
	s.step("rename emoji", func() error {
		if err := s.need(emoji.ID); err != nil {
			return err
		}
		return s.do("rename emoji", "PATCH", one, base+"/"+emoji.ID, map[string]any{"name": "nirn_conformance_2"}, nil)
	})
	s.step("delete emoji", func() error {
		if err := s.need(emoji.ID); err != nil {
			return err
		}
		return s.do("delete emoji", "DELETE", one, base+"/"+emoji.ID, nil, nil)
	})
}

// exerciseCommands uses guild commands only: global ones would reach every
// guild the bot is in.
func (s *scenario) exerciseCommands() {
	base := "/applications/" + s.appID + "/guilds/" + s.guild + "/commands"
	list := "/applications/{application_id}/guilds/{guild_id}/commands"
	one := list + "/{command_id}"
	var cmd object
	s.step("list guild commands", func() error {
		if err := s.need(s.appID); err != nil {
			return err
		}
		return s.do("list guild commands", "GET", list, base, nil, nil)
	})
	s.step("create guild command", func() error {
		if err := s.need(s.appID); err != nil {
			return err
		}
		return s.do("create guild command", "POST", list, base, map[string]any{"name": "nirn-conformance", "description": "nirn conformance test command", "type": 1}, &cmd)
	})
	s.step("edit guild command", func() error {
		if err := s.need(cmd.ID); err != nil {
			return err
		}
		return s.do("edit guild command", "PATCH", one, base+"/"+cmd.ID, map[string]any{"description": "nirn conformance test command, edited"}, nil)
	})
	s.step("delete guild command", func() error {
		if err := s.need(cmd.ID); err != nil {
			return err
		}
		return s.do("delete guild command", "DELETE", one, base+"/"+cmd.ID, nil, nil)
	})
}

// moderate comes last, in the order that keeps the test users usable as long
// as possible: a timeout is lifted at once, and kicking or banning removes a
// user from the guild, who must then rejoin through the invite printed at the
// end. Both need an explicit flag.
func (s *scenario) moderate() {
	g := "/guilds/" + s.guild
	s.step("timeout test user 1", func() error {
		if len(s.users) < 1 {
			return errSkip
		}
		route := "/guilds/{guild_id}/members/{user_id}"
		until := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
		if err := s.do("timeout test user 1", "PATCH", route, g+"/members/"+s.users[0], map[string]any{"communication_disabled_until": until}, nil); err != nil {
			return err
		}
		return s.do("lift timeout of test user 1", "PATCH", route, g+"/members/"+s.users[0], map[string]any{"communication_disabled_until": nil}, nil)
	})
	if s.allowKick && len(s.users) >= 3 {
		s.step("kick test user 3", func() error {
			return s.do("kick test user 3", "DELETE", "/guilds/{guild_id}/members/{user_id}", g+"/members/"+s.users[2], nil, nil)
		})
	}
	if s.allowBan && len(s.users) >= 4 {
		route := "/guilds/{guild_id}/bans/{user_id}"
		path := g + "/bans/" + s.users[3]
		s.step("ban test user 4", func() error {
			return s.do("ban test user 4", "PUT", route, path, map[string]any{"delete_message_seconds": 0}, nil)
		})
		s.step("read ban of test user 4", func() error { return s.do("read ban of test user 4", "GET", route, path, nil, nil) })
		s.step("unban test user 4", func() error { return s.do("unban test user 4", "DELETE", route, path, nil, nil) })
	}
}

// cleanup removes everything the scenario created, even after failures.
func (s *scenario) cleanup() {
	if s.webhook.ID != "" {
		s.step("delete webhook", func() error {
			return s.do("delete webhook", "DELETE", "/webhooks/{webhook_id}", "/webhooks/"+s.webhook.ID, nil, nil)
		})
	}
	for i, id := range append(append([]string{}, s.threads...), s.text, s.text2, s.voice, s.category) {
		if id == "" {
			continue
		}
		name := fmt.Sprintf("delete channel %d", i+1)
		s.step(name, func() error {
			return s.do(name, "DELETE", "/channels/{channel_id}", "/channels/"+id, nil, nil)
		})
	}
	for i, id := range s.roles {
		name := fmt.Sprintf("delete role %d", i+1)
		s.step(name, func() error {
			return s.do(name, "DELETE", "/guilds/{guild_id}/roles/{role_id}", "/guilds/"+s.guild+"/roles/"+id, nil, nil)
		})
	}
}

// rejoinInvite creates an invite on a channel the engine did not create, for
// test users it kicked or banned to come back.
func (s *scenario) rejoinInvite() string {
	if s.rejoinChannel == "" || (!s.allowKick && !s.allowBan) {
		return ""
	}
	var invite object
	if err := s.do("create rejoin invite", "POST", "/channels/{channel_id}/invites", "/channels/"+s.rejoinChannel+"/invites", map[string]any{"max_age": 86400, "max_uses": 0}, &invite); err != nil {
		s.failures = append(s.failures, "create rejoin invite: "+err.Error())
		return ""
	}
	return "https://discord.gg/" + invite.Code
}
