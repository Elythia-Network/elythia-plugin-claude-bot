package claudebot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selectEnv is memoryEnv with choosing on, keeping 2 exchanges.
func selectEnv(t *testing.T, mutate func(*Settings)) *env {
	t.Helper()
	return memoryEnv(t, func(s *Settings) {
		s.Memory.Select = true
		s.Memory.KeepRecent = 2
		s.Memory.SummarizeAt = 0
		if mutate != nil {
			mutate(s)
		}
	})
}

// seedAbout stores a summary of userID with n items "項目1"... of size chars.
func (e *env) seedAbout(userID string, n, size int) {
	e.t.Helper()
	ab := about{CallName: "ありす"}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		text := fmt.Sprintf("項目%d", i)
		text += strings.Repeat("あ", max(0, size-utf8Len(text)))
		ab.Items = append(ab.Items, aboutItem{ID: strconv.Itoa(i), Kind: aboutFact, Text: text, At: t0.Add(time.Duration(i) * time.Minute)})
	}
	ab.clamp()
	raw, err := json.Marshal(ab)
	require.NoError(e.t, err)
	_, err = e.db.Exec(`
INSERT INTO memories (user_id, acct, exchanges, updated_at, summary, summarized_at) VALUES ($1, '@' || $1, '[]', now(), $2, now())
ON CONFLICT (user_id) DO UPDATE SET summary = EXCLUDED.summary, summarized_at = EXCLUDED.summarized_at`, userID, string(raw))
	require.NoError(e.t, err)
}

func utf8Len(s string) int { return len([]rune(s)) }

// view is a noteView by userID with text.
func view(id, userID, text string) noteView {
	return noteView{ID: id, Text: &text, Visibility: "public", User: userLite{ID: userID, Username: userID}}
}

func modelOf(r capturedRequest) string {
	m, _ := r.Body["model"].(string)
	return m
}

func choose(t *testing.T, ids ...string) claudeReply {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"ids": ids})
	require.NoError(t, err)
	return message(string(raw), "end_turn", 100, 10)
}

func (e *env) lastSelection(userID string) *memorySelection {
	e.t.Helper()
	res, err := e.h.Routes(Plugin).Call(e.t, "POST /admin/memories/show",
		plugintest.Request{UserID: "admin", Administrator: true, Body: fmt.Sprintf(`{"userId":%q}`, userID)})
	require.NoError(e.t, err)
	return res.(*memoryResponse).LastSelection
}

func TestNewestMemory(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ab := &about{CallName: "ありす"}
	for i := 1; i <= 20; i++ {
		ab.Items = append(ab.Items, aboutItem{ID: strconv.Itoa(i), Kind: aboutFact, Text: strings.Repeat("あ", 100), At: t0.Add(time.Duration(i) * time.Minute)})
	}
	var exs []exchange
	for i := 1; i <= 5; i++ {
		exs = append(exs, exchange{NoteID: fmt.Sprintf("n%d", i)})
	}
	got := newestMemory(memoryView{About: ab, Exchanges: exs}, MemorySettings{KeepRecent: 2, PromptChars: 1500})
	require.NotNil(t, got.About)
	assert.Equal(t, "ありす", got.About.CallName)
	assert.Equal(t, "6", got.About.Items[0].ID, "項目は新しい方から予算まで")
	assert.Len(t, got.About.Items, 15)
	assert.Len(t, ab.Items, 20, "元の要約は書き換えない")
	assert.Equal(t, []string{"n4", "n5"}, noteIDs(got.Exchanges), "やり取りは新しい方から覚えておく数まで")

	none := newestMemory(memoryView{About: &about{Items: ab.Items}}, MemorySettings{KeepRecent: 2, PromptChars: 50})
	assert.Nil(t, none.About, "呼び名も入る項目も無ければ要約を渡さない")
}

func TestSelect_OffDoesNotCall(t *testing.T) {
	e := memoryEnv(t, func(s *Settings) { s.Memory.KeepRecent = 2; s.Memory.SummarizeAt = 0 })
	e.seed("alice", 5)
	e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")
	calls := e.claude.calls()
	require.Len(t, calls, 1, "OFFなら選ぶ呼び出しをしない")
	p := promptOf(calls[0])
	assert.Contains(t, p, "話4")
	assert.Contains(t, p, "話5")
	assert.NotContains(t, p, "話3\n", "新しい方から覚えておく数まで")
	assert.Nil(t, e.lastSelection("alice"), "OFFなら選んだ結果を残さない")
}

func TestSelect_FitsWithoutCalling(t *testing.T) {
	e := selectEnv(t, nil)
	e.seed("alice", 2)
	e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")
	require.Len(t, e.claude.calls(), 1, "全部が予算に収まるなら呼ばない")
	sel := e.lastSelection("alice")
	require.NotNil(t, sel)
	assert.False(t, sel.Called)
	assert.Equal(t, []string{"n1", "n2"}, sel.Exchanges)
	assert.Equal(t, "n9", sel.NoteID)
}

func TestSelect_ChoosesExchanges(t *testing.T) {
	e := selectEnv(t, nil)
	e.seed("alice", 5)
	e.claude.push(choose(t, "e2", "e9", "i1", "話3"))
	e.talk("notif-1", "n9", "alice", "", "猫の話をしよう", "public", "いいね")

	calls := e.claude.calls()
	require.Len(t, calls, 2)
	req := calls[0]
	assert.Equal(t, "claude-haiku-5-5", modelOf(req), "選ぶモデルで呼ぶ")
	assert.Equal(t, selectSystem, systemOf(req), "運営者のシステムプロンプトは使わない")
	oc, _ := req.Body["output_config"].(map[string]any)
	assert.NotContains(t, oc, "effort", "effortは送らない")
	format, _ := oc["format"].(map[string]any)
	require.NotNil(t, format)
	assert.Equal(t, "json_schema", format["type"])
	assert.EqualValues(t, selectMaxTokens, req.Body["max_tokens"])
	p := promptOf(req)
	assert.Contains(t, p, "<message>\n猫の話をしよう\n</message>")
	for i := 1; i <= 5; i++ {
		assert.Contains(t, p, fmt.Sprintf(`<exchange id="e%d"`, i), "全部のやり取りを候補にする")
	}

	reply := promptOf(calls[1])
	assert.Contains(t, reply, "話2\n", "選んだやり取りを渡す")
	assert.Contains(t, reply, "話5\n", "直近のやり取りは選ばれなくても渡す")
	for _, n := range []string{"話1\n", "話3\n", "話4\n"} {
		assert.NotContains(t, reply, n, "選ばなかったやり取りは渡さない")
	}
	assert.Less(t, strings.Index(reply, "話2"), strings.Index(reply, "話5"), "古い順に並べる")

	calls2, total := e.usageOfKind(kindMemorySelect)
	assert.Equal(t, 1, calls2)
	assert.EqualValues(t, 110, total, "選ぶ呼び出しの使った量を記録する")
	var uid string
	require.NoError(t, e.db.QueryRow(`SELECT user_id FROM usage_log WHERE kind = $1`, kindMemorySelect).Scan(&uid))
	assert.Equal(t, "alice", uid)

	sel := e.lastSelection("alice")
	require.NotNil(t, sel)
	assert.True(t, sel.Called)
	assert.Equal(t, "claude-haiku-5-5", sel.Model)
	assert.Empty(t, sel.Fallback)
	assert.Equal(t, []string{"n2", "n5"}, sel.Exchanges)
}

func TestSelect_SummaryOnlyDeleteClearsTheSelection(t *testing.T) {
	e := selectEnv(t, nil)
	e.seedAbout("alice", 20, 100)
	e.claude.push(choose(t, "i3"))
	e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")
	require.NotNil(t, e.lastSelection("alice"))
	_, err := e.h.Routes(Plugin).Call(t, "POST /admin/memories/delete",
		plugintest.Request{UserID: "admin", Administrator: true, Body: `{"userId":"alice","summaryOnly":true}`})
	require.NoError(t, err)
	assert.Nil(t, e.lastSelection("alice"), "選んだ結果には項目の本文が入るので、要約と一緒に消す")
}

func TestSelect_EmptyMemoryReplacesTheSelection(t *testing.T) {
	e := selectEnv(t, nil)
	e.seed("alice", 1)
	e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")
	require.Equal(t, []string{"n1"}, e.lastSelection("alice").Exchanges)
	_, err := e.db.Exec(`UPDATE memories SET exchanges = '[]' WHERE user_id = 'alice'`)
	require.NoError(t, err)
	e.talk("notif-2", "n10", "alice", "", "やあ", "public", "やあ")
	sel := e.lastSelection("alice")
	require.NotNil(t, sel)
	assert.Equal(t, "n10", sel.NoteID, "渡す記憶が無くても、直近の返事の選択として残す")
	assert.Empty(t, sel.Exchanges)
}

func TestSelect_OutsideTheAudienceIsNotRecorded(t *testing.T) {
	e := selectEnv(t, nil)
	remoteHost := "remote.example"
	_, err := e.db.Exec(`INSERT INTO memories (user_id, acct, exchanges, updated_at) VALUES ('bob', '@bob@remote.example', '[]', now())`)
	require.NoError(t, err)
	e.talk("notif-1", "n1", "bob", remoteHost, "やあ", "public", "やあ")
	assert.Nil(t, e.lastSelection("bob"), "覚える相手の範囲の外の人は残さない")
}

func TestSelect_KeepsAtMostKeepRecent(t *testing.T) {
	e := selectEnv(t, nil)
	e.seed("alice", 5)
	e.claude.push(choose(t, "e1", "e2", "e3", "e4"))
	e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")
	assert.Equal(t, []string{"n4", "n5"}, e.lastSelection("alice").Exchanges, "選んだ中から新しい方を覚えておく数まで")
}

func TestSelect_ChoosesItems(t *testing.T) {
	e := selectEnv(t, nil)
	e.seedAbout("alice", 20, 100)
	e.claude.push(choose(t, "i3", "i17", "3"))
	e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")

	calls := e.claude.calls()
	require.Len(t, calls, 2, "項目が予算を超えるので選ぶ")
	p := promptOf(calls[0])
	assert.Contains(t, p, `<item id="i1" kind="fact">項目1あ`)
	assert.Contains(t, p, `<item id="i20" kind="fact">`)
	reply := promptOf(calls[1])
	assert.Contains(t, reply, "呼び名: ありす", "呼び名は常に渡す")
	assert.Contains(t, reply, "- 項目3あ")
	assert.Contains(t, reply, "- 項目17あ")
	assert.NotContains(t, reply, "- 項目20あ", "選ばなかった項目は渡さない")
	sel := e.lastSelection("alice")
	require.NotNil(t, sel)
	require.Len(t, sel.Items, 2)
	assert.Equal(t, "3", sel.Items[0].ID)
	assert.True(t, strings.HasPrefix(sel.Items[0].Text, "項目3あ"), "本文も残す")
}

func TestSelect_ChosenItemsWithinTheBudget(t *testing.T) {
	e := selectEnv(t, func(s *Settings) { s.Memory.PromptChars = minMemoryPromptChars })
	e.seedAbout("alice", 10, 100)
	e.claude.push(choose(t, "i1", "i2", "i5", "i9"))
	e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")
	sel := e.lastSelection("alice")
	require.NotNil(t, sel)
	ids := []string{}
	for _, it := range sel.Items {
		ids = append(ids, it.ID)
	}
	assert.Equal(t, []string{"2", "5", "9"}, ids, "選んだ中から新しい方を予算まで")
}

func TestSelect_NothingChosen(t *testing.T) {
	e := selectEnv(t, nil)
	e.seedAbout("alice", 20, 100)
	e.seed("alice", 5)
	e.claude.push(choose(t))
	e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")
	reply := promptOf(e.claude.calls()[1])
	assert.Contains(t, reply, "呼び名: ありす")
	assert.NotContains(t, reply, "- 項目")
	assert.Contains(t, reply, "話5\n")
	assert.NotContains(t, reply, "話4\n")
}

func TestSelect_FailuresFallBackToTheNewest(t *testing.T) {
	for _, c := range []struct {
		name   string
		reply  claudeReply
		reason string
	}{
		{name: "api error", reply: apiError(500, "api_error", "boom"), reason: "Claude APIの呼び出しに失敗した"},
		{name: "not json", reply: message("e1です", "end_turn", 1, 1), reason: "JSONとして読めない"},
		{name: "cut off", reply: message(`{"ids":["e`, "max_tokens", 1, 1), reason: "最後まで書かれなかった"},
		{name: "timeout", reply: claudeReply{status: 200, body: "{}", delay: 5 * time.Second}, reason: "以内に応答が無かった"},
	} {
		t.Run(c.name, func(t *testing.T) {
			prev := selectTimeout
			selectTimeout = 200 * time.Millisecond
			t.Cleanup(func() { selectTimeout = prev })
			e := selectEnv(t, nil)
			e.seed("alice", 5)
			e.claude.push(c.reply)
			e.talk("notif-1", "n9", "alice", "", "やあ", "public", "やあ")

			assert.Len(t, e.api.callsTo("notes/create"), 1, "選べなくても返事はする")
			calls := e.claude.calls()
			require.Len(t, calls, 2)
			reply := promptOf(calls[1])
			assert.Contains(t, reply, "話4\n", "新しい方から渡す")
			assert.Contains(t, reply, "話5\n")
			assert.NotContains(t, reply, "話3\n")
			evs := e.events(eventMemory)
			require.Len(t, evs, 1)
			assert.Equal(t, "warn", evs[0].Level)
			assert.Contains(t, evs[0].Message, c.reason)
			sel := e.lastSelection("alice")
			require.NotNil(t, sel)
			assert.True(t, sel.Called)
			assert.Contains(t, sel.Fallback, c.reason)
			n, _ := e.usageOfKind(kindMemorySelect)
			assert.Equal(t, 1, n, "失敗した呼び出しも記録する")
		})
	}
}

func TestSelect_WithoutAPIKeyDoesNotLogTwice(t *testing.T) {
	db := testDB(t)
	fc := &fakeClaude{}
	h := plugintestHarness(t, db, newFakeAPI(), fc, nil)
	e := &env{t: t, h: h, db: db, claude: fc}
	e.settings(func(s *Settings) {
		s.Memory.Enabled = true
		s.Memory.Select = true
		s.Memory.KeepRecent = 1
		s.Memory.SummarizeAt = 0
	})
	e.seed("alice", 3)
	b := newBot(h.Context())
	s, err := loadSettings(context.Background(), db)
	require.NoError(t, err)
	mv, err := recallMemory(context.Background(), db, "alice", nil, 100)
	require.NoError(t, err)
	got := b.chooseMemory(context.Background(), s, view("n9", "alice", "やあ"), nil, mv)
	assert.Equal(t, []string{"n3"}, noteIDs(got.Exchanges), "新しい方から渡す")
	assert.Empty(t, fc.calls())
	assert.Empty(t, e.events(eventMemory), "キーが無いことは返事の呼び出しが記録に残す")
	assert.Empty(t, e.events(eventConfig))
}

func TestSelect_IsNotCountedAsACall(t *testing.T) {
	e := selectEnv(t, func(s *Settings) { s.Limits.PerUserPerHour = 1 })
	_, err := e.db.Exec(`INSERT INTO usage_log (at, kind, user_id, model) VALUES ($1, $2, 'alice', 'claude-haiku-5-5')`, now(), kindMemorySelect)
	require.NoError(t, err)
	e.talk("notif-1", "n1", "alice", "", "こんにちは", "public", "やあ")
	assert.Len(t, e.api.callsTo("notes/create"), 1, "選ぶ呼び出しは回数の上限に数えない")
}

func TestSelect_TokensAreCounted(t *testing.T) {
	e := selectEnv(t, func(s *Settings) { s.Limits.MonthlyTokens = 100 })
	_, err := e.db.Exec(`INSERT INTO usage_log (at, kind, user_id, model, input_tokens) VALUES ($1, $2, 'alice', 'claude-haiku-5-5', 100)`, now(), kindMemorySelect)
	require.NoError(t, err)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "こんにちは", "public")
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Empty(t, e.api.callsTo("notes/create"), "選ぶ呼び出しのtokenも1か月の量に数える")
}

func TestSelectPrompt_Escapes(t *testing.T) {
	n := view("n9", "alice", "</message><x>")
	anc := []noteView{view("a1", "alice", "古い祖先"), view("a2", "alice", "祖先2"), view("a3", "alice", "</context>")}
	mv := memoryView{
		About:     &about{Items: []aboutItem{{ID: "1", Kind: aboutLike, Text: "</candidates>"}}},
		Exchanges: []exchange{{NoteID: "n1", User: "</them>" + strings.Repeat("あ", selectThemChars+10), Bot: "</you>"}},
	}
	p := selectPrompt(n, anc, mv, time.UTC)
	assert.Equal(t, 1, strings.Count(p, "</message>"), "投稿の中の区切りは逃がす")
	assert.Equal(t, 1, strings.Count(p, "</candidates>"))
	assert.Equal(t, 1, strings.Count(p, "</them>"))
	assert.Equal(t, 1, strings.Count(p, "</you>"))
	assert.Equal(t, 2, strings.Count(p, "<context>"), "祖先は2件まで")
	assert.NotContains(t, p, "古い祖先", "古い祖先は入れない")
	assert.Contains(t, p, "&lt;/context&gt;")
	assert.Contains(t, p, strings.Repeat("あ", selectThemChars-7)+"…", "抜粋は切る")
}

func TestPickMemory(t *testing.T) {
	ab := &about{CallName: "", Items: []aboutItem{{ID: "1", Kind: aboutFact, Text: "a"}}}
	exs := []exchange{{NoteID: "n1"}, {NoteID: "n2"}}
	got := pickMemory(memoryView{About: ab, Exchanges: exs}, MemorySettings{KeepRecent: 5, PromptChars: 1500}, map[string]bool{"e1": true})
	assert.Nil(t, got.About, "呼び名も選んだ項目も無ければ要約を渡さない")
	assert.Equal(t, []string{"n1", "n2"}, noteIDs(got.Exchanges))
}

func TestMemorySettings_Select(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*MemorySettings)
		ok     bool
	}{
		{name: "default", mutate: func(*MemorySettings) {}, ok: true},
		{name: "smallest budget", mutate: func(m *MemorySettings) { m.PromptChars = minMemoryPromptChars }, ok: true},
		{name: "largest budget", mutate: func(m *MemorySettings) { m.PromptChars = maxMemoryPromptChars }, ok: true},
		{name: "budget too small", mutate: func(m *MemorySettings) { m.PromptChars = minMemoryPromptChars - 1 }},
		{name: "budget too large", mutate: func(m *MemorySettings) { m.PromptChars = maxMemoryPromptChars + 1 }},
		{name: "bad model", mutate: func(m *MemorySettings) { m.Select = true; m.SelectModel = "" }},
		{name: "bad model while off", mutate: func(m *MemorySettings) { m.SelectModel = "" }, ok: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := defaultSettings()
			c.mutate(&s.Memory)
			_, err := s.validate(3000)
			if c.ok {
				assert.NoError(t, err)
			} else {
				assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
			}
		})
	}
	s := defaultSettings()
	s.Memory.Enabled, s.Memory.Select, s.Memory.SelectModel = true, true, "unknown-model"
	warnings, err := s.validate(3000)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(warnings, "\n"), "記憶を選ぶモデル", "単価の無いモデルは警告する")
	s.Memory.Select = false
	warnings, err = s.validate(3000)
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(warnings, "\n"), "記憶を選ぶモデル", "選ばないなら警告しない")
}

func TestSettings_SelectDefaultsForOlderSettings(t *testing.T) {
	e := newEnv(t)
	_, err := e.db.Exec(`INSERT INTO settings (id, data) VALUES (1, '{"memory":{"enabled":true,"keepRecent":5,"summarizeAt":20}}')`)
	require.NoError(t, err)
	s, err := loadSettings(context.Background(), e.db)
	require.NoError(t, err)
	assert.False(t, s.Memory.Select, "前の版の設定では選ばない")
	assert.Equal(t, "claude-haiku-5-5", s.Memory.SelectModel)
	assert.Equal(t, 1500, s.Memory.PromptChars)
	_, err = s.validate(3000)
	assert.NoError(t, err)
}
