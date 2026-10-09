package claudebot

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReply_PostsReplyWithLengthInstruction(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.MaxChars = 120 })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "@claudebot こんにちは", "public")
	e.claude.push(message("こんにちは!", "end_turn", 100, 20))

	require.NoError(t, e.mention("notif-1", "n1"))

	calls := e.claude.calls()
	require.Len(t, calls, 1)
	req := calls[0]
	assert.Equal(t, "/v1/messages", req.Path)
	assert.Equal(t, "sk-ant-test-key-0000", req.Header.Get("X-Api-Key"))
	assert.NotEmpty(t, req.Header.Get("Anthropic-Version"))
	assert.Equal(t, "claude-opus-5-5", req.Body["model"])
	assert.EqualValues(t, autoMaxTokens(120, "low"), req.Body["max_tokens"])
	assert.Equal(t, map[string]any{"effort": "low"}, req.Body["output_config"])
	sys := systemOf(req)
	assert.True(t, strings.HasPrefix(sys, defaultSettings().Reply.SystemPrompt), sys)
	assert.True(t, strings.HasSuffix(sys, lengthInstruction(120)), "システムプロンプトの末尾に長さの指示が付く: %q", sys)
	assert.Contains(t, sys, "120文字以内")
	assert.NotContains(t, sys, "途中で切れました")
	assert.Contains(t, promptOf(req), "@claudebot こんにちは")

	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1)
	assert.Equal(t, e.botID, creates[0].As)
	assert.Equal(t, "@alice こんにちは!", creates[0].Params["text"])
	assert.Equal(t, "n1", creates[0].Params["replyId"])
	assert.Equal(t, "home", creates[0].Params["visibility"])
	assert.Empty(t, e.api.callsTo("notes/reactions/create"), "返事だけのときはリアクションしない")
	assert.Equal(t, 1, e.usageRows())
}

func TestReply_MaxTokensOverride(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.MaxTokens = 777; s.Effort = "" })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-1", "n1"))
	calls := e.claude.calls()
	require.Len(t, calls, 1)
	assert.EqualValues(t, 777, calls[0].Body["max_tokens"])
	_, hasEffort := calls[0].Body["output_config"]
	assert.False(t, hasEffort, "effort が空なら送らない")
}

func TestReply_MaxTokensRetriesOnceThenSilent(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "長い話をして", "public")
	e.claude.push(message("途中で切れた文", "max_tokens", 10, 600), message("また切れた", "max_tokens", 10, 600))

	require.NoError(t, e.mention("notif-1", "n1"))

	calls := e.claude.calls()
	require.Len(t, calls, 2, "1回だけ呼び直す")
	assert.NotContains(t, systemOf(calls[0]), "途中で切れました")
	assert.True(t, strings.HasSuffix(systemOf(calls[1]), shorterInstruction(200)), "呼び直しでは「もっと短く」を足す")
	assert.Empty(t, e.api.callsTo("notes/create"), "打ち切られた文章は投稿しない")
	assert.Equal(t, 2, e.usageRows(), "呼び直した分も使った量に数える")
	silenced := e.events(eventSilenced)
	require.Len(t, silenced, 1)
	assert.Contains(t, silenced[0].Message, "max_tokens")
}

func TestReply_MaxTokensRetrySucceeds(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "長い話をして", "public")
	e.claude.push(message("途中で", "max_tokens", 10, 600), message("短くしました", "end_turn", 10, 20))

	require.NoError(t, e.mention("notif-1", "n1"))

	require.Len(t, e.claude.calls(), 2)
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1)
	assert.Equal(t, "@alice 短くしました", creates[0].Params["text"])
}

func TestReply_SilentOnAPIError(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Mode = "both" })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi :blobcat:", "public")
	e.api.emojis["blobcat"] = nil
	e.claude.push(apiError(404, "not_found_error", "model: claude-nope"))

	require.NoError(t, e.mention("notif-1", "n1"), "沈黙は失敗ではない (再試行させない)")

	assert.Empty(t, e.api.callsTo("notes/create"), "エラーの文面も含めて何も投稿しない")
	assert.Empty(t, e.api.callsTo("notes/reactions/create"), "リアクションもしない")
	errs := e.events(eventAPIError)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Message, "404")
	assert.Contains(t, errs[0].Message, "claude-nope")
	assert.NotContains(t, errs[0].Message, "sk-ant-test-key", "APIキーを記録に残さない")
	assert.Equal(t, 1, e.usageRows(), "失敗した呼び出しも回数に数える")
}

func TestReply_SilentWhenOverPostLimit(t *testing.T) {
	e := newEnv(t)
	e.api.maxNote = 30
	e.settings(func(s *Settings) { s.Reply.MaxChars = 20 })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	// "@alice " (7 文字) + 24 文字 = 31 文字で、投稿の上限 30 を超える。
	e.claude.push(message(strings.Repeat("あ", 24), "end_turn", 1, 1))

	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Empty(t, e.api.callsTo("notes/create"))
	require.Len(t, e.events(eventSilenced), 1)
	assert.Contains(t, e.events(eventSilenced)[0].Message, "文字数の上限")

	// ちょうど上限なら投稿する。
	e.api.notes["n2"] = note("n2", "alice", "alice", "", "hi", "public")
	e.claude.push(message(strings.Repeat("あ", 23), "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-2", "n2"))
	assert.Len(t, e.api.callsTo("notes/create"), 1)
}

func TestReply_SilentOnRefusal(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(message("", "refusal", 1, 1))
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Empty(t, e.api.callsTo("notes/create"))
	assert.Len(t, e.events(eventSilenced), 1)
}

func TestReply_SilentWithoutAPIKey(t *testing.T) {
	db := testDB(t)
	api := newFakeAPI()
	fc := &fakeClaude{}
	h := plugintestHarness(t, db, api, fc, nil)
	acc := h.SeedAccount(Name, "claudebot")
	api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")

	require.NoError(t, h.Notify(plugin.Notification{ID: "x", Type: plugin.NotificationMention, AccountID: acc.ID, NoteID: "n1"}))
	assert.Empty(t, fc.calls())
	assert.Empty(t, api.callsTo("notes/create"))
	evs, err := recentEvents(t.Context(), db, 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, eventConfig, evs[0].Kind)
}

func TestReply_VisibilityNeverWiderThanOriginal(t *testing.T) {
	cases := []struct {
		name, def, original, want string
	}{
		{"public default, public note", "public", "public", "public"},
		{"public default, home note", "public", "home", "home"},
		{"public default, followers note", "public", "followers", "followers"},
		{"home default, public note", "home", "public", "home"},
		{"followers default, public note", "followers", "public", "followers"},
		{"home default, followers note", "home", "followers", "followers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings(func(s *Settings) { s.Reply.Visibility = c.def })
			e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", c.original)
			e.claude.push(message("やあ", "end_turn", 1, 1))
			require.NoError(t, e.mention("notif-1", "n1"))
			creates := e.api.callsTo("notes/create")
			require.Len(t, creates, 1)
			assert.Equal(t, c.want, creates[0].Params["visibility"])
		})
	}
}

func TestReply_LocalOnlyStaysLocal(t *testing.T) {
	e := newEnv(t)
	n := note("n1", "alice", "alice", "", "hi", "public")
	n["localOnly"] = true
	e.api.notes["n1"] = n
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-1", "n1"))
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1)
	assert.Equal(t, true, creates[0].Params["localOnly"])
}

func TestClampVisibility_UnknownIsNarrow(t *testing.T) {
	assert.Equal(t, "followers", clampVisibility("public", "something-new"))
	assert.Equal(t, "home", clampVisibility("bogus", "public"))
}

func TestReply_ReactionEmojiChoice(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		emojis map[string][]string
		want   string
	}{
		{"same-named local emoji", "@claudebot :blobcat: よろしく", map[string][]string{"blobcat": nil}, ":blobcat:"},
		{"first existing one", ":nope: :blobcat: :party:", map[string][]string{"blobcat": nil, "party": nil}, ":blobcat:"},
		{"no emoji on this server", "@claudebot :nope: hi", map[string][]string{}, "👍"},
		{"no emoji in text", "@claudebot hi", map[string][]string{"blobcat": nil}, "👍"},
		{"remote emoji syntax is not a local name", ":blobcat@remote.example: hi", map[string][]string{"blobcat": nil}, "👍"},
		{"role-restricted emoji is skipped", ":vip: :blobcat:", map[string][]string{"vip": {"role1"}, "blobcat": nil}, ":blobcat:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings(func(s *Settings) { s.Reply.Mode = "reaction" })
			e.api.emojis = c.emojis
			e.api.notes["n1"] = note("n1", "alice", "alice", "", c.text, "public")
			require.NoError(t, e.mention("notif-1", "n1"))
			reacts := e.api.callsTo("notes/reactions/create")
			require.Len(t, reacts, 1)
			assert.Equal(t, c.want, reacts[0].Params["reaction"])
			assert.Equal(t, "n1", reacts[0].Params["noteId"])
			assert.Equal(t, e.botID, reacts[0].As)
			assert.Empty(t, e.claude.calls(), "リアクションだけなら Claude を呼ばない")
			assert.Empty(t, e.api.callsTo("notes/create"))
		})
	}
}

func TestReply_BothReactsAndReplies(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Mode = "both" })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Len(t, e.api.callsTo("notes/create"), 1)
	reacts := e.api.callsTo("notes/reactions/create")
	require.Len(t, reacts, 1)
	assert.Equal(t, "👍", reacts[0].Params["reaction"])
}

func TestReply_OffDoesNothing(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Mode = "off" })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Empty(t, e.claude.calls())
	assert.Empty(t, e.api.callsTo("notes/create"))
	assert.Empty(t, e.api.callsTo("notes/reactions/create"))
}

func TestReply_IgnoresDirectMessages(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Mode = "both" })
	e.api.notes["dm"] = note("dm", "alice", "alice", "", "@claudebot ないしょ", "specified")
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-1", "dm"))
	assert.Empty(t, e.api.callsTo("notes/show"), "通知の公開範囲が指名なら、投稿を読みにも行かない")

	// 通知に公開範囲が載っていなくても、読んだ投稿が指名なら反応しない。
	require.NoError(t, e.h.Notify(plugin.Notification{ID: "notif-2", Type: plugin.NotificationReply,
		AccountID: e.botID, UserID: "alice", NoteID: "dm"}))
	assert.Len(t, e.api.callsTo("notes/show"), 1)

	// チャットのメッセージ。
	require.NoError(t, e.h.Notify(plugin.Notification{ID: "chat-1", Type: plugin.NotificationChatMessage,
		AccountID: e.botID, UserID: "alice", ChatMessageID: "m1"}))

	assert.Empty(t, e.claude.calls())
	assert.Empty(t, e.api.callsTo("notes/create"))
	assert.Empty(t, e.api.callsTo("notes/reactions/create"))
	assert.Len(t, e.api.callsTo("notes/show"), 1, "チャットでは投稿を読みにも行かない")
}

func TestReply_TransientAPIErrorIsRetriedWithoutPosting(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply claudeReply
	}{
		{"overloaded", apiError(529, "overloaded_error", "Overloaded")},
		{"rate limited", apiError(429, "rate_limit_error", "slow down")},
		{"server error", apiError(500, "api_error", "boom")},
		{"network", claudeReply{netErr: errors.New("dial tcp: connection refused")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings(func(s *Settings) { s.Reply.Mode = "both" })
			e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
			e.claude.push(c.reply)

			err := e.mention("n-id", "n1")
			require.Error(t, err, "一時的なエラーは通知の再試行に任せる")
			assert.False(t, errors.Is(err, plugin.ErrNoRetry))
			assert.Empty(t, e.api.callsTo("notes/create"), "エラーの文面を投稿しない")
			assert.Empty(t, e.api.callsTo("notes/reactions/create"))
			assert.Len(t, e.events(eventAPIError), 1)

			// 再試行で通れば、1 回だけ投稿する。
			e.claude.push(message("やあ", "end_turn", 1, 1))
			require.NoError(t, e.mention("n-id", "n1"))
			require.NoError(t, e.mention("n-id", "n1"))
			assert.Len(t, e.api.callsTo("notes/create"), 1)
			assert.Len(t, e.api.callsTo("notes/reactions/create"), 1)
		})
	}
}

func TestReply_HostRateLimitIsRetried(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.api.createFails = []int{429}
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.Error(t, e.mention("n-id", "n1"), "本体の 429 は後でやり直す")
	require.NoError(t, e.mention("n-id", "n1"))
	assert.Len(t, e.api.callsTo("notes/create"), 2)
	assert.Len(t, e.claude.calls(), 1)
}

func TestReply_PostRejectedIsNotRetried(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.api.createFails = []int{400}
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("n-id", "n1"))
	assert.Len(t, e.events(eventPostError), 1)
}

func TestReply_IgnoresOtherNotificationTypes(t *testing.T) {
	e := newEnv(t)
	for i, typ := range []plugin.NotificationType{plugin.NotificationQuote, plugin.NotificationReaction, plugin.NotificationFollow, "somethingNew"} {
		require.NoError(t, e.h.Notify(plugin.Notification{ID: string(rune('a' + i)), Type: typ, AccountID: e.botID, NoteID: "n1"}))
	}
	assert.Empty(t, e.api.callsTo("notes/show"))
}

func TestReply_ReplyNotificationIsAnswered(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "続き", "public")
	e.claude.push(message("はい", "end_turn", 1, 1))
	require.NoError(t, e.h.Notify(plugin.Notification{ID: "r1", Type: plugin.NotificationReply, AccountID: e.botID, NoteID: "n1"}))
	assert.Len(t, e.api.callsTo("notes/create"), 1)
}

func TestReply_IgnoresBots(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Mode = "both" })
	n := note("n1", "otherbot", "otherbot", "remote.example", "@claudebot hello", "public")
	n["user"].(map[string]any)["isBot"] = true
	e.api.notes["n1"] = n
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Empty(t, e.claude.calls())
	assert.Empty(t, e.api.callsTo("notes/create"))
	assert.Empty(t, e.api.callsTo("notes/reactions/create"))
	assert.Len(t, e.events(eventSkipped), 1)
}

func TestReply_IgnoresOwnNote(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", e.botID, "claudebot", "", "@claudebot self", "public")
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Empty(t, e.claude.calls())
}

func TestReply_Audience(t *testing.T) {
	cases := []struct {
		name      string
		audience  string
		host      string
		following bool
		want      bool
	}{
		{"everyone accepts remote", "everyone", "remote.example", false, true},
		{"local accepts local", "local", "", false, true},
		{"local rejects remote", "local", "remote.example", true, false},
		{"followers accepts follower", "followers", "remote.example", true, true},
		{"followers rejects non-follower", "followers", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings(func(s *Settings) { s.Reply.Audience = c.audience })
			e.api.notes["n1"] = note("n1", "alice", "alice", c.host, "hi", "public")
			e.api.followers["alice"] = c.following
			e.claude.push(message("やあ", "end_turn", 1, 1))
			require.NoError(t, e.mention("notif-1", "n1"))
			if c.want {
				assert.Len(t, e.api.callsTo("notes/create"), 1)
				assert.Len(t, e.claude.calls(), 1)
			} else {
				assert.Empty(t, e.api.callsTo("notes/create"))
				assert.Empty(t, e.claude.calls())
				assert.Len(t, e.events(eventSkipped), 1)
			}
		})
	}
}

func TestReply_ThreadContext(t *testing.T) {
	e := newEnv(t)
	n := note("n3", "alice", "alice", "remote.example", "それで?", "public")
	n["replyId"] = "n2"
	e.api.notes["n3"] = n
	// bot が読めない投稿 (本体が中身を消したもの)。公開範囲の判定より前に
	// 落とすので、作者の名前も送らない。
	hidden := note("h", "carol", "carol", "", "", "public")
	hidden["isHidden"] = true
	cw := note("n0", "dave", "dave", "", "本文", "public")
	cw["cw"] = "ねたばれ"
	// notes/conversation は近い親から順に返す。
	e.api.conversation["n3"] = []map[string]any{
		note("n2", e.botID, "claudebot", "", "前の返事", "public"),
		hidden,
		note("n1", "alice", "alice", "remote.example", "最初の話 <b>", "public"),
		cw,
	}
	e.claude.push(message("続きです", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-1", "n3"))

	calls := e.claude.calls()
	require.Len(t, calls, 1)
	p := promptOf(calls[0])
	iCW := strings.Index(p, "ねたばれ")
	i1 := strings.Index(p, "最初の話")
	i2 := strings.Index(p, "前の返事")
	i3 := strings.Index(p, "それで?")
	require.True(t, iCW >= 0 && i1 > iCW && i2 > i1 && i3 > i2, "古い順に並ぶ: %s", p)
	assert.Contains(t, p, "あなた (@claudebot)")
	assert.Contains(t, p, "@alice@remote.example")
	assert.Contains(t, p, "&lt;b&gt;", "投稿の中のタグは区切りを壊さないよう逃がす")
	assert.NotContains(t, p, "carol", "botが読めない投稿は作者も送らない")
	conv := e.api.callsTo("notes/conversation")
	require.Len(t, conv, 1)
	assert.Equal(t, e.botID, conv[0].As, "botとして読む (botが読めるものだけ)")
}

func TestReply_ContextNotesLimit(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.ContextNotes = 2; s.Reply.MaxRoundTrips = 0 })
	n := note("n3", "alice", "alice", "", "最新", "public")
	n["replyId"] = "n2"
	e.api.notes["n3"] = n
	e.api.conversation["n3"] = []map[string]any{
		note("n2", "bob", "bob", "", "ひとつ前", "public"),
		note("n1", "bob", "bob", "", "ふたつ前", "public"),
	}
	e.claude.push(message("ok", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-1", "n3"))
	p := promptOf(e.claude.calls()[0])
	assert.Contains(t, p, "ひとつ前")
	assert.NotContains(t, p, "ふたつ前")
}

func TestReply_RoundTripCap(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.MaxRoundTrips = 2 })
	n := note("n5", "alice", "alice", "", "まだ続ける", "public")
	n["replyId"] = "n4"
	e.api.notes["n5"] = n
	e.api.conversation["n5"] = []map[string]any{
		note("n4", e.botID, "claudebot", "", "返事2", "public"),
		note("n3", "alice", "alice", "", "質問2", "public"),
		note("n2", e.botID, "claudebot", "", "返事1", "public"),
		note("n1", "alice", "alice", "", "質問1", "public"),
	}
	require.NoError(t, e.mention("notif-1", "n5"))
	assert.Empty(t, e.claude.calls())
	assert.Empty(t, e.api.callsTo("notes/create"))
	assert.Len(t, e.events(eventSkipped), 1)
	// 往復を数えるため、送る数より深く取る。
	conv := e.api.callsTo("notes/conversation")
	require.Len(t, conv, 1)
	assert.EqualValues(t, threadDepth, conv[0].Params["limit"])

	// 1 回少なければ返事をする。
	e.api.conversation["n5"] = e.api.conversation["n5"][1:]
	e.claude.push(message("ok", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-2", "n5"))
	assert.Len(t, e.api.callsTo("notes/create"), 1)
}

func TestReply_IdempotentOnDuplicateNotification(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(message("やあ", "end_turn", 1, 1), message("二度目", "end_turn", 1, 1))

	require.NoError(t, e.mention("same-id", "n1"))
	require.NoError(t, e.mention("same-id", "n1"))

	assert.Len(t, e.claude.calls(), 1)
	assert.Len(t, e.api.callsTo("notes/create"), 1)
}

func TestReply_RetryAfterPostFailureReusesText(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.api.createFails = []int{502}
	e.claude.push(message("やあ", "end_turn", 1, 1))

	require.Error(t, e.mention("n-id", "n1"), "本体の 5xx は再試行させる")
	require.NoError(t, e.mention("n-id", "n1"))

	assert.Len(t, e.claude.calls(), 1, "再試行で Claude を呼び直さない (二重に課金しない)")
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 2)
	assert.Equal(t, "@alice やあ", creates[1].Params["text"])

	// 終わった後にもう一度届いても何もしない。
	require.NoError(t, e.mention("n-id", "n1"))
	assert.Len(t, e.api.callsTo("notes/create"), 2)
}

func TestReply_StaleClaimIsRetaken(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	// 処理中のまま worker が落ちた通知。
	_, err := e.db.Exec(`INSERT INTO handled_notifications (id, status, claimed_at) VALUES ('n-id', 'processing', $1)`,
		now().Add(-time.Minute))
	require.NoError(t, err)
	require.NoError(t, e.mention("n-id", "n1"))
	assert.Empty(t, e.claude.calls(), "処理中の通知は他の worker に任せる")

	_, err = e.db.Exec(`UPDATE handled_notifications SET claimed_at = $1`, now().Add(-staleClaim-time.Minute))
	require.NoError(t, err)
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("n-id", "n1"))
	assert.Len(t, e.api.callsTo("notes/create"), 1)
}

func TestReply_DeletedNoteIsNotRetried(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.mention("notif-1", "gone"))
	assert.Empty(t, e.claude.calls())
}

func TestReply_ReactionErrors(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Mode = "reaction" })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")

	e.api.reactError = "ALREADY_REACTED"
	require.NoError(t, e.mention("a", "n1"))
	assert.Empty(t, e.events(eventReactError), "既に付けていれば成功と同じ (再配達で二重に付けようとした場合)")

	e.api.reactError = "YOU_HAVE_BEEN_BLOCKED"
	require.NoError(t, e.mention("b", "n1"), "リアクションの失敗は再試行させない")
	assert.Len(t, e.events(eventReactError), 1)
}
