package claudebot

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPricer_Lookup(t *testing.T) {
	p := newPricer(nil)
	cases := []struct {
		model  string
		prompt int64
		want   Price
		ok     bool
	}{
		{"claude-opus-5-5", 10, Price{4, 5, 0.20, 20}, true},
		{"claude-fable-5-1", 10, Price{10, 12.50, 0.25, 50}, true},
		{"claude-sonnet-4-5", 10, Price{3, 3.75, 0.30, 15}, true},
		// 4.6 より前のモデルは日付付きの ID でも引ける。
		{"claude-sonnet-4-5-20250929", 10, Price{3, 3.75, 0.30, 15}, true},
		{"claude-haiku-4-5-20251001", 10, Price{1, 1.25, 0.10, 5}, true},
		{"claude-opus-4-5-20251101", 10, Price{5, 6.25, 0.50, 25}, true},
		// 4.6 以降は日付付きの ID を持たないので、似た形でも引かない。
		{"claude-opus-4-6-20260101", 10, Price{}, false},
		{"claude-unknown-9", 10, Price{}, false},
		// Haiku 5.5 は入力が 10 万 token を超えると単価が上がる。
		{"claude-haiku-5-5", 100_000, Price{0.10, 0.125, 0.01, 0.50}, true},
		{"claude-haiku-5-5", 100_001, Price{0.50, 0.625, 0.05, 2.50}, true},
	}
	for _, c := range cases {
		got, ok := p.lookup(c.model, c.prompt)
		assert.Equal(t, c.ok, ok, c.model)
		assert.Equal(t, c.want, got, "%s (%d)", c.model, c.prompt)
	}
}

func TestPricer_OverrideWins(t *testing.T) {
	p := newPricer([]PriceOverride{
		{Model: "claude-opus-5-5", Price: Price{Input: 1, Output: 2}},
		{Model: "my-new-model", Price: Price{Input: 3, CacheWrite: 4, CacheRead: 5, Output: 6}},
		{Model: "claude-haiku-5-5", Price: Price{Input: 7}},
	})
	got, ok := p.lookup("claude-opus-5-5", 1)
	require.True(t, ok)
	assert.Equal(t, Price{Input: 1, Output: 2}, got)
	got, ok = p.lookup("my-new-model", 1)
	require.True(t, ok)
	assert.Equal(t, 6.0, got.Output)
	// 上書きには段階が無い。
	got, _ = p.lookup("claude-haiku-5-5", 500_000)
	assert.Equal(t, Price{Input: 7}, got)
}

func TestCost(t *testing.T) {
	// 100 万 token ずつ使えば、単価の合計になる。
	tk := tokens{Input: 1_000_000, Output: 1_000_000, CacheCreation: 1_000_000, CacheRead: 1_000_000}
	assert.InDelta(t, 4+5+0.20+20, cost(tk, Price{4, 5, 0.20, 20}), 1e-9)
	assert.InDelta(t, 0.0012, cost(tokens{Input: 300}, Price{Input: 4}), 1e-12)
}

func TestSummarize_CostBudgetAndUnpriced(t *testing.T) {
	e := newEnv(t)
	// 2026-10-09 12:00 UTC = 21:00 JST
	t0 := now()
	e.addUsage("alice", t0.Add(-time.Hour), "claude-opus-5-5", tokens{Input: 1_000_000, Output: 100_000}) // 4 + 2 = 6
	e.addUsage("bob", t0.AddDate(0, 0, -3), "claude-sonnet-4-5-20250929", tokens{Input: 1_000_000})       // 3
	e.addUsage("", t0.Add(-2*time.Hour), "claude-haiku-5-5", tokens{Input: 200_000, Output: 1_000_000})   // 大きい入力: 0.1 + 2.5
	e.addUsage("", t0.Add(-2*time.Hour), "claude-haiku-5-5", tokens{Input: 50_000, CacheRead: 50_000})    // 入力はちょうど 10 万 token (小さい方)
	e.addUsage("carol", t0.Add(-time.Minute), "mystery-model", tokens{Input: 5, Output: 5})
	e.addUsage("old", time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), "claude-opus-5-5", tokens{Input: 1_000_000}) // 先月 (JST)

	s := defaultSettings()
	s.Budget.MonthlyUSD = 14
	s.Budget.WarnPercent = 20
	sum, err := summarize(context.Background(), e.db, s)
	require.NoError(t, err)

	smallHaiku := (50_000*0.10 + 50_000*0.01) / 1_000_000
	wantMonth := 6 + 3 + (0.1 + 2.5) + smallHaiku
	assert.InDelta(t, wantMonth, sum.Month.CostUSD, 1e-9)
	assert.EqualValues(t, 5, sum.Month.Calls)
	assert.Equal(t, []string{"mystery-model"}, sum.Month.Unpriced, "単価が未設定のモデルは額に入れずに名前を出す")
	assert.EqualValues(t, 4, sum.Today.Calls, "今日 (JST) の分だけ")
	assert.InDelta(t, wantMonth-3, sum.Today.CostUSD, 1e-9)
	for _, m := range sum.Month.ByModel {
		if m.Model == "mystery-model" {
			assert.Nil(t, m.CostUSD)
			assert.EqualValues(t, 10, m.Tokens.total())
		}
	}

	require.NotNil(t, sum.Budget)
	assert.InDelta(t, 14-wantMonth, sum.Budget.RemainingUSD, 1e-9)
	assert.True(t, sum.Budget.Warning, "残り %.2f は予算の20%%未満", sum.Budget.RemainingUSD)
	assert.False(t, sum.Budget.Exhausted)
	assert.True(t, sum.Budget.Underestimated)

	// 予算に余裕があれば警告しない。
	s.Budget.MonthlyUSD = 100
	sum, err = summarize(context.Background(), e.db, s)
	require.NoError(t, err)
	assert.False(t, sum.Budget.Warning)

	// 上書きで額が変わる。
	s.PriceOverrides = []PriceOverride{{Model: "mystery-model", Price: Price{Input: 1_000_000, Output: 0}}}
	sum, err = summarize(context.Background(), e.db, s)
	require.NoError(t, err)
	assert.Empty(t, sum.Month.Unpriced)
	assert.InDelta(t, wantMonth+5, sum.Month.CostUSD, 1e-9)
}

func TestComputeBudget(t *testing.T) {
	assert.Nil(t, computeBudget(BudgetSettings{}, periodUsage{CostUSD: 5}), "予算が無ければ出さない")
	b := computeBudget(BudgetSettings{MonthlyUSD: 10, WarnPercent: 20}, periodUsage{CostUSD: 8.5})
	assert.True(t, b.Warning)
	assert.False(t, b.Exhausted)
	b = computeBudget(BudgetSettings{MonthlyUSD: 10, WarnPercent: 20}, periodUsage{CostUSD: 7.9})
	assert.False(t, b.Warning)
	b = computeBudget(BudgetSettings{MonthlyUSD: 10, WarnPercent: 20}, periodUsage{CostUSD: 10})
	assert.True(t, b.Exhausted)
	assert.True(t, b.Warning)
	assert.InDelta(t, 0, b.RemainingUSD, 1e-12)
}

func TestLimits_PerUserPerHour(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Limits.PerUserPerHour = 2 })
	e.addUsage("alice", now().Add(-10*time.Minute), "claude-opus-5-5", tokens{Input: 1})
	e.addUsage("alice", now().Add(-50*time.Minute), "claude-opus-5-5", tokens{Input: 1})
	e.addUsage("alice", now().Add(-2*time.Hour), "claude-opus-5-5", tokens{Input: 1}) // 1 時間より前
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.api.notes["b1"] = note("b1", "bob", "bob", "", "hi", "public")
	e.claude.push(message("やあ", "end_turn", 1, 1))

	require.NoError(t, e.mention("a", "n1"))
	assert.Empty(t, e.claude.calls(), "alice は上限に達している")
	assert.Empty(t, e.api.callsTo("notes/create"), "既定は沈黙")
	require.Len(t, e.events(eventSilenced), 1)

	require.NoError(t, e.mention("b", "b1"))
	assert.Len(t, e.claude.calls(), 1, "bob は別枠")
}

func TestLimits_RefusalMessageOncePerHour(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) {
		s.Limits.PerUserPerHour = 1
		s.Limits.OverLimit = "message"
		s.Limits.OverLimitMessage = "今は無理です"
		s.Reply.Visibility = "public"
	})
	e.addUsage("alice", now().Add(-time.Minute), "claude-opus-5-5", tokens{Input: 1})
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "followers")
	e.api.notes["n2"] = note("n2", "alice", "alice", "", "hi", "public")

	require.NoError(t, e.mention("a", "n1"))
	require.NoError(t, e.mention("b", "n2"))

	assert.Empty(t, e.claude.calls())
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1, "断りの文は 1 時間に 1 回まで")
	assert.Equal(t, "@alice 今は無理です", creates[0].Params["text"])
	assert.Equal(t, "followers", creates[0].Params["visibility"], "断りの文も元の投稿より広げない")
}

func TestLimits_GlobalPerDay(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Limits.GlobalPerDay = 2; s.Limits.PerUserPerHour = 0 })
	// 今日 (JST) の 2 回。2026-10-09 00:30 JST = 10-08 15:30 UTC
	e.addUsage("x", time.Date(2026, 10, 8, 15, 30, 0, 0, time.UTC), "claude-opus-5-5", tokens{Input: 1})
	e.addUsage("", now().Add(-time.Minute), "claude-opus-5-5", tokens{Input: 1})
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	require.NoError(t, e.mention("a", "n1"))
	assert.Empty(t, e.claude.calls())

	// 昨日 (JST) の分は数えない。
	_, err := e.db.Exec(`UPDATE usage_log SET at = $1 WHERE user_id = 'x'`, time.Date(2026, 10, 8, 14, 30, 0, 0, time.UTC))
	require.NoError(t, err)
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("b", "n1"))
	assert.Len(t, e.claude.calls(), 1)
}

func TestLimits_MonthlyTokens(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Limits.MonthlyTokens = 1000 })
	e.addUsage("x", now().AddDate(0, 0, -2), "claude-opus-5-5", tokens{Input: 400, Output: 300, CacheCreation: 200, CacheRead: 100})
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	require.NoError(t, e.mention("a", "n1"))
	assert.Empty(t, e.claude.calls())

	e.settings(func(s *Settings) { s.Limits.MonthlyTokens = 1001 })
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("b", "n1"))
	assert.Len(t, e.claude.calls(), 1)
}

func TestLimits_BudgetExhaustedStops(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Budget.MonthlyUSD = 4; s.Budget.StopWhenExhausted = true })
	e.addUsage("x", now().AddDate(0, 0, -2), "claude-opus-5-5", tokens{Input: 1_000_000}) // 4 USD
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	require.NoError(t, e.mention("a", "n1"))
	assert.Empty(t, e.claude.calls())
	require.Len(t, e.events(eventSilenced), 1)
	assert.Contains(t, e.events(eventSilenced)[0].Message, "予算")

	// 止めない設定なら返事をする。
	e.settings(func(s *Settings) { s.Budget.MonthlyUSD = 4; s.Budget.StopWhenExhausted = false })
	e.claude.push(message("やあ", "end_turn", 1, 1))
	require.NoError(t, e.mention("b", "n1"))
	assert.Len(t, e.claude.calls(), 1)
}

func TestLimits_RetryIsCountedAndChecked(t *testing.T) {
	e := newEnv(t)
	// 1 回目の呼び出しで 1 日の上限に達する。呼び直しはしない。
	e.settings(func(s *Settings) { s.Limits.GlobalPerDay = 1 })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(message("切れ", "max_tokens", 1, 1), message("短い", "end_turn", 1, 1))
	require.NoError(t, e.mention("a", "n1"))
	assert.Len(t, e.claude.calls(), 1)
	assert.Empty(t, e.api.callsTo("notes/create"))
}

func TestPeriodStarts(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	day, month := periodStarts(time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC), loc) // 11-01 01:00 JST
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, loc), day)
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, loc), month)
}
