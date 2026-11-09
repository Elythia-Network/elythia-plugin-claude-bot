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

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// summaryEnv is memoryEnv that summarizes after 3 exchanges, keeping 1.
func summaryEnv(t *testing.T, mutate func(*Settings)) *env {
	t.Helper()
	return memoryEnv(t, func(s *Settings) {
		s.Memory.KeepRecent = 1
		s.Memory.SummarizeAt = 3
		if mutate != nil {
			mutate(s)
		}
	})
}

// seed remembers n exchanges with alice directly ("話1"... / "返事1"...).
func (e *env) seed(userID string, n int) {
	e.t.Helper()
	u := userLite{ID: userID, Username: userID}
	for i := 1; i <= n; i++ {
		mustRemember(e.t, context.Background(), e.db, u,
			exchange{NoteID: fmt.Sprintf("n%d", i), User: fmt.Sprintf("話%d", i), Bot: fmt.Sprintf("返事%d", i), At: now().Add(time.Duration(i) * time.Minute)}, 100)
	}
}

func (e *env) runSummary(userID string) error {
	e.t.Helper()
	return e.h.Jobs(Plugin).Run(e.t, jobSummarize, fmt.Sprintf(`{"userId":%q}`, userID))
}

func (e *env) aboutOf(userID string) *about {
	e.t.Helper()
	mv, err := recallMemory(context.Background(), e.db, userID, nil, 1000)
	require.NoError(e.t, err)
	return mv.About
}

func (e *env) leased(userID string) bool {
	e.t.Helper()
	var held bool
	require.NoError(e.t, e.db.QueryRow(`SELECT summarizing_since IS NOT NULL FROM memories WHERE user_id = $1`, userID).Scan(&held))
	return held
}

// backedOff reports whether a failed summary of userID waits before it is
// tried again: the job does nothing now and runs after summaryRetryAfter.
func (e *env) backedOff(userID string) bool {
	e.t.Helper()
	before := len(e.claude.calls())
	require.NoError(e.t, e.runSummary(userID))
	return len(e.claude.calls()) == before && e.leased(userID)
}

func (e *env) usageOfKind(kind string) (calls int, total int64) {
	e.t.Helper()
	require.NoError(e.t, e.db.QueryRow(`
SELECT count(*), COALESCE(sum(input_tokens + output_tokens), 0) FROM usage_log WHERE kind = $1`, kind).Scan(&calls, &total))
	return calls, total
}

func editJSON(t *testing.T, ed summaryEdit) string {
	t.Helper()
	raw, err := json.Marshal(ed)
	require.NoError(t, err)
	return string(raw)
}

func noteIDs(mem []exchange) []string {
	ids := []string{}
	for _, ex := range mem {
		ids = append(ids, ex.NoteID)
	}
	return ids
}

func TestSummary_EnqueuedWhenTheMemoryIsFull(t *testing.T) {
	e := summaryEnv(t, nil)
	e.talk("notif-1", "n1", "alice", "", "話1", "public", "返事1")
	e.talk("notif-2", "n2", "alice", "", "話2", "public", "返事2")
	assert.Empty(t, e.h.Enqueues(), "まだたまっていない")

	e.talk("notif-3", "n3", "alice", "", "話3", "public", "返事3")
	enq := e.h.Enqueues()
	require.Len(t, enq, 1)
	assert.Equal(t, jobSummarize, enq[0].Name)
	assert.JSONEq(t, `{"userId":"alice"}`, string(enq[0].Payload))
	assert.Equal(t, summaryDedup, enq[0].Options.DedupTTL, "同じ人の要約を積み重ねない")
	assert.LessOrEqual(t, enq[0].Options.MaxAttempts, 1, "再試行させない")
}

func TestSummary_NotEnqueuedWhenOff(t *testing.T) {
	e := summaryEnv(t, func(s *Settings) { s.Memory.SummarizeAt = 0 })
	for i := 1; i <= 4; i++ {
		e.talk(fmt.Sprintf("notif-%d", i), fmt.Sprintf("n%d", i), "alice", "", "話", "public", "返事")
	}
	assert.Empty(t, e.h.Enqueues())
	assert.Len(t, e.memoryOf("alice"), 1, "要約しないときは返事に渡す数だけを残す")
}

func TestSummary_BacklogIsTwiceSummarizeAt(t *testing.T) {
	e := summaryEnv(t, func(s *Settings) { s.Limits.PerUserPerHour = 0 })
	for i := 1; i <= 8; i++ {
		e.talk(fmt.Sprintf("notif-%d", i), fmt.Sprintf("n%d", i), "alice", "", "話", "public", "返事")
	}
	assert.Equal(t, []string{"n3", "n4", "n5", "n6", "n7", "n8"}, noteIDs(e.memoryOf("alice")),
		"要約が進まなくても、summarizeAtの2倍で切る")
}

func TestSummary_WritesTheSummaryAndKeepsTheRecent(t *testing.T) {
	e := summaryEnv(t, func(s *Settings) { s.Memory.KeepRecent = 2 })
	e.seed("alice", 3)
	e.claude.push(message(editJSON(t, summaryEdit{CallName: "ありす", Add: []aboutAdd{{Kind: aboutLike, Text: "猫"}, {Kind: aboutTopic, Text: "引っ越し"}, {Kind: aboutFact, Text: "朝が苦手"}}}), "end_turn", 300, 50))
	require.NoError(t, e.runSummary("alice"))

	calls := e.claude.calls()
	require.Len(t, calls, 1)
	req := calls[0]
	assert.Equal(t, summarySystem, systemOf(req), "運営者のシステムプロンプトは使わない")
	format, _ := req.Body["output_config"].(map[string]any)["format"].(map[string]any)
	require.NotNil(t, format, "structured outputsで形を決める")
	assert.Equal(t, "json_schema", format["type"])
	schema, _ := format["schema"].(map[string]any)
	assert.Equal(t, false, schema["additionalProperties"])
	assert.ElementsMatch(t, []any{"callName", "add", "update", "remove"}, schema["required"])
	assert.Equal(t, "low", req.Body["output_config"].(map[string]any)["effort"], "設定のeffortを使う")
	p := promptOf(req)
	assert.Contains(t, p, "(まだありません)")
	assert.Contains(t, p, "話1", "古い方を材料にする")
	assert.NotContains(t, p, "話2", "残す直近の分は材料にしない")
	assert.NotContains(t, p, "話3")

	assert.Equal(t, []string{"n2", "n3"}, noteIDs(e.memoryOf("alice")), "材料にした分を消し、直近を残す")
	ab := e.aboutOf("alice")
	require.NotNil(t, ab)
	assert.Equal(t, "ありす", ab.CallName)
	assert.False(t, e.leased("alice"), "印を外す")
	calls2, total := e.usageOfKind(kindMemory)
	assert.Equal(t, 1, calls2)
	assert.EqualValues(t, 350, total, "使ったtokenを記録する")
	var uid string
	require.NoError(t, e.db.QueryRow(`SELECT user_id FROM usage_log WHERE kind = $1`, kindMemory).Scan(&uid))
	assert.Equal(t, "alice", uid)

	// 次の返事に「この人について」として渡す。
	e.talk("notif-1", "n9", "alice", "", "おはよう", "public", "おはよう!")
	reply := promptOf(e.claude.calls()[1])
	require.Contains(t, reply, "<about>")
	assert.Contains(t, reply, "呼び名: ありす")
	assert.Contains(t, reply, "好きなもの・関心:\n- 猫\n")
	assert.Contains(t, reply, "これまでに話した話題:\n- 引っ越し\n")
	assert.Contains(t, reply, "メモ:\n- 朝が苦手\n")
	assert.Less(t, strings.Index(reply, "</about>"), strings.Index(reply, "<exchange"), "要約をやり取りの前に置く")
}

func TestSummary_UsesTheCurrentSummary(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", 3)
	e.claude.push(message(editJSON(t, summaryEdit{CallName: "ありす", Add: []aboutAdd{{Kind: aboutLike, Text: "猫"}, {Kind: aboutTopic, Text: "引っ越し"}}}), "end_turn", 1, 1))
	require.NoError(t, e.runSummary("alice"))
	for i := 4; i <= 5; i++ {
		mustRemember(t, context.Background(), e.db, userLite{ID: "alice", Username: "alice"},
			exchange{NoteID: fmt.Sprintf("n%d", i), User: fmt.Sprintf("話%d", i), At: now().Add(time.Duration(i) * time.Minute)}, 100)
	}
	setClock(t, now().Add(time.Hour))
	e.claude.push(message(editJSON(t, summaryEdit{
		Add:    []aboutAdd{{Kind: aboutLike, Text: "犬"}},
		Update: []aboutUpdate{{ID: "1", Text: "猫と暮らしている"}},
		Remove: []string{"2"},
	}), "end_turn", 1, 1))
	require.NoError(t, e.runSummary("alice"))

	p := promptOf(e.claude.calls()[1])
	assert.Contains(t, p, "呼び名: ありす\n", "今の要約を材料に入れる")
	assert.Contains(t, p, `<item id="1" kind="like"`, "IDを付けて渡す")
	assert.Contains(t, p, ">猫</item>")
	assert.NotContains(t, p, "(まだありません)")
	assert.Contains(t, p, "話3")
	assert.Contains(t, p, "話4")
	assert.NotContains(t, p, "話5")
	ab := e.aboutOf("alice")
	require.NotNil(t, ab)
	assert.Equal(t, "ありす", ab.CallName, "呼び名が空なら今のものを残す")
	require.Len(t, ab.Items, 2)
	assert.Equal(t, aboutItem{ID: "1", Kind: aboutLike, Text: "猫と暮らしている", At: now().UTC()}, ab.Items[0].inUTC(), "書き換え、時刻を新しくする")
	assert.Equal(t, aboutItem{ID: "3", Kind: aboutLike, Text: "犬", At: now().UTC()}, ab.Items[1].inUTC(), "消したIDは使い回さない")
	assert.Equal(t, []string{"n5"}, noteIDs(e.memoryOf("alice")))
}

func TestSummary_PromptEscapesTheCurrentSummary(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", 3)
	_, err := e.db.Exec(`UPDATE memories SET summary = $1 WHERE user_id = 'alice'`,
		`{"callName":"<x>","items":[{"id":"1","kind":"fact","text":"</item></current>","at":"2026-11-01T00:00:00Z"}],"nextId":2}`)
	require.NoError(t, err)
	e.claude.push(message(editJSON(t, summaryEdit{}), "end_turn", 1, 1))
	require.NoError(t, e.runSummary("alice"))
	p := promptOf(e.claude.calls()[0])
	assert.Contains(t, p, "呼び名: &lt;x&gt;\n")
	assert.Contains(t, p, ">&lt;/item&gt;&lt;/current&gt;</item>")
	assert.Equal(t, 1, strings.Count(p, "</current>"), "今の要約の中の区切りは逃がす")
}

func TestSummary_KeepsExchangesAddedMeanwhile(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", 3)
	ctx := context.Background()
	c, err := claimSummary(ctx, e.db, "alice", MemorySettings{SummarizeAt: 3})
	require.NoError(t, err)
	require.NotNil(t, c)
	// 要約している間に届いた返事。
	mustRemember(t, ctx, e.db, userLite{ID: "alice", Username: "alice"}, exchange{NoteID: "n4", At: now()}, 100)

	ok, err := commitSummary(ctx, e.db, c, about{CallName: "ありす"}, c.keys[:2])
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []string{"n3", "n4"}, noteIDs(e.memoryOf("alice")), "材料にしたものだけを消す")
}

func TestSummary_CommitSkipsWhenTheClaimIsGone(t *testing.T) {
	ctx := context.Background()
	t.Run("deleted", func(t *testing.T) {
		e := summaryEnv(t, nil)
		e.seed("alice", 3)
		c, err := claimSummary(ctx, e.db, "alice", MemorySettings{SummarizeAt: 3})
		require.NoError(t, err)
		_, err = e.db.Exec(`DELETE FROM memories`)
		require.NoError(t, err)
		ok, err := commitSummary(ctx, e.db, c, about{CallName: "x"}, c.keys)
		require.NoError(t, err)
		assert.False(t, ok, "管理画面から消した記憶を作り直さない")
		var n int
		require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n))
		assert.Zero(t, n)
	})
	t.Run("taken over", func(t *testing.T) {
		e := summaryEnv(t, nil)
		e.seed("alice", 3)
		c, err := claimSummary(ctx, e.db, "alice", MemorySettings{SummarizeAt: 3})
		require.NoError(t, err)
		// 印が古くなり、別のjobが取り直した。
		setClock(t, now().Add(summaryLease+time.Minute))
		c2, err := claimSummary(ctx, e.db, "alice", MemorySettings{SummarizeAt: 3})
		require.NoError(t, err)
		require.NotNil(t, c2, "古い印は取り直せる")
		ok, err := commitSummary(ctx, e.db, c, about{CallName: "古い"}, c.keys)
		require.NoError(t, err)
		assert.False(t, ok, "取り直された後は書き戻さない")
		assert.Len(t, e.memoryOf("alice"), 3)
	})
}

func TestSummary_ACurrentClaimBlocksAnotherJob(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", 3)
	_, err := claimSummary(context.Background(), e.db, "alice", MemorySettings{SummarizeAt: 3})
	require.NoError(t, err)
	require.NoError(t, e.runSummary("alice"))
	assert.Empty(t, e.claude.calls(), "同じ人の要約を同時に作らない")
}

func TestSummary_NothingToDo(t *testing.T) {
	t.Run("not full", func(t *testing.T) {
		e := summaryEnv(t, nil)
		e.seed("alice", 2)
		require.NoError(t, e.runSummary("alice"))
		assert.Empty(t, e.claude.calls())
		assert.False(t, e.leased("alice"))
	})
	t.Run("unknown person", func(t *testing.T) {
		e := summaryEnv(t, nil)
		require.NoError(t, e.runSummary("nobody"))
		assert.Empty(t, e.claude.calls())
	})
	t.Run("disabled", func(t *testing.T) {
		e := summaryEnv(t, nil)
		e.seed("alice", 3)
		e.settings(func(s *Settings) { s.Memory.SummarizeAt = 3; s.Memory.KeepRecent = 1 })
		require.NoError(t, e.runSummary("alice"))
		assert.Empty(t, e.claude.calls(), "記憶を無効にしたら要約しない")
	})
	t.Run("summarizing off", func(t *testing.T) {
		e := summaryEnv(t, func(s *Settings) { s.Memory.SummarizeAt = 0 })
		e.seed("alice", 3)
		require.NoError(t, e.runSummary("alice"))
		assert.Empty(t, e.claude.calls())
	})
	t.Run("remote person with local audience", func(t *testing.T) {
		e := summaryEnv(t, nil)
		remoteHost := "remote.example"
		for i := 1; i <= 3; i++ {
			mustRemember(t, context.Background(), e.db, userLite{ID: "bob", Username: "bob", Host: &remoteHost},
				exchange{NoteID: fmt.Sprintf("n%d", i), At: now().Add(time.Duration(i) * time.Minute)}, 100)
		}
		require.NoError(t, e.runSummary("bob"))
		assert.Empty(t, e.claude.calls(), "覚える相手の範囲の外の人は要約しない")
		assert.False(t, e.leased("bob"))
	})
	t.Run("keep everything", func(t *testing.T) {
		e := summaryEnv(t, nil)
		e.seed("alice", 3)
		// 範囲の検証を通らない値でも、要約の側で何も消さないことを確かめる。
		c, err := claimSummary(context.Background(), e.db, "alice", MemorySettings{SummarizeAt: 3})
		require.NoError(t, err)
		require.NoError(t, releaseSummary(context.Background(), e.db, c))
		e.settings(func(s *Settings) { s.Memory.Enabled = true; s.Memory.KeepRecent = 5; s.Memory.SummarizeAt = 3 })
		require.NoError(t, e.runSummary("alice"))
		assert.Empty(t, e.claude.calls(), "残す数がたまった数以上なら材料が無い")
		assert.False(t, e.leased("alice"))
	})
}

func TestSummary_FailuresKeepTheMemory(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply claudeReply
		event string
		usage bool
	}{
		{name: "api error", reply: apiError(500, "api_error", "boom"), event: "Claude APIの呼び出しに失敗", usage: true},
		{name: "bad request", reply: apiError(400, "invalid_request_error", "output_config.format is not supported"), event: "Claude APIの呼び出しに失敗", usage: true},
		{name: "not json", reply: message("覚え書きです", "end_turn", 1, 1), event: "JSONとして読めない", usage: true},
		{name: "cut off", reply: message(`{"callName":"あ`, "max_tokens", 1, 1), event: "最後まで書かれなかった", usage: true},
		{name: "refusal", reply: message("", "refusal", 1, 1), event: "最後まで書かれなかった", usage: true},
		{name: "empty", reply: message(`{"callName":" ","add":[{"kind":"fact","text":" "}],"update":[],"remove":[]}`, "end_turn", 1, 1), event: "要約が空だった", usage: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := summaryEnv(t, nil)
			e.seed("alice", 3)
			e.claude.push(c.reply)
			require.NoError(t, e.runSummary("alice"), "jobとしては再試行させない")
			assert.Len(t, e.memoryOf("alice"), 3, "失敗したら記録を消さない")
			assert.Nil(t, e.aboutOf("alice"))
			assert.True(t, e.backedOff("alice"), "すぐには作り直さない")
			evs := e.events(eventMemory)
			require.Len(t, evs, 1)
			assert.Contains(t, evs[0].Message, c.event)
			calls, _ := e.usageOfKind(kindMemory)
			assert.Equal(t, 1, calls, "失敗した呼び出しも数える")

			// 間をあけた後には作り直せる。
			setClock(t, now().Add(summaryRetryAfter+time.Minute))
			e.claude.push(message(editJSON(t, summaryEdit{CallName: "ありす"}), "end_turn", 1, 1))
			require.NoError(t, e.runSummary("alice"))
			require.NotNil(t, e.aboutOf("alice"))
		})
	}
}

func TestSummary_StopsAtTheCostLimits(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*Settings)
		usage  func(e *env)
	}{
		{name: "monthly tokens", mutate: func(s *Settings) { s.Limits.MonthlyTokens = 100 }, usage: func(e *env) {
			e.addUsage("bob", now(), "claude-opus-5-5", tokens{Input: 100})
		}},
		{name: "budget", mutate: func(s *Settings) { s.Budget.MonthlyUSD = 0.01; s.Budget.StopWhenExhausted = true }, usage: func(e *env) {
			e.addUsage("bob", now(), "claude-opus-5-5", tokens{Input: 1_000_000})
		}},
		{name: "global calls", mutate: func(s *Settings) { s.Limits.GlobalPerDay = 1 }, usage: func(e *env) {
			e.addUsage("bob", now(), "claude-opus-5-5", tokens{})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := summaryEnv(t, c.mutate)
			e.seed("alice", 3)
			c.usage(e)
			require.NoError(t, e.runSummary("alice"))
			assert.Empty(t, e.claude.calls())
			assert.True(t, e.backedOff("alice"))
			assert.Len(t, e.memoryOf("alice"), 3)
			evs := e.events(eventMemory)
			require.Len(t, evs, 1)
			assert.Contains(t, evs[0].Message, "費用の上限")
		})
	}
}

func TestSummary_WithoutAPIKey(t *testing.T) {
	db := testDB(t)
	fc := &fakeClaude{}
	h := plugintestHarness(t, db, newFakeAPI(), fc, nil)
	e := &env{t: t, h: h, db: db, claude: fc}
	e.settings(func(s *Settings) { s.Memory.Enabled = true; s.Memory.KeepRecent = 1; s.Memory.SummarizeAt = 3 })
	e.seed("alice", 3)
	require.NoError(t, e.runSummary("alice"))
	assert.Empty(t, fc.calls())
	assert.True(t, e.backedOff("alice"))
	assert.Len(t, e.memoryOf("alice"), 3)
	assert.NotEmpty(t, e.events(eventConfig), "キーが無いことを記録に残す")
}

func TestSummary_CallsAreNotCountedAsCalls(t *testing.T) {
	// 要約の呼び出しは、回数の上限 (1人・サーバー・全体) には数えない。
	for _, c := range []struct {
		name   string
		mutate func(*Settings)
		host   string
	}{
		{name: "per user", mutate: func(s *Settings) { s.Limits.PerUserPerHour = 1 }},
		{name: "per host", mutate: func(s *Settings) { s.Limits.PerHostPerDay = 1; s.Memory.Audience = "everyone" }, host: "remote.example"},
		{name: "global", mutate: func(s *Settings) { s.Limits.GlobalPerDay = 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := summaryEnv(t, c.mutate)
			var host any
			if c.host != "" {
				host = c.host
			}
			_, err := e.db.Exec(`INSERT INTO usage_log (at, kind, user_id, host, model) VALUES ($1, $2, 'alice', $3, 'claude-opus-5-5')`, now(), kindMemory, host)
			require.NoError(t, err)
			e.talk("notif-1", "n1", "alice", c.host, "こんにちは", "public", "やあ")
			assert.Len(t, e.api.callsTo("notes/create"), 1)
		})
	}
}

func TestSummary_TokensAreCounted(t *testing.T) {
	e := summaryEnv(t, func(s *Settings) { s.Limits.MonthlyTokens = 100 })
	_, err := e.db.Exec(`INSERT INTO usage_log (at, kind, user_id, model, input_tokens) VALUES ($1, $2, 'alice', 'claude-opus-5-5', 100)`, now(), kindMemory)
	require.NoError(t, err)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "こんにちは", "public")
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Empty(t, e.api.callsTo("notes/create"), "要約で使ったtokenも1か月の量に数える")
}

func (it aboutItem) inUTC() aboutItem {
	it.At = it.At.UTC()
	return it
}

func itemTexts(items []aboutItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

func TestAbout_Clamp(t *testing.T) {
	long := func(n int) string { return strings.Repeat("あ", n) }
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a := about{
		CallName: " " + long(maxAboutCallName+5) + " ",
		Items: []aboutItem{
			{ID: "1", Kind: aboutLike, Text: " " + long(maxAboutItemText+1) + " ", At: t0.Add(2 * time.Minute)},
			{ID: "2", Kind: aboutTopic, Text: " \x00 ", At: t0},
			{ID: "3", Kind: "unknown", Text: "a\x00b", At: t0.Add(time.Minute)},
			{ID: "1", Kind: aboutLike, Text: "IDが重なる", At: t0},
			{ID: "", Kind: aboutLike, Text: "IDが無い", At: t0},
		},
	}
	a.clamp()
	assert.Equal(t, long(maxAboutCallName)+"…", a.CallName)
	require.Len(t, a.Items, 2, "空の項目、IDの重なる項目、IDの無い項目は落とす")
	assert.Equal(t, aboutItem{ID: "3", Kind: aboutFact, Text: "ab", At: t0.Add(time.Minute)}, a.Items[0], "NULを除き、知らない区分はメモにし、古い順に並べる")
	assert.Equal(t, long(maxAboutItemText)+"…", a.Items[1].Text)
	assert.True(t, about{Items: []aboutItem{}}.empty())
	assert.False(t, about{CallName: "x"}.empty())
	assert.False(t, about{Items: []aboutItem{{ID: "1", Text: "x"}}}.empty())
}

func TestAbout_ClampKeepsTheNewest(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var a about
	// 新しいものを先に並べ、並べ替えてから切ることを確かめる。
	for i := maxAboutItems + 5; i >= 1; i-- {
		a.Items = append(a.Items, aboutItem{ID: strconv.Itoa(i), Kind: aboutFact, Text: fmt.Sprintf("項目%d", i), At: t0.Add(time.Duration(i) * time.Minute)})
	}
	a.clamp()
	require.Len(t, a.Items, maxAboutItems)
	assert.Equal(t, "6", a.Items[0].ID, "上限を超えたら古い方から落とす")
	assert.Equal(t, strconv.Itoa(maxAboutItems+5), a.Items[maxAboutItems-1].ID)
}

func TestSummaryEdit_Apply(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	cur := &about{CallName: "ありす", Items: []aboutItem{
		{ID: "1", Kind: aboutLike, Text: "猫", At: t0},
		{ID: "2", Kind: aboutTopic, Text: "引っ越し", At: t0},
		{ID: "7", Kind: aboutFact, Text: "朝が苦手", At: t0},
	}}
	got := summaryEdit{
		Add:    []aboutAdd{{Kind: aboutTopic, Text: "新しい仕事"}},
		Update: []aboutUpdate{{ID: "1", Text: "猫と暮らしている"}, {ID: "7", Text: "朝が苦手"}, {ID: "99", Text: "知らないID"}},
		Remove: []string{"2", "98"},
	}.apply(cur, t1)
	assert.Equal(t, "ありす", got.CallName, "呼び名が空なら今のものを残す")
	assert.Equal(t, []aboutItem{
		{ID: "7", Kind: aboutFact, Text: "朝が苦手", At: t0},
		{ID: "1", Kind: aboutLike, Text: "猫と暮らしている", At: t1},
		{ID: "8", Kind: aboutTopic, Text: "新しい仕事", At: t1},
	}, got.Items, "同じ本文の書き換えは時刻を変えない。新しいIDは最大の次から")
	assert.Len(t, cur.Items, 3, "今の要約は書き換えない")
	assert.Equal(t, "猫", cur.Items[0].Text)

	assert.Equal(t, "りす", summaryEdit{CallName: " りす "}.apply(cur, t1).CallName, "呼び名を書き換える")
	blank := summaryEdit{Update: []aboutUpdate{{ID: "1", Text: " "}, {ID: "2", Text: " 引っ越し "}}}.apply(cur, t1)
	assert.Equal(t, cur.Items, blank.Items, "空の書き換えで消さず、前後の空白だけの違いでは時刻を変えない")
	long := &about{Items: []aboutItem{{ID: "1", Kind: aboutFact, Text: strings.Repeat("あ", maxAboutItemText) + "…", At: t0}}}
	same := summaryEdit{Update: []aboutUpdate{{ID: "1", Text: strings.Repeat("あ", maxAboutItemText+20)}}}.apply(long, t1)
	assert.Equal(t, t0, same.Items[0].At, "切った後に同じ本文なら時刻を変えない")
	fresh := summaryEdit{Add: []aboutAdd{{Kind: aboutLike, Text: "犬"}}}.apply(nil, t1)
	assert.Equal(t, []aboutItem{{ID: "1", Kind: aboutLike, Text: "犬", At: t1}}, fresh.Items, "要約が無ければ1から")
	assert.True(t, summaryEdit{Remove: []string{"1", "2", "7"}}.apply(&about{Items: cur.Items}, t1).empty())
	reused := summaryEdit{Remove: []string{"7"}, Add: []aboutAdd{{Kind: aboutFact, Text: "x"}}}.apply(cur, t1)
	assert.Equal(t, "8", reused.Items[len(reused.Items)-1].ID, "消した最大のIDを使い回さない")
	assert.Equal(t, "20", summaryEdit{Add: []aboutAdd{{Kind: aboutFact, Text: "x"}}}.apply(&about{Items: cur.Items, NextID: 20}, t1).Items[3].ID, "保存した次のIDを使う")
}

func TestSummaryEdit_ApplyCapsTheEdits(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var ed summaryEdit
	cur := &about{}
	for i := 1; i <= maxAboutEdits+5; i++ {
		ed.Add = append(ed.Add, aboutAdd{Kind: aboutFact, Text: fmt.Sprintf("足す%d", i)})
		ed.Update = append(ed.Update, aboutUpdate{ID: strconv.Itoa(i), Text: fmt.Sprintf("書き換え%d", i)})
		cur.Items = append(cur.Items, aboutItem{ID: strconv.Itoa(i), Kind: aboutFact, Text: fmt.Sprintf("元%d", i), At: t0})
	}
	got := ed.apply(cur, t0.Add(time.Hour))
	added, rewritten := 0, 0
	for _, it := range got.Items {
		switch {
		case strings.HasPrefix(it.Text, "足す"):
			added++
		case strings.HasPrefix(it.Text, "書き換え"):
			rewritten++
		}
	}
	assert.Equal(t, maxAboutEdits, added, "1回に足すのは上限まで")
	assert.Equal(t, maxAboutEdits, rewritten, "1回に書き換えるのは上限まで")
}

func TestAbout_Recent(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a := about{Items: []aboutItem{
		{ID: "1", Text: strings.Repeat("古", 6), At: t0},
		{ID: "2", Text: strings.Repeat("中", 5), At: t0.Add(time.Minute)},
		{ID: "3", Text: strings.Repeat("新", 5), At: t0.Add(2 * time.Minute)},
	}}
	assert.Equal(t, []string{"2", "3"}, itemIDs(a.recent(15)), "新しい方から予算まで")
	assert.Equal(t, []string{"2", "3"}, itemIDs(a.recent(10)), "予算ちょうどまで入れる")
	assert.Equal(t, []string{"3"}, itemIDs(a.recent(9)))
	assert.Empty(t, a.recent(4))
	assert.Equal(t, []string{"1", "2", "3"}, itemIDs(a.recent(16)))
}

func itemIDs(items []aboutItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestDecodeAbout(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	t.Run("legacy", func(t *testing.T) {
		ab := decodeAbout([]byte(`{"callName":"ありす","likes":["猫","犬"],"topics":["引っ越し"],"notes":"朝が苦手。夜は元気。\n東京に住んでいる"}`), &at)
		require.NotNil(t, ab)
		assert.Equal(t, "ありす", ab.CallName)
		assert.Equal(t, []aboutItem{
			{ID: "1", Kind: aboutLike, Text: "猫", At: at},
			{ID: "2", Kind: aboutLike, Text: "犬", At: at},
			{ID: "3", Kind: aboutTopic, Text: "引っ越し", At: at},
			{ID: "4", Kind: aboutFact, Text: "朝が苦手。夜は元気。東京に住んでいる", At: at},
		}, ab.Items, "#18の欄を項目に読み替え、メモの短い文はつなぐ")
		assert.Equal(t, 5, ab.NextID, "次のIDは今ある最大の次")
	})
	t.Run("legacy long note", func(t *testing.T) {
		long := strings.Repeat("あ", maxAboutItemText) + strings.Repeat("い", maxAboutItemText) + "う、え"
		mid := strings.Repeat("か", 60) + "。"
		note := long + "。\n" + mid + mid
		ab := decodeAbout([]byte(fmt.Sprintf(`{"callName":"","likes":[],"topics":[],"notes":%q}`, note)), &at)
		require.NotNil(t, ab)
		assert.Equal(t, []string{strings.Repeat("あ", maxAboutItemText), strings.Repeat("い", maxAboutItemText), "う、え。", mid, mid}, itemTexts(ab.Items),
			"長い文は切らずに分け、つないで100文字を超える文はつながない")
	})
	t.Run("legacy lines", func(t *testing.T) {
		ab := decodeAbout([]byte(`{"callName":"","likes":[],"topics":[],"notes":"猫を飼っている\n東京に住んでいる。\n- 朝型"}`), &at)
		require.NotNil(t, ab)
		assert.Equal(t, []string{"猫を飼っている。東京に住んでいる。- 朝型"}, itemTexts(ab.Items), "行をつなぐときは区切る")
	})
	t.Run("legacy keeps likes and topics", func(t *testing.T) {
		var likes, topics []string
		for i := 0; i < 10; i++ {
			likes, topics = append(likes, fmt.Sprintf("好き%d", i)), append(topics, fmt.Sprintf("話題%d", i))
		}
		raw, err := json.Marshal(map[string]any{"callName": "", "likes": likes, "topics": topics, "notes": strings.Repeat("・短い。\n", 37)})
		require.NoError(t, err)
		ab := decodeAbout(raw, &at)
		require.NotNil(t, ab)
		kinds := map[string]int{}
		for _, it := range ab.Items {
			kinds[it.Kind]++
		}
		assert.Equal(t, 10, kinds[aboutLike], "箇条書きのメモで好きなものを押し出さない")
		assert.Equal(t, 10, kinds[aboutTopic])
	})
	t.Run("legacy without time", func(t *testing.T) {
		ab := decodeAbout([]byte(`{"callName":"","likes":["猫"],"topics":[],"notes":""}`), nil)
		require.NotNil(t, ab)
		assert.Equal(t, []string{"猫"}, itemTexts(ab.Items))
	})
	t.Run("items", func(t *testing.T) {
		ab := decodeAbout([]byte(`{"callName":"ありす","items":[{"id":"4","kind":"like","text":"猫","at":"2026-11-01T00:00:00Z"}],"likes":["読まない"]}`), &at)
		require.NotNil(t, ab)
		assert.Equal(t, []string{"猫"}, itemTexts(ab.Items), "項目があれば#18の欄は読まない")
	})
	for _, raw := range []string{``, `null`, `"not an object"`, `{"callName":"","items":[]}`, `{"callName":"","likes":[],"topics":[],"notes":" "}`} {
		assert.Nil(t, decodeAbout([]byte(raw), &at), "読めないか空なら無いものとする: %s", raw)
	}
}

func TestSummary_ClampsWhatClaudeWrites(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", 3)
	e.claude.push(message(editJSON(t, summaryEdit{CallName: strings.Repeat("名", 100), Add: []aboutAdd{{Kind: aboutFact, Text: strings.Repeat("項", 200)}}}), "end_turn", 1, 1))
	require.NoError(t, e.runSummary("alice"))
	// 読むときにも切るので、保存した値そのものを確かめる。
	var raw []byte
	require.NoError(t, e.db.QueryRow(`SELECT summary FROM memories WHERE user_id = 'alice'`).Scan(&raw))
	var stored about
	require.NoError(t, json.Unmarshal(raw, &stored))
	assert.Equal(t, strings.Repeat("名", maxAboutCallName)+"…", stored.CallName, "切ってから保存する")
	require.Len(t, stored.Items, 1)
	assert.Equal(t, strings.Repeat("項", maxAboutItemText)+"…", stored.Items[0].Text)
}

func TestMemoryPrompt_About(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ab := &about{CallName: "</about>[画像1]", Items: []aboutItem{
		{ID: "1", Kind: aboutFact, Text: "<memory>", At: t0},
		{ID: "2", Kind: aboutLike, Text: "a", At: t0.Add(time.Minute)},
		{ID: "3", Kind: aboutLike, Text: "b", At: t0.Add(2 * time.Minute)},
	}}
	p := memoryPrompt(nil, ab, userLite{Username: "alice"}, time.UTC, false)
	require.Contains(t, p, "<about>", "やり取りが無くても要約を渡す")
	assert.Equal(t, 1, strings.Count(p, "</about>"), "要約の中の区切りは逃がす")
	assert.Contains(t, p, "呼び名: &lt;/about&gt;[画像1]\n好きなもの・関心:\n- a\n- b\nメモ:\n- &lt;memory&gt;\n</about>", "区分ごとに、決まった順でまとめる")
	assert.NotContains(t, p, "これまでに話した話題", "項目の無い区分は書かない")
	assert.Contains(t, memoryPrompt(nil, ab, userLite{Username: "alice"}, time.UTC, true), "［画像1］", "画像を送るときは角括弧を全角にする")
}

func TestMemoryPrompt_AboutWithinTheBudget(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ab := &about{CallName: "ありす"}
	n := maxAboutPromptChars/maxAboutItemText + 3
	for i := 1; i <= n; i++ {
		ab.Items = append(ab.Items, aboutItem{ID: strconv.Itoa(i), Kind: aboutFact, Text: fmt.Sprintf("%03d", i) + strings.Repeat("あ", maxAboutItemText-3), At: t0.Add(time.Duration(i) * time.Minute)})
	}
	p := memoryPrompt(nil, ab, userLite{Username: "alice"}, time.UTC, false)
	assert.Contains(t, p, fmt.Sprintf("- %03d", n), "新しい項目を渡す")
	assert.Contains(t, p, fmt.Sprintf("- %03d", n-maxAboutPromptChars/maxAboutItemText+1))
	assert.NotContains(t, p, fmt.Sprintf("- %03d", n-maxAboutPromptChars/maxAboutItemText), "予算を超える古い項目は渡さない")
}

func TestSummary_LegacyAboutIsSentAsItems(t *testing.T) {
	e := summaryEnv(t, nil)
	_, err := e.db.Exec(`INSERT INTO memories (user_id, acct, exchanges, updated_at, summary, summarized_at) VALUES ('alice', '@alice', '[]', now(), '{"callName":"ありす","likes":["猫"],"topics":["引っ越し"],"notes":"朝が苦手"}', now())`)
	require.NoError(t, err)
	e.talk("notif-1", "n1", "alice", "", "やあ", "public", "やあ")
	p := promptOf(e.claude.calls()[0])
	assert.Contains(t, p, "呼び名: ありす\n好きなもの・関心:\n- 猫\nこれまでに話した話題:\n- 引っ越し\nメモ:\n- 朝が苦手\n", "#18の要約もそのまま使える")
}

func TestSummary_AboutOnlyIsSent(t *testing.T) {
	e := summaryEnv(t, nil)
	_, err := e.db.Exec(`INSERT INTO memories (user_id, acct, exchanges, updated_at, summary) VALUES ('alice', '@alice', '[]', now(), '{"callName":"ありす","likes":[],"topics":[],"notes":""}')`)
	require.NoError(t, err)
	e.talk("notif-1", "n1", "alice", "", "やあ", "public", "やあ")
	assert.Contains(t, promptOf(e.claude.calls()[0]), "呼び名: ありす")
}

func TestSummary_BrokenSummaryIsIgnored(t *testing.T) {
	e := summaryEnv(t, nil)
	_, err := e.db.Exec(`INSERT INTO memories (user_id, acct, exchanges, updated_at, summary)
VALUES ('alice', '@alice', '[{"noteId":"old","user":"前の話","bot":"前の返事","at":"2026-10-09T00:00:00Z"}]', now(), '"not an object"')`)
	require.NoError(t, err)
	e.talk("notif-1", "n1", "alice", "", "やあ", "public", "やあ")
	p := promptOf(e.claude.calls()[0])
	assert.Contains(t, p, "前の話", "読めない要約があっても、やり取りは使う")
	assert.NotContains(t, p, "<about>")
}

func TestSummary_JobPayload(t *testing.T) {
	e := summaryEnv(t, nil)
	jobs := e.h.Jobs(Plugin)
	for _, payload := range []string{`{"userId":""}`, `[]`} {
		err := jobs.Run(t, jobSummarize, payload)
		require.Error(t, err)
		assert.ErrorIs(t, err, plugin.ErrNoRetry, "形の違うjobは再試行させない: %s", payload)
	}
}

func TestRoutes_MemorySummary(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", 3)
	e.claude.push(message(editJSON(t, summaryEdit{CallName: "ありす"}), "end_turn", 1, 1))
	require.NoError(t, e.runSummary("alice"))
	r := e.h.Routes(Plugin)
	admin := func(body string) plugintest.Request {
		return plugintest.Request{UserID: "admin", Administrator: true, Body: body}
	}

	res, err := r.Call(t, "POST /admin/memories", admin(`{}`))
	require.NoError(t, err)
	require.Len(t, res.(*memoriesResponse).Memories, 1)
	assert.True(t, res.(*memoriesResponse).Memories[0].HasSummary)

	res, err = r.Call(t, "POST /admin/memories/show", admin(`{"userId":"alice"}`))
	require.NoError(t, err)
	shown := res.(*memoryResponse)
	require.NotNil(t, shown.About)
	assert.Equal(t, "ありす", shown.About.CallName)
	require.NotNil(t, shown.SummarizedAt)
	assert.Len(t, shown.Exchanges, 1)

	_, err = r.Call(t, "POST /admin/memories/delete", admin(`{"summaryOnly":true}`))
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
	_, err = r.Call(t, "POST /admin/memories/delete", admin(`{"summaryOnly":true,"all":true}`))
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err), "全員の要約だけを消す形は受け付けない")

	res, err = r.Call(t, "POST /admin/memories/delete", admin(`{"userId":"alice","summaryOnly":true}`))
	require.NoError(t, err)
	assert.False(t, res.(*memoriesResponse).Memories[0].HasSummary)
	assert.Nil(t, e.aboutOf("alice"))
	assert.Len(t, e.memoryOf("alice"), 1, "要約だけを消し、やり取りは残す")
	res, err = r.Call(t, "POST /admin/memories/show", admin(`{"userId":"alice"}`))
	require.NoError(t, err)
	assert.Nil(t, res.(*memoryResponse).SummarizedAt)
}

func TestMemorySettings_SummarizeAt(t *testing.T) {
	for _, c := range []struct {
		name string
		keep int
		at   int
		ok   bool
	}{
		{name: "off", keep: 5, at: 0, ok: true},
		{name: "equal to keep", keep: 5, at: 5},
		{name: "gap too small", keep: 5, at: 5 + minSummaryGap - 1},
		{name: "smallest gap", keep: 5, at: 5 + minSummaryGap, ok: true},
		{name: "largest gap", keep: 5, at: 5 + maxSummaryBatch, ok: true},
		{name: "gap over the batch", keep: 5, at: 5 + maxSummaryBatch + 1},
		{name: "max", keep: maxMemoryKeepRecent, at: maxMemorySummarizeAt, ok: true},
		{name: "negative", keep: 5, at: -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := defaultSettings()
			s.Memory.KeepRecent, s.Memory.SummarizeAt = c.keep, c.at
			_, err := s.validate(3000)
			if c.ok {
				assert.NoError(t, err)
			} else {
				assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
			}
		})
	}
}

func TestSummary_UnreadableExchanges(t *testing.T) {
	e := summaryEnv(t, nil)
	_, err := e.db.Exec(`INSERT INTO memories (user_id, acct, exchanges, updated_at) VALUES ('alice', '@alice', '[1, 2, 3]', now())`)
	require.NoError(t, err)
	require.NoError(t, e.runSummary("alice"))
	assert.Empty(t, e.claude.calls(), "読めない記録は要約しない")
	assert.True(t, e.backedOff("alice"), "記録を何度も残さないよう間をあける")
	evs := e.events(eventMemory)
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0].Message, "読めないため、要約しませんでした")
}

func TestSummary_BatchIsCapped(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", maxSummaryBatch+5)
	e.claude.push(message(editJSON(t, summaryEdit{CallName: "ありす"}), "end_turn", 1, 1))
	require.NoError(t, e.runSummary("alice"))
	p := promptOf(e.claude.calls()[0])
	assert.Contains(t, p, "話1\n")
	assert.Contains(t, p, fmt.Sprintf("話%d\n", maxSummaryBatch))
	assert.NotContains(t, p, fmt.Sprintf("話%d\n", maxSummaryBatch+1), "1回に入れるのは古い方から上限まで")
	assert.Len(t, e.memoryOf("alice"), 5, "入れた分だけを消す")
}

func TestSummary_SummaryOnlyDeleteDuringAJob(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", 3)
	ctx := context.Background()
	c, err := claimSummary(ctx, e.db, "alice", MemorySettings{SummarizeAt: 3})
	require.NoError(t, err)
	_, err = e.h.Routes(Plugin).Call(t, "POST /admin/memories/delete",
		plugintest.Request{UserID: "admin", Administrator: true, Body: `{"userId":"alice","summaryOnly":true}`})
	require.NoError(t, err)
	ok, err := commitSummary(ctx, e.db, c, about{CallName: "消したはずの要約"}, c.keys)
	require.NoError(t, err)
	assert.False(t, ok, "要約だけを消した後に、消した要約を材料にした要約を書き戻さない")
	assert.Nil(t, e.aboutOf("alice"))
}

func TestMemory_RememberDropsBrokenElements(t *testing.T) {
	e := memoryEnv(t, nil)
	_, err := e.db.Exec(`INSERT INTO memories (user_id, acct, exchanges, updated_at) VALUES ('alice', '@alice', '[1, "x", {"noteId":"n0","at":"2026-10-09T00:00:00Z"}]', now())`)
	require.NoError(t, err)
	mustRemember(t, context.Background(), e.db, userLite{ID: "alice", Username: "alice"}, exchange{NoteID: "n1", At: now()}, 10)
	assert.Equal(t, []string{"n0", "n1"}, noteIDs(e.memoryOf("alice")), "オブジェクトでない要素は残さない")
}

func TestSettings_SummarizeAtForOlderSettings(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		raw  string
		want int
	}{
		{raw: `{"memory":{"enabled":true,"keepRecent":20}}`, want: 20 + minSummaryGap},
		{raw: `{"memory":{"enabled":true,"keepRecent":3}}`, want: 20},
		{raw: `{"memory":{"keepRecent":3,"summarizeAt":0}}`, want: 0},
	} {
		_, err := e.db.Exec(`INSERT INTO settings (id, data) VALUES (1, $1) ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data`, c.raw)
		require.NoError(t, err)
		s, err := loadSettings(context.Background(), e.db)
		require.NoError(t, err)
		assert.Equal(t, c.want, s.Memory.SummarizeAt, c.raw)
		_, err = s.validate(3000)
		assert.NoError(t, err, "前の版の設定のまま保存できる: %s", c.raw)
	}
}

func TestSummary_SteadyStateKeepsOnlyTheRecent(t *testing.T) {
	// 検証を通る一番広い差でも、1回の要約で覚えておく数まで減る。
	e := summaryEnv(t, func(s *Settings) {
		s.Memory.KeepRecent = 2
		s.Memory.SummarizeAt = 2 + maxSummaryBatch
	})
	cur, err := loadSettings(context.Background(), e.db)
	require.NoError(t, err)
	_, err = cur.validate(3000)
	require.NoError(t, err, "検証を通る設定で確かめる")
	e.seed("alice", 2+maxSummaryBatch)
	e.claude.push(message(editJSON(t, summaryEdit{CallName: "ありす"}), "end_turn", 1, 1))
	require.NoError(t, e.runSummary("alice"))
	assert.Len(t, e.memoryOf("alice"), 2)
}

func TestSummary_CanceledJobDoesNotWait(t *testing.T) {
	e := summaryEnv(t, nil)
	e.seed("alice", 3)
	e.claude.push(claudeReply{status: 200, body: "{}", delay: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.NoError(t, newBot(e.h.Context()).summarizeMemory(ctx, "alice"))
	assert.False(t, e.leased("alice"), "取り消されただけなら、1時間あけずに印を外す")
	assert.Len(t, e.memoryOf("alice"), 3)
}
