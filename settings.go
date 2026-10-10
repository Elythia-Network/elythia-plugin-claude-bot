package claudebot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/elythia-network/elythia/plugin"
)

// Settings is everything the operator configures from the admin page.
//
// 1 行の jsonb に置く。項目は今後も増えるので、列に分けると migration が
// 増えるだけになる。読むときは既定値の上に重ねるので、後から足した項目は
// 保存し直さなくても既定値で動く。
type Settings struct {
	// Model is the Claude model ID, free text (e.g. "claude-opus-5-5").
	Model string `json:"model"`
	// Effort is output_config.effort. Empty means "do not send".
	Effort string `json:"effort"`
	// Timezone is the IANA name used for the schedule and for "today" /
	// "this month".
	Timezone string `json:"timezone"`

	Reply     ReplySettings     `json:"reply"`
	Scheduled ScheduledSettings `json:"scheduled"`
	Limits    LimitSettings     `json:"limits"`
	Budget    BudgetSettings    `json:"budget"`
	Vision    VisionSettings    `json:"vision"`

	// PriceOverrides replaces the built-in price of a model.
	PriceOverrides []PriceOverride `json:"priceOverrides"`
}

// ReplySettings controls answering mentions.
type ReplySettings struct {
	// Mode is "off", "reply", "reaction" or "both".
	Mode         string `json:"mode"`
	SystemPrompt string `json:"systemPrompt"`
	// MaxChars is the length told to the model, in characters.
	MaxChars int `json:"maxChars"`
	// MaxTokens overrides the automatic max_tokens. 0 means automatic.
	MaxTokens int `json:"maxTokens"`
	// Visibility is the default visibility of a reply. 元の投稿より広くはしない。
	Visibility string `json:"visibility"`
	// Audience is "everyone", "followers" or "local".
	Audience string `json:"audience"`
	// ContextNotes is how many notes of the thread are sent to the model.
	ContextNotes int `json:"contextNotes"`
	// MaxRoundTrips caps how many times the bot answers within one thread.
	// 0 means no cap.
	MaxRoundTrips int `json:"maxRoundTrips"`
}

// ScheduledSettings controls the scheduled posts.
type ScheduledSettings struct {
	Enabled      bool   `json:"enabled"`
	SystemPrompt string `json:"systemPrompt"`
	MaxChars     int    `json:"maxChars"`
	MaxTokens    int    `json:"maxTokens"`
	Visibility   string `json:"visibility"`
	// Times are "HH:MM" in Timezone.
	Times []string `json:"times"`
	// IntervalMinutes posts every N minutes counted from 00:00. 0 is off.
	IntervalMinutes int `json:"intervalMinutes"`
}

// VisionSettings controls sending attached images to the model (#6).
type VisionSettings struct {
	// Enabled sends images. 既定はOFF。画像も外部(Anthropic)へ送ることになり、
	// tokenも増えるので、運営者が選んでONにする。
	Enabled bool `json:"enabled"`
	// MaxImages caps the images sent for one reply.
	MaxImages int `json:"maxImages"`
	// IncludeThread also sends the images of the ancestors. OFFなら
	// メンションされた投稿の画像だけを送る。
	IncludeThread bool `json:"includeThread"`
	// IncludeSensitive also sends images marked as sensitive.
	IncludeSensitive bool `json:"includeSensitive"`
}

// LimitSettings caps the cost. 0 means no cap.
type LimitSettings struct {
	PerUserPerHour int `json:"perUserPerHour"`
	GlobalPerDay   int `json:"globalPerDay"`
	// PerHostPerDay caps the calls for users of one remote server a day.
	// 誰でも話しかけられる設定で、1 つのサーバーにアカウントを大量に作って
	// 全体の上限を使い切られるのを抑える。ローカルの利用者には効かない。
	PerHostPerDay int   `json:"perHostPerDay"`
	MonthlyTokens int64 `json:"monthlyTokens"`
	// OverLimit is "silent" or "message".
	OverLimit        string `json:"overLimit"`
	OverLimitMessage string `json:"overLimitMessage"`
}

// BudgetSettings is the monthly budget in USD.
type BudgetSettings struct {
	MonthlyUSD float64 `json:"monthlyUsd"`
	// WarnPercent warns when the remainder falls below this share.
	WarnPercent int `json:"warnPercent"`
	// StopWhenExhausted stops replies and scheduled posts once the estimate
	// reaches the budget.
	StopWhenExhausted bool `json:"stopWhenExhausted"`
}

// PriceOverride is an operator supplied price, USD per million tokens.
type PriceOverride struct {
	Model string `json:"model"`
	Price
}

// DefaultDescription is the profile text set when the account is created.
//
// メンションした投稿が外部 (Anthropic) へ送られることを、話しかける前に
// 分かるようにしておく (issue の要件 11)。
//
// 画像には触れていない。「画像を見る」(#6)をONにしたら、運営者が自己紹介を
// 書き換える前提で、管理画面とREADMEで案内する。
const DefaultDescription = "このアカウントはbotです。メンションした投稿の本文と、そのスレッドの投稿は、返事を作るためにAnthropic社のClaude APIへ送られます。"

func defaultSettings() Settings {
	return Settings{
		Model:    "claude-opus-5-5",
		Effort:   "low",
		Timezone: "Asia/Tokyo",
		Reply: ReplySettings{
			Mode:          "reply",
			SystemPrompt:  "あなたはこのサーバーで動いているbotです。話しかけてきた相手に、親しみやすく簡潔に日本語で返事をしてください。",
			MaxChars:      200,
			Visibility:    "home",
			Audience:      "everyone",
			ContextNotes:  10,
			MaxRoundTrips: 5,
		},
		Scheduled: ScheduledSettings{
			SystemPrompt: "あなたはこのサーバーで動いているbotです。時刻に合った短い投稿を日本語で書いてください。",
			MaxChars:     200,
			Visibility:   "home",
			Times:        []string{},
		},
		Limits: LimitSettings{
			PerUserPerHour:   5,
			GlobalPerDay:     200,
			MonthlyTokens:    2_000_000,
			OverLimit:        "silent",
			OverLimitMessage: "ただいま返事の回数の上限に達しているため、お返事できません。時間をおいて話しかけてください。",
		},
		Budget: BudgetSettings{
			WarnPercent:       20,
			StopWhenExhausted: true,
		},
		Vision: VisionSettings{
			MaxImages: 4,
		},
		PriceOverrides: []PriceOverride{},
	}
}

func loadSettings(ctx context.Context, db *sql.DB) (Settings, error) {
	s := defaultSettings()
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT data FROM settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("load settings: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return defaultSettings(), fmt.Errorf("decode settings: %w", err)
	}
	return s, nil
}

func saveSettings(ctx context.Context, db *sql.DB, s Settings) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
INSERT INTO settings (id, data, updated_at) VALUES (1, $1, now())
ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, raw)
	if err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return nil
}

func (s Settings) location() *time.Location {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

var (
	modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,99}$`)
	timePattern  = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

const (
	maxPromptChars = 20000
	// maxTokensCap は上書きできる max_tokens の上限。ctx.HTTP() は 1 回の
	// リクエストを 30 秒で切るので、これより大きくしても返事は届かない。
	maxTokensCap = 32000
	// maxVisionImages は1回の返事で送る画像の数の上限の上限。1枚ずつ取りに
	// 行くので、多すぎると1つの通知の処理が長くなる。
	maxVisionImages = 20
)

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func badRequest(format string, args ...any) error {
	return plugin.Errorf(http.StatusBadRequest, format, args...)
}

// effortLimits lists the efforts that known models reject (400)。
//
// effort を持たないモデルは nil (空以外は全て拒否)。表に無いモデルは
// 確かめられないので、保存は許して警告だけを出す。
var effortLimits = map[string][]string{
	"claude-haiku-4-5":  nil,
	"claude-sonnet-4-5": nil,
	"claude-opus-4-5":   {"", "low", "medium", "high"},
	"claude-opus-4-6":   {"", "low", "medium", "high", "max"},
	"claude-sonnet-4-6": {"", "low", "medium", "high", "max"},
}

// checkEffort refuses known-incompatible model / effort pairs.
func checkEffort(model, effort string) error {
	allowed, ok := effortLimits[canonicalModel(model)]
	if !ok {
		return nil
	}
	if allowed == nil {
		if effort != "" {
			return badRequest("%s は effort に対応していません。effort を「送らない」にしてください", model)
		}
		return nil
	}
	if !oneOf(effort, allowed...) {
		return badRequest("%s は effort %q に対応していません", model, effort)
	}
	return nil
}

// validate checks s and returns warnings that do not prevent saving.
// maxNoteLength is the server's note length limit.
func (s Settings) validate(maxNoteLength int) ([]string, error) {
	warnings := []string{}
	if err := s.check(maxNoteLength); err != nil {
		return nil, err
	}
	known := false
	if _, ok := priceTable[canonicalModel(s.Model)]; ok {
		known = true
	}
	for _, o := range s.PriceOverrides {
		if canonicalModel(o.Model) == canonicalModel(s.Model) {
			known = true
		}
	}
	if !known {
		warnings = append(warnings, fmt.Sprintf(
			"モデル %q は単価の表にありません。effort に対応しているかを確かめられず、使った額は「単価が未設定」になります (単価の上書きで設定できます)。", s.Model))
	}
	return warnings, nil
}

func (s Settings) check(maxNoteLength int) error {
	if !modelPattern.MatchString(s.Model) {
		return badRequest("モデルのIDが不正です")
	}
	if !oneOf(s.Effort, "", "low", "medium", "high", "xhigh", "max") {
		return badRequest("effortの値が不正です")
	}
	if err := checkEffort(s.Model, s.Effort); err != nil {
		return err
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil || s.Timezone == "" {
		return badRequest("タイムゾーン %q を読めません", s.Timezone)
	}

	r := s.Reply
	if !oneOf(r.Mode, "off", "reply", "reaction", "both") {
		return badRequest("返事の方法が不正です")
	}
	if err := validateText("返事のシステムプロンプト", r.SystemPrompt); err != nil {
		return err
	}
	if r.MaxChars < 1 || r.MaxChars > maxNoteLength {
		return badRequest("返事の長さは1〜%d文字にしてください (投稿の文字数の上限)", maxNoteLength)
	}
	if r.MaxTokens < 0 || r.MaxTokens > maxTokensCap {
		return badRequest("返事のmax_tokensは0(自動)〜%dにしてください", maxTokensCap)
	}
	if !oneOf(r.Visibility, "public", "home", "followers") {
		return badRequest("返事の公開範囲が不正です")
	}
	if !oneOf(r.Audience, "everyone", "followers", "local") {
		return badRequest("話しかけられる人の範囲が不正です")
	}
	if r.ContextNotes < 1 || r.ContextNotes > 50 {
		return badRequest("送るスレッドの投稿の数は1〜50にしてください")
	}
	if r.MaxRoundTrips < 0 || r.MaxRoundTrips > 100 {
		return badRequest("同じスレッドでの返事の上限は0〜100にしてください")
	}

	sc := s.Scheduled
	if err := validateText("定時の投稿のシステムプロンプト", sc.SystemPrompt); err != nil {
		return err
	}
	if sc.MaxChars < 1 || sc.MaxChars > maxNoteLength {
		return badRequest("定時の投稿の長さは1〜%d文字にしてください (投稿の文字数の上限)", maxNoteLength)
	}
	if sc.MaxTokens < 0 || sc.MaxTokens > maxTokensCap {
		return badRequest("定時の投稿のmax_tokensは0(自動)〜%dにしてください", maxTokensCap)
	}
	if !oneOf(sc.Visibility, "public", "home", "followers") {
		return badRequest("定時の投稿の公開範囲が不正です")
	}
	if len(sc.Times) > 48 {
		return badRequest("定時の投稿の時刻は48個までです")
	}
	for _, t := range sc.Times {
		if !timePattern.MatchString(t) {
			return badRequest("時刻 %q はHH:MMの形で書いてください", t)
		}
	}
	if sc.IntervalMinutes != 0 && (sc.IntervalMinutes < 5 || sc.IntervalMinutes > 1440) {
		return badRequest("投稿の間隔は5〜1440分にしてください (0で使わない)")
	}
	if sc.Enabled && len(sc.Times) == 0 && sc.IntervalMinutes == 0 {
		return badRequest("定時の投稿を有効にするなら、時刻か間隔を設定してください")
	}

	l := s.Limits
	if l.PerUserPerHour < 0 || l.GlobalPerDay < 0 || l.PerHostPerDay < 0 || l.MonthlyTokens < 0 {
		return badRequest("費用の上限に負の値は使えません")
	}
	if !oneOf(l.OverLimit, "silent", "message") {
		return badRequest("上限を超えたときの振る舞いが不正です")
	}
	if l.OverLimit == "message" && l.OverLimitMessage == "" {
		return badRequest("断るときの文を入れてください")
	}
	if utf8.RuneCountInString(l.OverLimitMessage) > maxNoteLength-100 {
		return badRequest("断るときの文が長すぎます")
	}

	b := s.Budget
	if math.IsNaN(b.MonthlyUSD) || math.IsInf(b.MonthlyUSD, 0) || b.MonthlyUSD < 0 {
		return badRequest("予算が不正です")
	}
	if b.WarnPercent < 0 || b.WarnPercent > 100 {
		return badRequest("警告を出す残りの割合は0〜100にしてください")
	}

	if s.Vision.MaxImages < 1 || s.Vision.MaxImages > maxVisionImages {
		return badRequest("送る画像の数の上限は1〜%dにしてください", maxVisionImages)
	}

	if len(s.PriceOverrides) > 100 {
		return badRequest("単価の上書きは100件までです")
	}
	seen := map[string]bool{}
	for _, o := range s.PriceOverrides {
		if !modelPattern.MatchString(o.Model) {
			return badRequest("単価を上書きするモデルのIDが不正です")
		}
		// 別名と日付付きの ID は同じモデルとして数える。
		key := canonicalModel(o.Model)
		if seen[key] {
			return badRequest("単価の上書きでモデル %q が重複しています", o.Model)
		}
		seen[key] = true
		for _, v := range []float64{o.Input, o.CacheWrite, o.CacheRead, o.Output} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return badRequest("モデル %q の単価が不正です", o.Model)
			}
		}
	}
	return nil
}

func validateText(label, v string) error {
	if utf8.RuneCountInString(v) > maxPromptChars {
		return badRequest("%sは%d文字以内にしてください", label, maxPromptChars)
	}
	return nil
}
