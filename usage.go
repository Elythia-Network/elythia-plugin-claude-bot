package claudebot

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// usageRecord is one Claude API call.
type usageRecord struct {
	Kind   string
	UserID string
	// Host is the remote server of UserID. ローカルの利用者なら空。
	Host       string
	Model      string
	Tokens     tokens
	StopReason string
}

func recordUsage(ctx context.Context, db *sql.DB, r usageRecord) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO usage_log (at, kind, user_id, host, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, stop_reason)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8, $9, $10)`,
		now(), r.Kind, r.UserID, r.Host, r.Model, r.Tokens.Input, r.Tokens.Output, r.Tokens.CacheCreation, r.Tokens.CacheRead, r.StopReason)
	if err != nil {
		return fmt.Errorf("record usage: %w", err)
	}
	return nil
}

// periodStarts returns the start of today and of this month in loc.
func periodStarts(t time.Time, loc *time.Location) (day, month time.Time) {
	l := t.In(loc)
	day = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	month = time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc)
	return day, month
}

// modelUsage is the usage of one model in a period.
type modelUsage struct {
	Model  string `json:"model"`
	Calls  int64  `json:"calls"`
	Tokens tokens `json:"tokens"`
	// CostUSD is nil when the model has no price.
	CostUSD *float64 `json:"costUsd"`
}

// periodUsage is the usage in one period (today or this month).
type periodUsage struct {
	Calls  int64  `json:"calls"`
	Tokens tokens `json:"tokens"`
	// CostUSD sums only the models that have a price.
	CostUSD float64      `json:"costUsd"`
	ByModel []modelUsage `json:"byModel"`
	// Unpriced lists models without a price (「単価が未設定」)。
	Unpriced []string `json:"unpriced"`
}

// usageSince sums the calls since start, pricing them with p.
//
// 単価は読むときに当てる。上書きを変えたら過去の分も新しい単価で数え直す
// (概算なので、どの単価で数えたかを行ごとに持つより分かりやすい)。
// Haiku 5.5 のように入力の大きさで単価が変わるモデルがあるので、
// 1 回の入力が閾値を超えたかどうかで分けて集計する。
func usageSince(ctx context.Context, db *sql.DB, start time.Time, p pricer) (periodUsage, error) {
	rows, err := db.QueryContext(ctx, `
SELECT model,
       (input_tokens + cache_creation_tokens + cache_read_tokens) AS prompt,
       input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
FROM usage_log WHERE at >= $1`, start)
	if err != nil {
		return periodUsage{}, fmt.Errorf("usage: %w", err)
	}
	defer rows.Close() //nolint:errcheck // 読み捨て

	out := periodUsage{ByModel: []modelUsage{}, Unpriced: []string{}}
	byModel := map[string]*modelUsage{}
	for rows.Next() {
		var model string
		var prompt int64
		var t tokens
		if err := rows.Scan(&model, &prompt, &t.Input, &t.Output, &t.CacheCreation, &t.CacheRead); err != nil {
			return periodUsage{}, fmt.Errorf("usage scan: %w", err)
		}
		m := byModel[model]
		if m == nil {
			m = &modelUsage{Model: model}
			byModel[model] = m
		}
		m.Calls++
		m.Tokens.add(t)
		out.Calls++
		out.Tokens.add(t)
		if pr, ok := p.lookup(model, prompt); ok {
			c := cost(t, pr)
			if m.CostUSD == nil {
				m.CostUSD = new(float64)
			}
			*m.CostUSD += c
			out.CostUSD += c
		}
	}
	if err := rows.Err(); err != nil {
		return periodUsage{}, fmt.Errorf("usage rows: %w", err)
	}
	for _, m := range byModel {
		out.ByModel = append(out.ByModel, *m)
		if m.CostUSD == nil {
			out.Unpriced = append(out.Unpriced, m.Model)
		}
	}
	sort.Slice(out.ByModel, func(i, j int) bool { return out.ByModel[i].Model < out.ByModel[j].Model })
	sort.Strings(out.Unpriced)
	return out, nil
}

// budgetStatus is the budget shown on the admin page.
type budgetStatus struct {
	MonthlyUSD   float64 `json:"monthlyUsd"`
	SpentUSD     float64 `json:"spentUsd"`
	RemainingUSD float64 `json:"remainingUsd"`
	// Warning is true when the remainder is below WarnPercent of the budget.
	Warning bool `json:"warning"`
	// Exhausted is true when the estimate reached the budget.
	Exhausted bool `json:"exhausted"`
	// Underestimated is true when some usage has no price, so the real
	// spending is higher than SpentUSD.
	Underestimated bool `json:"underestimated"`
}

func computeBudget(b BudgetSettings, month periodUsage) *budgetStatus {
	if b.MonthlyUSD <= 0 {
		return nil
	}
	st := &budgetStatus{
		MonthlyUSD:     b.MonthlyUSD,
		SpentUSD:       month.CostUSD,
		RemainingUSD:   b.MonthlyUSD - month.CostUSD,
		Underestimated: len(month.Unpriced) > 0,
	}
	st.Exhausted = st.RemainingUSD <= 0
	st.Warning = st.Exhausted || st.RemainingUSD < b.MonthlyUSD*float64(b.WarnPercent)/100
	return st
}

// usageSummary is everything the admin page shows about usage.
type usageSummary struct {
	Today  periodUsage   `json:"today"`
	Month  periodUsage   `json:"month"`
	Budget *budgetStatus `json:"budget"`
}

func summarize(ctx context.Context, db *sql.DB, s Settings) (usageSummary, error) {
	day, month := periodStarts(now(), s.location())
	p := newPricer(s.PriceOverrides)
	today, err := usageSince(ctx, db, day, p)
	if err != nil {
		return usageSummary{}, err
	}
	mon, err := usageSince(ctx, db, month, p)
	if err != nil {
		return usageSummary{}, err
	}
	return usageSummary{Today: today, Month: mon, Budget: computeBudget(s.Budget, mon)}, nil
}

// checkLimits explains why a call is not allowed, or "" when it is.
//
// userID が空なら (定時の投稿) 1 人あたりの上限は見ない。host が空なら
// (ローカルの利用者と定時の投稿) サーバーごとの上限は見ない。回数は API を
// 呼んだ回数で数えるので、max_tokens で打ち切られて呼び直した分も、
// エラーや timeout で終わった呼び出しも 1 回に数える。
func checkLimits(ctx context.Context, db *sql.DB, s Settings, userID, host string) (string, error) {
	t := now()
	day, month := periodStarts(t, s.location())
	l := s.Limits
	if userID != "" && l.PerUserPerHour > 0 {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM usage_log WHERE user_id = $1 AND at > $2`, userID, t.Add(-time.Hour)).Scan(&n); err != nil {
			return "", fmt.Errorf("limit per user: %w", err)
		}
		if n >= l.PerUserPerHour {
			return fmt.Sprintf("1人あたりの上限 (1時間に%d回) に達しました", l.PerUserPerHour), nil
		}
	}
	if host != "" && l.PerHostPerDay > 0 {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM usage_log WHERE host = $1 AND at >= $2`, host, day).Scan(&n); err != nil {
			return "", fmt.Errorf("limit per host: %w", err)
		}
		if n >= l.PerHostPerDay {
			return fmt.Sprintf("サーバー %s の1日あたりの上限 (%d回) に達しました", host, l.PerHostPerDay), nil
		}
	}
	if l.GlobalPerDay > 0 {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM usage_log WHERE at >= $1`, day).Scan(&n); err != nil {
			return "", fmt.Errorf("limit per day: %w", err)
		}
		if n >= l.GlobalPerDay {
			return fmt.Sprintf("全体の1日あたりの上限 (%d回) に達しました", l.GlobalPerDay), nil
		}
	}
	if l.MonthlyTokens > 0 {
		var n int64
		if err := db.QueryRowContext(ctx, `
SELECT COALESCE(sum(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0)
FROM usage_log WHERE at >= $1`, month).Scan(&n); err != nil {
			return "", fmt.Errorf("limit tokens: %w", err)
		}
		if n >= l.MonthlyTokens {
			return fmt.Sprintf("1か月のtokenの上限 (%d) に達しました", l.MonthlyTokens), nil
		}
	}
	if s.Budget.MonthlyUSD > 0 && s.Budget.StopWhenExhausted {
		mon, err := usageSince(ctx, db, month, newPricer(s.PriceOverrides))
		if err != nil {
			return "", err
		}
		if b := computeBudget(s.Budget, mon); b != nil && b.Exhausted {
			return fmt.Sprintf("今月の予算 (%.2f USD) を使い切りました (概算)", s.Budget.MonthlyUSD), nil
		}
	}
	return "", nil
}
