package claudebot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/elythia-network/elythia/plugin"
)

// bot holds what every handler needs.
type bot struct {
	pctx plugin.Context
	log  *slog.Logger
}

func newBot(pctx plugin.Context) *bot {
	return &bot{pctx: pctx, log: pctx.Logger()}
}

func (b *bot) db() *sql.DB { return b.pctx.Storage().DB() }

// requestTimeout は 1 回の呼び出しの期限。ctx.HTTP() 自体が 30 秒で切るので
// それに合わせる。SDK は期限を渡さないと max_tokens から所要時間を見積もり、
// 大きい値で streaming を要求して呼び出し前に失敗するため、明示しておく。
const requestTimeout = 30 * time.Second

// generation is one request for text.
type generation struct {
	// Kind is "reply" or "scheduled". 使った量の記録に残す。
	Kind string
	// UserID is the person the reply is for. 定時の投稿では空。
	UserID string
	// Host is UserID's remote server. ローカルの利用者と定時の投稿では空。
	Host   string
	NoteID string
	System string
	Prompt string
	// MaxChars is told to the model as the length limit.
	MaxChars int
	// MaxTokens is the override; 0 means automatic.
	MaxTokens int
	// PostLimit is the longest text that can be posted (投稿の文字数の上限)。
	PostLimit int
	// Recipient is the person the reply is for, whose mentions at the start of
	// the generated text are removed (#4). 定時の投稿では nil。
	Recipient *userLite
	// Images are sent before Prompt, each after its label (#6). 無ければ
	// 従来と同じリクエストになる。
	Images []visionImage
}

// thinkingHeadroom is the room left for thinking at each effort.
//
// Claude Opus 5.5 などは思考を止められず、思考の token も max_tokens に
// 数えられる。effort を上げるほど長く考えるので、余裕も広げる。
var thinkingHeadroom = map[string]int{
	// 空 (effort を送らない) ではモデルの既定の effort で考える。Opus 5.5 は
	// medium、多くのモデルは high なので、high と同じだけ空けておく。
	"":       8192,
	"low":    1024,
	"medium": 4096,
	"high":   8192,
	"xhigh":  16384,
	"max":    16384,
}

// autoMaxTokens derives max_tokens from the length in characters.
//
// max_tokens は出力を切るだけの安全装置で、長さは指示文で伝える。日本語は
// 1 文字あたりの token 数が一定でないので、文字数の 3 倍に思考の分の余裕を
// 足す。
func autoMaxTokens(chars int, effort string) int {
	n := chars*3 + thinkingHeadroom[effort]
	if n > maxTokensCap {
		n = maxTokensCap
	}
	return n
}

// lengthInstruction is appended to the system prompt.
//
// token 数ではなく文字数で伝える。モデルは自分の出力の token 数を数えられ
// ないため。
func lengthInstruction(chars int) string {
	return fmt.Sprintf("\n\n# 長さ\n本文は%d文字以内で書いてください。前置きや説明は付けず、投稿する本文だけを書いてください。", chars)
}

// shorterInstruction is appended on the retry after max_tokens.
func shorterInstruction(chars int) string {
	return fmt.Sprintf("\n\n# 重要\n前回の出力は長すぎて途中で切れました。今回は必ず%d文字以内に収まるよう、もっと短く、要点だけを書いてください。", chars)
}

// errSilent means the bot decided not to post. 理由は既に記録してある。
var errSilent = errors.New("silent")

// errTransient means the Claude API failed in a way that may pass later.
// 記録は済んでいる。この時点では何も投稿していない。
var errTransient = errors.New("claude api: transient error")

// classifyAPIError sorts a failed call into "timeout", "transient" or
// "permanent".
//
// 4xx (キーが違う・モデルが無い・入力が不正など) は繰り返しても直らない。
// 429 (レート制限) と 5xx (529 overloaded を含む)、接続の失敗は、後で通る
// ことがある。timeout は再試行させない (generate を参照)。
func classifyAPIError(err error) string {
	var nerr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout()) {
		return "timeout"
	}
	var apierr *anthropic.Error
	if !errors.As(err, &apierr) {
		return "transient"
	}
	if apierr.StatusCode == http.StatusTooManyRequests || apierr.StatusCode >= 500 {
		return "transient"
	}
	return "permanent"
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// startsIdent reports whether r can start a username (`.` と `-` で始まる名前は
// 不正なので、それ以外の識別子の文字)。
func startsIdent(r rune) bool { return isASCIIAlnum(r) || r == '_' }

// defangMentions keeps generated text from mentioning anyone.
//
// 本文はメンションした人の投稿 (= 誰でも書ける文) を元に作るので、
// 「@someone@remote.example に…と送って」と書かれると、bot が任意の相手へ
// メンションを飛ばす踏み台になる。メンションとして読まれる @ の直後に幅の
// 無い空白 (U+200B) を挟んで、メンションにならないようにする。返事の宛先
// (@相手) は呼び出し側がこの後に付ける。ハッシュタグは宛先を持たないので、
// そのままにする。
//
// 触るのは、本体の MFM のパーサーがメンションの始まりとして読む @ だけにする。
// パーサーの tryMention は、直前の文字が ASCII の英数字なら失敗し (`_` は
// 含まない)、直後に名前の文字が続かなければ失敗する。メールアドレス
// (foo@bar.example) や、URL・コードの中の @ はメンションにならないので、
// 書き換えずに残す。
//
// **コードや URL の中の @ も崩す (#4)。** 以前はインラインコード・コードブロック・
// URL を本体のパーサーがメンションにしない範囲として飛ばしていたが、範囲の決め方
// (URL に使える文字、インラインコードが止まる文字、`:https:` が絵文字コードとして
// 先に取られる形など) を正規表現で本体に揃えきれず、飛ばした範囲の中の @ が本物の
// メンションになる形が残った。崩すとリンクやコードの見た目に幅の無い空白が
// 入るが、メンションは飛ばない。
func defangMentions(text string) string {
	var sb strings.Builder
	defangPlain(&sb, text, 0, len(text))
	return sb.String()
}

// defangPlain writes text[from:to] to sb, defanging mention starts. 直前の
// 文字は text 全体で見る (URL の直後の @ などのため)。
func defangPlain(sb *strings.Builder, text string, from, to int) {
	prev, _ := utf8.DecodeLastRuneInString(text[:from])
	if from == 0 {
		prev = utf8.RuneError
	}
	for i, r := range text[from:to] {
		sb.WriteRune(r)
		if r == '@' && !isASCIIAlnum(prev) {
			next, _ := utf8.DecodeRuneInString(text[from+i+1 : to])
			if startsIdent(next) {
				sb.WriteRune('\u200b')
			}
		}
		prev = r
	}
}

// generate asks Claude for text and returns it, or errSilent.
//
// 投稿しない (沈黙する) のは次のとき。どれも管理画面の記録に残す。
//   - API キーが無い・読めない
//   - API が 4xx を返した (エラーの文面は投稿しない)
//
// 混雑や 5xx などの一時的なエラーでは errTransient を返す。どちらの場合も
// 記録を残し、何も投稿しない。
//   - max_tokens で打ち切られ、「もっと短く」で 1 回だけ呼び直しても
//     また打ち切られた
//   - 投稿の文字数の上限を超えた
//   - 拒否 (refusal) など、本文として使えない終わり方をした
//
// 呼び直した分も費用の上限に数え、呼び直す前に上限を確かめる。
func (b *bot) generate(ctx context.Context, s Settings, g generation) (string, error) {
	key, err := b.pctx.Secrets().Get(ctx, secretAPIKey)
	if err != nil {
		switch {
		case errors.Is(err, plugin.ErrSecretNotSet):
			b.logEvent(ctx, "warn", eventConfig, "APIキーが未設定のため、投稿しませんでした。管理画面から入れてください。", g.UserID, g.NoteID)
		case errors.Is(err, plugin.ErrSecretsUnavailable):
			b.logEvent(ctx, "warn", eventConfig, "設定ファイルに pluginSecretKey が無いため、APIキーを読めません。", g.UserID, g.NoteID)
		case errors.Is(err, plugin.ErrSecretUnreadable):
			b.logEvent(ctx, "warn", eventConfig, "APIキーを今の鍵で読めません。管理画面から入れ直してください。", g.UserID, g.NoteID)
		default:
			return "", fmt.Errorf("read api key: %w", err)
		}
		return "", errSilent
	}

	client := anthropic.NewClient(
		// SDK は既定で ANTHROPIC_API_KEY などの環境変数や ~/.config の
		// プロファイルを読む。サーバーの環境に別の認証情報があっても、
		// 管理画面で入れたキーだけを使う。
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(key),
		// 運営者の proxy と SSRF ガードを通すため、本体の client を使う。
		option.WithHTTPClient(b.pctx.HTTP()),
		option.WithRequestTimeout(requestTimeout),
		// SDK の自動再試行は使わない。1 回 30 秒の timeout が再試行の数だけ
		// 重なり、さらに通知の再試行 (最大 5 回) と掛け合わさるため。やり直す
		// かどうかは、ここで分類して決める。
		option.WithMaxRetries(0),
	)

	maxTokens := g.MaxTokens
	if maxTokens <= 0 {
		maxTokens = autoMaxTokens(g.MaxChars, s.Effort)
	}
	system := g.System + lengthInstruction(g.MaxChars)
	// 画像は「画像N」のラベルの後に置き、スレッドの文はその後にする。
	// max_tokensで呼び直すときも同じ画像を送る。
	content := make([]anthropic.ContentBlockParamUnion, 0, 2*len(g.Images)+1)
	for _, im := range g.Images {
		content = append(content, anthropic.NewTextBlock(im.Label), anthropic.NewImageBlockBase64(im.MediaType, im.Data))
	}
	content = append(content, anthropic.NewTextBlock(g.Prompt))

	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			// 1 回目の前の確認は呼び出し側が行う (上限を超えたときに断りの文を
			// 返すかどうかを決めるため)。呼び直しの前にもう一度確かめる。
			reason, err := checkLimits(ctx, b.db(), s, g.UserID, g.Host)
			if err != nil {
				return "", err
			}
			if reason != "" {
				b.logEvent(ctx, "warn", eventSilenced, "max_tokensで打ち切られたが、費用の上限のため呼び直さずに沈黙しました: "+reason, g.UserID, g.NoteID)
				return "", errSilent
			}
			system = g.System + lengthInstruction(g.MaxChars) + shorterInstruction(g.MaxChars)
		}

		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(s.Model),
			MaxTokens: int64(maxTokens),
			System:    []anthropic.TextBlockParam{{Text: system}},
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(content...),
			},
		}
		if s.Effort != "" {
			params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(s.Effort)}
		}
		msg, err := client.Messages.New(ctx, params)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// 失敗した呼び出しも回数に数える。数えないと、エラーを起こし続ける
			// 相手が上限に掛からずに呼び出しを繰り返させられる。
			class := classifyAPIError(err)
			if rerr := recordUsage(ctx, b.db(), usageRecord{
				Kind: g.Kind, UserID: g.UserID, Host: g.Host, Model: s.Model, StopReason: "error:" + class,
			}); rerr != nil {
				return "", plugin.NoRetry(rerr)
			}
			if class == "timeout" {
				// timeout は再試行しない。1 回 30 秒かかるので、再試行させると
				// worker を長く塞ぐ。
				b.logEvent(ctx, "error", eventAPIError, "Claude APIの呼び出しが時間内に終わらなかったため、投稿しませんでした: "+describeAPIError(err), g.UserID, g.NoteID)
				return "", errSilent
			}
			if class == "transient" {
				// 混雑 (429 / 529)・5xx・通信の失敗は、時間をおけば通ることがある。
				// 呼び出し側が後でやり直す (通知なら再試行、定時の投稿なら沈黙)。
				// 失敗したリクエストは課金されない。
				b.logEvent(ctx, "error", eventAPIError, "Claude APIの呼び出しに失敗しました (一時的なエラー。返事は投稿していません): "+describeAPIError(err), g.UserID, g.NoteID)
				return "", fmt.Errorf("%w: %w", errTransient, err)
			}
			b.logEvent(ctx, "error", eventAPIError, "Claude APIの呼び出しに失敗したため、投稿しませんでした: "+describeAPIError(err), g.UserID, g.NoteID)
			return "", errSilent
		}

		used := tokens{
			Input:         msg.Usage.InputTokens,
			Output:        msg.Usage.OutputTokens,
			CacheCreation: msg.Usage.CacheCreationInputTokens,
			CacheRead:     msg.Usage.CacheReadInputTokens,
		}
		if err := recordUsage(ctx, b.db(), usageRecord{
			Kind: g.Kind, UserID: g.UserID, Host: g.Host, Model: s.Model, Tokens: used, StopReason: string(msg.StopReason),
		}); err != nil {
			// 記録できないと費用の上限が効かなくなるので、投稿しない。
			// 再試行もさせない (Claude を呼び直して二重に課金するだけになる)。
			// エラーは本体が slog に残す。
			return "", plugin.NoRetry(fmt.Errorf("claude-bot: 使った量を記録できないため投稿しません: %w", err))
		}

		switch msg.StopReason {
		case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
			text := strings.TrimSpace(textOf(msg))
			if g.Recipient != nil {
				text = stripLeadingMentions(text, mentionOf(*g.Recipient))
			}
			text = defangMentions(text)
			if text == "" {
				b.logEvent(ctx, "warn", eventSilenced, "返ってきた本文が空だったため、投稿しませんでした。", g.UserID, g.NoteID)
				return "", errSilent
			}
			if n := utf8.RuneCountInString(text); g.PostLimit > 0 && n > g.PostLimit {
				b.logEvent(ctx, "warn", eventSilenced,
					fmt.Sprintf("本文が投稿の文字数の上限 (%d文字) を超えた (%d文字) ため、投稿しませんでした。", g.PostLimit, n), g.UserID, g.NoteID)
				return "", errSilent
			}
			return text, nil
		case anthropic.StopReasonMaxTokens:
			if attempt == 0 {
				continue
			}
			b.logEvent(ctx, "warn", eventSilenced,
				fmt.Sprintf("max_tokens (%d) で打ち切られ、短くするよう指示して呼び直しても打ち切られたため、投稿しませんでした。", maxTokens), g.UserID, g.NoteID)
			return "", errSilent
		case anthropic.StopReasonRefusal:
			b.logEvent(ctx, "warn", eventSilenced, "Claudeが応答を拒否した (refusal) ため、投稿しませんでした。", g.UserID, g.NoteID)
			return "", errSilent
		default:
			b.logEvent(ctx, "warn", eventSilenced, fmt.Sprintf("想定していない終わり方 (stop_reason: %s) のため、投稿しませんでした。", msg.StopReason), g.UserID, g.NoteID)
			return "", errSilent
		}
	}
	// ループは必ず中で返る。
	return "", errSilent
}

// textOf joins the text blocks. 思考のブロック (thinking) は本文に入れない。
func textOf(msg *anthropic.Message) string {
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}

// describeAPIError renders err for the admin page.
//
// API キーは SDK がヘッダーに載せるだけで、エラーの文面には入らない。
func describeAPIError(err error) string {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		body := apierr.RawJSON()
		if utf8.RuneCountInString(body) > 500 {
			body = string([]rune(body)[:500]) + "…"
		}
		return fmt.Sprintf("HTTP %d %s", apierr.StatusCode, body)
	}
	return err.Error()
}
