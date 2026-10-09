package claudebot

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// threadWith sets up a public mention "n9" by alice whose ancestors are
// given nearest first.
func threadWith(e *env, ancestors ...map[string]any) {
	n := note("n9", "alice", "alice", "", "どう思う?", "public")
	n["replyId"] = "parent"
	e.api.notes["n9"] = n
	e.api.conversation["n9"] = ancestors
}

func TestContext_OthersFollowersOnlyNoteIsNotSent(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Visibility = "public" })
	threadWith(e,
		note("p2", e.botID, "claudebot", "", "botの限定の返事", "followers"),
		note("p1", "carol", "carol", "", "キャロルの限定の話", "followers"),
		note("p0", "dave", "dave", "", "デイブの公開の話", "public"),
	)
	e.claude.push(message("ok", "end_turn", 1, 1))
	require.NoError(t, e.mention("x", "n9"))

	p := promptOf(e.claude.calls()[0])
	assert.NotContains(t, p, "キャロルの限定の話", "話しかけた人が読めないかもしれない投稿は送らない")
	assert.NotContains(t, p, "botの限定の返事", "bot自身の投稿も同じ規則に従う")
	assert.Contains(t, p, "デイブの公開の話")
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1)
	assert.Equal(t, "public", creates[0].Params["visibility"])
	_, local := creates[0].Params["localOnly"]
	assert.False(t, local)
}

func TestContext_RequestersOwnNotesAreSentAndNarrowTheReply(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Visibility = "public" })
	own := note("p1", "alice", "alice", "", "アリスの限定の話", "followers")
	ownLocal := note("p0", "alice", "alice", "", "アリスの連合しない話", "public")
	ownLocal["localOnly"] = true
	threadWith(e, own, ownLocal)
	e.claude.push(message("ok", "end_turn", 1, 1))
	require.NoError(t, e.mention("x", "n9"))

	p := promptOf(e.claude.calls()[0])
	assert.Contains(t, p, "アリスの限定の話")
	assert.Contains(t, p, "アリスの連合しない話")
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1)
	assert.Equal(t, "followers", creates[0].Params["visibility"], "文脈に入れた投稿より広げない")
	assert.Equal(t, true, creates[0].Params["localOnly"], "連合しない投稿を文脈に入れたら、返事も連合させない")
}

func TestContext_LocalOnlyAndDirectNotesOfOthersAreNotSent(t *testing.T) {
	e := newEnv(t)
	local := note("p1", "carol", "carol", "", "キャロルの連合しない話", "public")
	local["localOnly"] = true
	threadWith(e,
		local,
		note("p0", "alice", "alice", "", "アリスの指名の話", "specified"),
	)
	e.claude.push(message("ok", "end_turn", 1, 1))
	require.NoError(t, e.mention("x", "n9"))
	p := promptOf(e.claude.calls()[0])
	assert.NotContains(t, p, "キャロルの連合しない話")
	assert.NotContains(t, p, "アリスの指名の話", "指名の投稿は本人のものでも送らない")
	_, localOnly := e.api.callsTo("notes/create")[0].Params["localOnly"]
	assert.False(t, localOnly)
}

func TestDefangMentions(t *testing.T) {
	e := newEnv(t)
	e.claude.push(message("@victim@remote.example @bob こんにちは user@example.com #tag", "end_turn", 1, 1))
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	require.NoError(t, e.mention("x", "n1"))
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1)
	text := creates[0].Params["text"].(string)
	assert.True(t, strings.HasPrefix(text, "@alice "), "宛先の前置きはメンションのまま")
	body := strings.TrimPrefix(text, "@alice ")
	assert.NotContains(t, body, "@victim", "生成した本文はメンションにしない")
	assert.NotContains(t, body, "@bob")
	assert.Contains(t, body, "@\u200bvictim@remote.example", "先頭の @ を崩せば、後ろの @ はメンションの一部として読まれない")
	assert.Contains(t, body, "#tag", "ハッシュタグはそのまま")
	assert.Contains(t, body, "user@example.com", "メールアドレスは触らない")

	assert.Equal(t, "a @\u200bb", defangMentions("a @b"))
	assert.Equal(t, "メール@ ", defangMentions("メール@ "))
}

func TestDefangMentions_OnlyWhatTheParserReadsAsMentions(t *testing.T) {
	cases := []struct{ in, want string }{
		// メンションになるものは崩す。
		{"@bob", "@\u200bbob"},
		{"hi @bob@remote.example!", "hi @\u200bbob@remote.example!"},
		{"(@bob)", "(@\u200bbob)"},
		{"_@bob", "_@\u200bbob"}, // パーサーは直前の _ を英数字として扱わない
		{"あ@bob", "あ@\u200bbob"},
		{"@_bob", "@\u200b_bob"},
		{"https://remote.example/x @bob", "https://remote.example/x @\u200bbob"},
		// メンションにならないものは触らない。
		{"foo@bar.example", "foo@bar.example"},
		{"https://remote.example/@bob", "https://remote.example/@bob"},
		{"see http://a.example/@x/y?z=@w ok", "see http://a.example/@x/y?z=@w ok"},
		{"`@bob`", "`@bob`"},
		{"```\n@bob\n```", "```\n@bob\n```"},
		{"@.bob @-bob @ bob", "@.bob @-bob @ bob"},
		{"", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, defangMentions(c.in), c.in)
	}
}

func TestScheduled_DefangsMentions(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Scheduled.Enabled = true; s.Scheduled.Times = []string{"07:00"} })
	e.claude.push(message("おはよう @someone", "end_turn", 1, 1))
	setClock(t, time.Date(2026, 10, 9, 7, 0, 0, 0, jst))
	require.NoError(t, e.h.Jobs(Plugin).Run(t, jobTick, ""))
	assert.Equal(t, "おはよう @\u200bsomeone", e.api.callsTo("notes/create")[0].Params["text"])
}

// timeoutErr is a net.Error that reports a timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestReply_TimeoutIsSilentAndNotRetried(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(claudeReply{netErr: timeoutErr{}})
	require.NoError(t, e.mention("x", "n1"), "timeout は再試行させない")
	assert.Len(t, e.claude.calls(), 1, "SDK の自動再試行もしない")
	assert.Empty(t, e.api.callsTo("notes/create"))
	evs := e.events(eventAPIError)
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0].Message, "時間内に終わらなかった")
	var stop string
	require.NoError(t, e.db.QueryRow(`SELECT stop_reason FROM usage_log`).Scan(&stop))
	assert.Equal(t, "error:timeout", stop)
}

func TestReply_NoSDKRetryOnOverload(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(apiError(529, "overloaded_error", "Overloaded"), message("二度目", "end_turn", 1, 1))
	require.Error(t, e.mention("x", "n1"))
	assert.Len(t, e.claude.calls(), 1, "SDK の中で呼び直さない (通知の再試行に任せる)")
	assert.Empty(t, e.api.callsTo("notes/create"))
}

func TestLimits_FailedAttemptsCount(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Limits.PerUserPerHour = 1 })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(apiError(400, "invalid_request_error", "bad"), message("ok", "end_turn", 1, 1))
	require.NoError(t, e.mention("a", "n1"))
	require.NoError(t, e.mention("b", "n1"))
	assert.Len(t, e.claude.calls(), 1, "失敗した呼び出しも1人あたりの回数に数える")
}

func TestReply_UsageRecordFailureIsNotRetried(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	// 読むのはそのままで、書き込みだけを失敗させる。
	_, err := e.db.Exec(`ALTER TABLE usage_log ADD CONSTRAINT test_no_insert CHECK (false) NOT VALID`)
	require.NoError(t, err)
	e.claude.push(message("やあ", "end_turn", 1, 1))
	err = e.mention("x", "n1")
	require.Error(t, err, "黙って握りつぶさない")
	assert.True(t, errors.Is(err, plugin.ErrNoRetry), "再試行して二重に課金しない")
	assert.Empty(t, e.api.callsTo("notes/create"), "上限を数えられないなら投稿しない")
}

func TestReply_UsageRecordFailureOnAPIErrorIsNotRetried(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	_, err := e.db.Exec(`ALTER TABLE usage_log ADD CONSTRAINT test_no_insert CHECK (false) NOT VALID`)
	require.NoError(t, err)
	e.claude.push(apiError(529, "overloaded_error", "Overloaded"))
	err = e.mention("x", "n1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, plugin.ErrNoRetry))
}

func TestReply_EmptyTextIsSilent(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(message("   ", "end_turn", 1, 1))
	require.NoError(t, e.mention("x", "n1"))
	assert.Empty(t, e.api.callsTo("notes/create"))
	require.Len(t, e.events(eventSilenced), 1)
	assert.Contains(t, e.events(eventSilenced)[0].Message, "空")
}

func TestComputeBudget_ExhaustedAlwaysWarns(t *testing.T) {
	b := computeBudget(BudgetSettings{MonthlyUSD: 10, WarnPercent: 0}, periodUsage{CostUSD: 10})
	assert.True(t, b.Exhausted)
	assert.True(t, b.Warning, "警告の割合が 0 でも、使い切ったら警告する")
}

func TestBoth_NoReactionUnlessReplied(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		e := newEnv(t)
		e.settings(func(s *Settings) {
			s.Reply.Mode = "both"
			s.Limits.PerUserPerHour = 1
			s.Limits.OverLimit = "message"
		})
		e.addUsage("alice", now(), "claude-opus-5-5", tokens{Input: 1})
		e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
		require.NoError(t, e.mention("x", "n1"))
		assert.Len(t, e.api.callsTo("notes/create"), 1, "断りの文は出す")
		assert.Empty(t, e.api.callsTo("notes/reactions/create"))
	})
	t.Run("post rejected", func(t *testing.T) {
		e := newEnv(t)
		e.settings(func(s *Settings) { s.Reply.Mode = "both" })
		e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
		e.api.createFails = []int{400}
		e.claude.push(message("やあ", "end_turn", 1, 1))
		require.NoError(t, e.mention("x", "n1"))
		assert.Empty(t, e.api.callsTo("notes/reactions/create"))
	})
}

func TestReaction_OnlyForMentions(t *testing.T) {
	for _, mode := range []string{"reaction", "both"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			e.settings(func(s *Settings) { s.Reply.Mode = mode })
			e.api.notes["n1"] = note("n1", "alice", "alice", "", "botへの返信", "public")
			e.claude.push(message("やあ", "end_turn", 1, 1))
			require.NoError(t, e.h.Notify(plugin.Notification{ID: "r1", Type: plugin.NotificationReply,
				AccountID: e.botID, UserID: "alice", NoteID: "n1", NoteVisibility: "public"}))
			assert.Empty(t, e.api.callsTo("notes/reactions/create"), "返信の通知にはリアクションしない")
			if mode == "both" {
				assert.Len(t, e.api.callsTo("notes/create"), 1, "返事はする")
			}
		})
	}
}

func TestLimits_PerHostPerDay(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Limits.PerHostPerDay = 2; s.Limits.PerUserPerHour = 0 })
	e.api.notes["r1"] = note("r1", "mallory1", "m1", "spam.example", "hi", "public")
	e.api.notes["r2"] = note("r2", "mallory2", "m2", "spam.example", "hi", "public")
	e.api.notes["r3"] = note("r3", "mallory3", "m3", "spam.example", "hi", "public")
	e.api.notes["o1"] = note("o1", "olive", "olive", "other.example", "hi", "public")
	e.api.notes["l1"] = note("l1", "luke", "luke", "", "hi", "public")
	for range 4 {
		e.claude.push(message("やあ", "end_turn", 1, 1))
	}
	for i, id := range []string{"r1", "r2", "r3", "o1", "l1"} {
		require.NoError(t, e.mention(string(rune('a'+i)), id))
	}
	assert.Len(t, e.claude.calls(), 4, "spam.example は 2 回まで。他のサーバーとローカルは別枠")
	var host string
	require.NoError(t, e.db.QueryRow(`SELECT host FROM usage_log WHERE user_id = 'mallory1'`).Scan(&host))
	assert.Equal(t, "spam.example", host)
	var n int
	require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM usage_log WHERE user_id = 'luke' AND host IS NULL`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestRoundTrips_CountedPerRootNotWindow(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.MaxRoundTrips = 2 })
	// alice が自分の投稿を挟み続けて、bot の返事が祖先から見えなくなっている。
	n := note("n9", "alice", "alice", "", "まだ?", "public")
	n["replyId"] = "a2"
	e.api.notes["n9"] = n
	e.api.conversation["n9"] = []map[string]any{
		note("a2", "alice", "alice", "", "...", "public"),
		note("a1", "alice", "alice", "", "...", "public"),
		note("root", "alice", "alice", "", "最初", "public"),
	}
	e.claude.push(message("1", "end_turn", 1, 1), message("2", "end_turn", 1, 1), message("3", "end_turn", 1, 1))
	require.NoError(t, e.mention("x1", "n9"))
	require.NoError(t, e.mention("x2", "n9"))
	require.NoError(t, e.mention("x3", "n9"))
	assert.Len(t, e.api.callsTo("notes/create"), 2, "同じ根のスレッドでは 2 回まで")
	var root string
	require.NoError(t, e.db.QueryRow(`SELECT root_id FROM bot_replies WHERE notification_id = 'x1'`).Scan(&root))
	assert.Equal(t, "root", root)
	assert.Len(t, e.events(eventSkipped), 1)
}

func TestSettings_EffortCompatibility(t *testing.T) {
	cases := []struct {
		model, effort string
		ok            bool
	}{
		{"claude-haiku-4-5", "low", false},
		{"claude-haiku-4-5-20251001", "low", false},
		{"claude-haiku-4-5", "", true},
		{"claude-sonnet-4-5", "high", false},
		{"claude-opus-4-5", "high", true},
		{"claude-opus-4-5", "xhigh", false},
		{"claude-opus-4-5-20251101", "max", false},
		{"claude-opus-4-6", "max", true},
		{"claude-opus-4-6", "xhigh", false},
		{"claude-sonnet-4-6", "xhigh", false},
		{"claude-opus-5-5", "xhigh", true},
		{"claude-something-new", "max", true},
	}
	for _, c := range cases {
		s := defaultSettings()
		s.Model, s.Effort = c.model, c.effort
		_, err := s.validate(3000)
		assert.Equal(t, c.ok, err == nil, "%s %s: %v", c.model, c.effort, err)
	}
	s := defaultSettings()
	s.Model = "claude-something-new"
	w, err := s.validate(3000)
	require.NoError(t, err)
	require.Len(t, w, 1, "表に無いモデルは警告する")
	s.PriceOverrides = []PriceOverride{{Model: "claude-something-new", Price: Price{Input: 1}}}
	w, err = s.validate(3000)
	require.NoError(t, err)
	assert.Empty(t, w, "上書きで単価があれば警告しない")
	s = defaultSettings()
	w, err = s.validate(3000)
	require.NoError(t, err)
	assert.Empty(t, w)
}

func TestRoutes_SaveReturnsWarningsAndCanEdit(t *testing.T) {
	e := newEnv(t)
	r := e.h.Routes(Plugin)
	s := defaultSettings()
	s.Model = "claude-brand-new"
	res, err := r.Call(t, "POST /admin/settings", plugintest.Request{Administrator: true, Body: asJSON(t, map[string]any{"settings": s})})
	require.NoError(t, err)
	st := res.(*stateResponse)
	assert.True(t, st.CanEdit)
	assert.Len(t, st.Warnings, 1)

	res, err = r.Call(t, "POST /admin/state", plugintest.Request{Moderator: true})
	require.NoError(t, err)
	assert.False(t, res.(*stateResponse).CanEdit, "モデレーターには保存させない")

	s.Model = "claude-haiku-4-5"
	_, err = r.Call(t, "POST /admin/settings", plugintest.Request{Administrator: true, Body: asJSON(t, map[string]any{"settings": s})})
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err), "effort に対応しないモデル")
}

func TestPricer_OverrideAppliesToAliasAndDatedID(t *testing.T) {
	p := newPricer([]PriceOverride{{Model: "claude-sonnet-4-5-20250929", Price: Price{Input: 9}}})
	got, ok := p.lookup("claude-sonnet-4-5", 1)
	require.True(t, ok)
	assert.Equal(t, 9.0, got.Input)
	got, _ = p.lookup("claude-sonnet-4-5-20991231", 1)
	assert.Equal(t, 9.0, got.Input, "別の日付の ID も同じモデル")

	p = newPricer([]PriceOverride{{Model: "claude-haiku-4-5", Price: Price{Input: 8}}})
	got, _ = p.lookup("claude-haiku-4-5-20251001", 1)
	assert.Equal(t, 8.0, got.Input)

	assert.Equal(t, "claude-opus-4-6-20260101", canonicalModel("claude-opus-4-6-20260101"))

	s := defaultSettings()
	s.PriceOverrides = []PriceOverride{{Model: "claude-haiku-4-5"}, {Model: "claude-haiku-4-5-20251001"}}
	_, err := s.validate(3000)
	assert.Error(t, err, "別名と日付付きの ID の二重の上書きは断る")
}

func TestReply_RetryKeepsScopeOfDraft(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Visibility = "public" })
	own := note("p1", "alice", "alice", "", "アリスの限定の話", "followers")
	own["localOnly"] = true
	threadWith(e, own)
	e.api.createFails = []int{502}
	e.claude.push(message("ok", "end_turn", 1, 1))
	require.Error(t, e.mention("x", "n9"))
	first := e.api.callsTo("notes/create")
	require.Len(t, first, 1)
	assert.Equal(t, "followers", first[0].Params["visibility"])

	// 再試行の前に、文脈にした投稿が消えた (見えなくなった)。
	e.api.conversation["n9"] = nil
	require.NoError(t, e.mention("x", "n9"))
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 2)
	assert.Equal(t, "followers", creates[1].Params["visibility"], "本文を書いたときの公開範囲より広げない")
	assert.Equal(t, true, creates[1].Params["localOnly"])
	assert.Len(t, e.claude.calls(), 1)
}

func TestNarrower(t *testing.T) {
	assert.Equal(t, replyScope{Visibility: "home"}, narrower(replyScope{Visibility: "home"}, replyScope{}), "記録が無ければそのまま")
	assert.Equal(t, replyScope{Visibility: "followers", LocalOnly: true},
		narrower(replyScope{Visibility: "public"}, replyScope{Visibility: "followers", LocalOnly: true}))
	assert.Equal(t, replyScope{Visibility: "followers"}, narrower(replyScope{Visibility: "followers"}, replyScope{Visibility: "public"}))
}
