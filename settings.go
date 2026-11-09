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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elythia-network/elythia/plugin"

	"github.com/elythia-network/elythia-plugin-claude-bot/reversi"
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
	Web       WebSettings       `json:"web"`
	Memory    MemorySettings    `json:"memory"`
	Reversi   ReversiSettings   `json:"reversi"`

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

// WebSettings controls the web search and web fetch server tools (#11).
type WebSettings struct {
	// Search offers web search. 既定はOFF。検索は1回ごとに課金されるので、
	// 運営者が選んでONにする。
	Search bool `json:"search"`
	// Fetch offers web fetch. 取得したページの本文もtokenとして数えられる。
	Fetch bool `json:"fetch"`
	// Audience is who may use the web: "none" (only Users), "local",
	// "followers" (people who follow the bot) or "everyone".
	Audience string `json:"audience"`
	// Users may use the web in addition to Audience ("@name" for local users,
	// "@name@host" for remote ones).
	Users []string `json:"users"`
	// PerUserPerHour caps the searches and fetches for one person an hour.
	// 0 means no cap.
	PerUserPerHour int `json:"perUserPerHour"`
	// GlobalPerHour caps the searches and fetches of the whole bot an hour.
	// 0 means no cap.
	GlobalPerHour int `json:"globalPerHour"`
	// MaxUsesPerReply caps the searches and fetches for one reply.
	MaxUsesPerReply int `json:"maxUsesPerReply"`
	// BlockedDomains are left out of search results and never fetched.
	BlockedDomains []string `json:"blockedDomains"`
	// Citations appends the URLs of the pages used to the reply.
	Citations bool `json:"citations"`
	// MaxContentTokens bounds the text of one fetched page (max_content_tokens).
	// 取得したページの本文は入力のtokenとして課金され、その後のサーバー側の
	// 繰り返しのたびに読み直されるので、長いページで1回の費用が膨らまない
	// ようにする。
	MaxContentTokens int `json:"maxContentTokens"`
}

// MemorySettings controls remembering the exchanges with each person (#17).
type MemorySettings struct {
	// Enabled records the exchanges and sends them with later replies. 既定は
	// OFF。話しかけた内容をサーバーに残し、以後の返事にも使うことになるので、
	// 運営者が選んでONにする。
	Enabled bool `json:"enabled"`
	// Audience is whose exchanges are remembered: "local" or "everyone".
	Audience string `json:"audience"`
	// KeepRecent is how many recent exchanges are kept and sent.
	KeepRecent int `json:"keepRecent"`
	// SummarizeAt is how many exchanges are kept before they are summarized
	// (#18). 0 means never summarize.
	SummarizeAt int `json:"summarizeAt"`
	// Select asks SelectModel to choose the remembered items and exchanges
	// related to the note being answered (#31). 試験的な機能で、既定はOFF。
	Select bool `json:"select"`
	// SelectModel is the model that chooses (e.g. "claude-haiku-5-5").
	SelectModel string `json:"selectModel"`
	// PromptChars is how many characters of summary items go into a reply.
	PromptChars int `json:"promptChars"`
}

// ReversiSettings controls playing reversi with the bot (#24).
type ReversiSettings struct {
	// Enabled accepts invitations. 既定はOFF。招待を受けると相手を待たせる
	// ことになるので、運営者が選んでONにする。
	Enabled bool `json:"enabled"`
	// Audience is whose invitations are accepted: "local", "followers" or
	// "everyone".
	Audience string `json:"audience"`
	// Level is the strength (reversi.MinLevel..reversi.MaxLevel).
	Level int `json:"level"`
	// MaxThinkMs caps the time for one move, in milliseconds.
	MaxThinkMs int `json:"maxThinkMs"`
	// MaxGames caps the games in progress at once.
	MaxGames int `json:"maxGames"`
	// ReadyTimeoutMinutes is how long the bot waits for the opponent to get
	// ready before it gives up the game.
	ReadyTimeoutMinutes int `json:"readyTimeoutMinutes"`
}

// backlog is how many exchanges a person's memory holds at most.
//
// 要約するときは、要約が失敗し続けても膨らみ続けないよう summarizeAt の2倍で
// 切る。要約しないときは、返事に渡す数だけを残す。
func (m MemorySettings) backlog() int {
	if m.SummarizeAt > 0 {
		return m.SummarizeAt * 2
	}
	return m.KeepRecent
}

// enabledFor reports whether the exchanges with u are remembered and used.
func (m MemorySettings) enabledFor(u userLite) bool {
	if !m.Enabled {
		return false
	}
	return m.Audience == "everyone" || u.isLocal()
}

// enabled reports whether either tool is on.
func (w WebSettings) enabled() bool { return w.Search || w.Fetch }

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
// 画像とweb、会話の記憶には触れていない。「画像を見る」(#6)や「webを使う」(#11)、
// 「会話を覚える」(#17)をONにしたら、運営者が自己紹介を書き換える前提で、
// 管理画面とREADMEで案内する。
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
		Web: WebSettings{
			Audience:        "none",
			Users:           []string{},
			PerUserPerHour:  10,
			GlobalPerHour:   60,
			MaxUsesPerReply: 3,
			BlockedDomains:  []string{},
			Citations:       true,
			// 1ページ1万tokenで、ニュースの記事やブログの1本はほぼ収まる。
			MaxContentTokens: 10000,
		},
		Memory: MemorySettings{
			Audience:    "local",
			KeepRecent:  5,
			SummarizeAt: 20,
			SelectModel: "claude-haiku-5-5",
			// #18 の決まった欄の合計 (1330文字) と同じくらいにする。
			PromptChars: 1500,
		},
		Reversi: ReversiSettings{
			Audience:            "local",
			Level:               reversi.DefaultLevel,
			MaxThinkMs:          2000,
			MaxGames:            3,
			ReadyTimeoutMinutes: 10,
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
	// 要約 (#18) より前に保存した設定には summarizeAt が無い。覚えておく数を
	// 大きくしてあると、既定値では検証を通らず、どの設定も保存できなくなる
	// ので、その数に合わせた値にする。
	var probe struct {
		Memory struct {
			SummarizeAt *int `json:"summarizeAt"`
		} `json:"memory"`
	}
	if json.Unmarshal(raw, &probe) == nil && probe.Memory.SummarizeAt == nil {
		s.Memory.SummarizeAt = max(defaultSettings().Memory.SummarizeAt, s.Memory.KeepRecent+minSummaryGap)
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
	// maxWebUsesPerReply は1回の返事で使える検索と取得の回数の上限の上限。
	// webを使う呼び出しは1回2分、1回の返事で合わせて3分までなので、
	// 多くしても使い切れない。
	maxWebUsesPerReply = 10
	// maxWebUsers は「webを使える人」の一覧の長さの上限。
	maxWebUsers = 500
	// maxBlockedDomains は除くドメインの数の上限。
	maxBlockedDomains = 64
	// maxWebPerHour は1時間あたりの回数の上限に入れられる値の上限。
	maxWebPerHour = 100000
	// minWebContentTokens と maxWebContentTokens は、取得したページの本文の
	// token数の上限に入れられる値の範囲。小さすぎると本文がほぼ読めず、
	// 大きすぎると1回の取得で費用が膨らむ。
	minWebContentTokens = 1000
	maxWebContentTokens = 100000
	// maxMemoryKeepRecent は覚えておくやり取りの数の上限。1件は最大で
	// maxMemoryChars × 2 文字なので、返事の入力が膨らみすぎない数にする。
	maxMemoryKeepRecent = 20
	// maxMemorySummarizeAt は要約するまでのやり取りの数の上限。記録は最大で
	// この2倍になる。要約するまでの数は、覚えておく数に maxSummaryBatch を
	// 足した数までにする (下の検証) ので、それより大きくはならない。
	maxMemorySummarizeAt = maxMemoryKeepRecent + maxSummaryBatch
	// minMemoryPromptChars / maxMemoryPromptChars bound the characters of
	// summary items in a reply. 下限は項目 (100文字まで) が3つは入る数。上限は
	// 保存できる項目の全部 (60 × 100) が入る数。
	minMemoryPromptChars = 3 * maxAboutItemText
	maxMemoryPromptChars = maxAboutItems * maxAboutItemText
	// minSummaryGap は、要約するまでの数と覚えておく数の差の下限。要約は
	// 記録がこの差の分だけ増えるたびに呼ぶ。差が小さいと、返事のたびに要約を
	// 呼ぶことになる (要約の呼び出しは回数の上限に数えない)。
	minSummaryGap = 10
	// minReversiThinkMs と maxReversiThinkMs は、1手の考える時間の上限に
	// 入れられる値の範囲。探索はキューのworkerの中で動くので、長くすると
	// 他のjobと通知を待たせる。
	minReversiThinkMs = 100
	maxReversiThinkMs = 10000
	// maxReversiGames は同時に進める対局の数の上限の上限。
	maxReversiGames = 10
	// maxReversiReadyTimeout は準備を待つ時間の上限 (分)。動かない記録を閉じる
	// 掃除 (reversiStale の24時間) より十分短くし、準備を外す job が先に動くように
	// する。
	maxReversiReadyTimeout = 720
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
	// 使った額を数えるときと同じ方法で引く (表か上書きにあれば単価がある)。
	_, known := newPricer(s.PriceOverrides).lookup(s.Model, 0)
	// 単価の無いモデルの呼び出しは額に入らないので、予算を使い切っても
	// 止まらない。1か月のtokenの量も無制限だと、止める仕組みが何も
	// 残らないので、保存させない。返事と定時の投稿は同じモデルを使う。
	if !known && s.Budget.MonthlyUSD > 0 && s.Budget.StopWhenExhausted && s.Limits.MonthlyTokens == 0 {
		return nil, badRequest(
			"モデル %q は単価が未設定のため、使った額を数えられず、予算を使い切っても止まりません。単価を上書きするか、1か月のtokenの量に上限を入れてください。", s.Model)
	}
	if !known {
		warnings = append(warnings, fmt.Sprintf(
			"モデル %q は単価の表にありません。effort に対応しているかを確かめられず、使った額は「単価が未設定」になります (単価の上書きで設定できます)。", s.Model))
	}
	// 単価の無いモデルの呼び出しは額に入らないので知らせる。返事のモデルと違って
	// 保存は止めない。選ぶ呼び出しは返事の前に費用の上限を通ったときだけで、
	// 返事の呼び出しが額に入るので、予算を使い切れば選ぶ呼び出しも止まる。
	if s.Memory.Enabled && s.Memory.Select {
		if _, ok := newPricer(s.PriceOverrides).lookup(s.Memory.SelectModel, 0); !ok {
			warnings = append(warnings, fmt.Sprintf(
				"記憶を選ぶモデル %q は単価の表にありません。選ぶ呼び出しの額は「単価が未設定」になります (単価の上書きで設定できます)。", s.Memory.SelectModel))
		}
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

	if err := s.Web.check(); err != nil {
		return err
	}

	if !oneOf(s.Memory.Audience, "local", "everyone") {
		return badRequest("会話を覚える相手の範囲が不正です")
	}
	if s.Memory.KeepRecent < 1 || s.Memory.KeepRecent > maxMemoryKeepRecent {
		return badRequest("覚えておくやり取りの数は1〜%dにしてください", maxMemoryKeepRecent)
	}
	// 差を maxSummaryBatch までにするのは、1回の要約で入れられる数を超えると、
	// 要約の後も記録が覚えておく数まで減らず、相手の発言が説明より長く残るため。
	if n := s.Memory.SummarizeAt; n != 0 && (n < s.Memory.KeepRecent+minSummaryGap || n > s.Memory.KeepRecent+maxSummaryBatch) {
		return badRequest("要約するまでのやり取りの数は、覚えておく数 (%d) に%dを足した数以上、%dを足した数以下にしてください (0で要約しない)", s.Memory.KeepRecent, minSummaryGap, maxSummaryBatch)
	}

	if s.Memory.PromptChars < minMemoryPromptChars || s.Memory.PromptChars > maxMemoryPromptChars {
		return badRequest("返事に渡す覚え書きの文字数は%d〜%dにしてください", minMemoryPromptChars, maxMemoryPromptChars)
	}
	// 選ばない間はモデルの欄を画面に出さないので、ONのときだけ確かめる。
	if s.Memory.Select && !modelPattern.MatchString(s.Memory.SelectModel) {
		return badRequest("記憶を選ぶモデルのIDが不正です")
	}

	if err := s.Reversi.check(); err != nil {
		return err
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

var (
	// webUserPattern is "@name" or "@name@host". 名前とホストの文字は、
	// mention.goのleadingMentionと同じ。
	webUserPattern = regexp.MustCompile(`^@[A-Za-z0-9_](?:[A-Za-z0-9_.-]*[A-Za-z0-9_])?(?:@[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)?$`)
	// domainPattern is a hostname with at least one dot, without a scheme,
	// port or path. APIは裸のTLDや1語の名前を受け付けない。web fetchは
	// パスを付けたドメインを受け付けないので、パスは許さない。
	domainPattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

// check validates the web settings.
func (w WebSettings) check() error {
	if !oneOf(w.Audience, "none", "local", "followers", "everyone") {
		return badRequest("webを使える人の範囲が不正です")
	}
	if len(w.Users) > maxWebUsers {
		return badRequest("webを使える人の一覧は%d件までです", maxWebUsers)
	}
	for _, u := range w.Users {
		if !webUserPattern.MatchString(u) {
			return badRequest("webを使える人 %q は@名前か@名前@ホストの形で書いてください (国際化ドメインのホストは xn-- で始まる形で書きます)", u)
		}
	}
	if w.PerUserPerHour < 0 || w.PerUserPerHour > maxWebPerHour || w.GlobalPerHour < 0 || w.GlobalPerHour > maxWebPerHour {
		return badRequest("webを使う回数の上限は0(上限なし)〜%dにしてください", maxWebPerHour)
	}
	if w.MaxUsesPerReply < 1 || w.MaxUsesPerReply > maxWebUsesPerReply {
		return badRequest("1回の返事でwebを使う回数は1〜%dにしてください", maxWebUsesPerReply)
	}
	if w.MaxContentTokens < minWebContentTokens || w.MaxContentTokens > maxWebContentTokens {
		return badRequest("取得したページの本文のtoken数の上限は%d〜%dにしてください", minWebContentTokens, maxWebContentTokens)
	}
	if len(w.BlockedDomains) > maxBlockedDomains {
		return badRequest("除くドメインは%d件までです", maxBlockedDomains)
	}
	for _, d := range w.BlockedDomains {
		if len(d) > 253 || !domainPattern.MatchString(d) {
			return badRequest("除くドメイン %q はexample.comのように、スキームやパスを付けずに書いてください", d)
		}
	}
	return nil
}

// normalize trims the lists and lowercases what is case-insensitive. 画面で
// 入れた空白や大文字のために保存できなくならないようにする。
func (w *WebSettings) normalize() {
	users := make([]string, 0, len(w.Users))
	for _, u := range w.Users {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		// ホストは大文字小文字を区別しないので小文字にそろえる。名前は
		// 比べるときに大文字小文字を無視する。
		if i := strings.LastIndex(u, "@"); i > 0 {
			u = u[:i] + strings.ToLower(u[i:])
		}
		users = append(users, u)
	}
	w.Users = users
	domains := make([]string, 0, len(w.BlockedDomains))
	for _, d := range w.BlockedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			domains = append(domains, d)
		}
	}
	w.BlockedDomains = domains
}

// check validates the reversi settings.
func (r ReversiSettings) check() error {
	if !oneOf(r.Audience, "local", "followers", "everyone") {
		return badRequest("リバーシの招待を受ける相手の範囲が不正です")
	}
	if r.Level < reversi.MinLevel || r.Level > reversi.MaxLevel {
		return badRequest("リバーシの強さは%d〜%dにしてください", reversi.MinLevel, reversi.MaxLevel)
	}
	if r.MaxThinkMs < minReversiThinkMs || r.MaxThinkMs > maxReversiThinkMs {
		return badRequest("1手の考える時間は%d〜%dミリ秒にしてください", minReversiThinkMs, maxReversiThinkMs)
	}
	if r.MaxGames < 1 || r.MaxGames > maxReversiGames {
		return badRequest("同時に進める対局の数は1〜%dにしてください", maxReversiGames)
	}
	if r.ReadyTimeoutMinutes < 1 || r.ReadyTimeoutMinutes > maxReversiReadyTimeout {
		return badRequest("相手の準備を待つ時間は1〜%d分にしてください", maxReversiReadyTimeout)
	}
	return nil
}

func validateText(label, v string) error {
	if utf8.RuneCountInString(v) > maxPromptChars {
		return badRequest("%sは%d文字以内にしてください", label, maxPromptChars)
	}
	return nil
}
