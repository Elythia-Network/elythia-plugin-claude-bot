package claudebot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elythia-network/elythia/plugin"
)

func notifications(pctx plugin.Context, n plugin.Notifications) error {
	b := newBot(pctx)
	n.Handle(b.handleNotification)
	return nil
}

// staleClaim is how long a "processing" claim blocks another delivery.
//
// worker が途中で落ちた通知は、この時間が過ぎた後の再配達で取り直す。
// 1 回の処理は Claude API の 30 秒 × 2 回程度なので、十分に長い。
const staleClaim = 15 * time.Minute

// handleNotification answers a mention or a reply to the bot.
//
// **冪等に書く。** 通知は at-least-once で届くので、通知の ID ごとに処理の
// 状態を storage に持つ。
//   - 処理を始める前に ID を取る。取れなかったら (処理中・処理済み) 何もしない
//   - エラーを返すとき (本体の 5xx など) は failed にして、再試行で取り直せる
//     ようにする。Claude が書いた本文は保存しておき、再試行では呼び直さない
//     (二重に課金しない)
//
// 投稿した後で done にする前に落ちると、再配達で同じ返事をもう一度投稿する。
// この窓は storage と本体の API を 1 つのトランザクションに入れられない以上
// 閉じられない。
func (b *bot) handleNotification(ctx context.Context, ev plugin.Notification) error {
	switch ev.Type {
	case plugin.NotificationMention, plugin.NotificationReply:
	default:
		// チャット (DM) や引用、知らない種類には反応しない。
		return nil
	}
	if ev.NoteID == "" || ev.ID == "" {
		return nil
	}
	// 公開範囲が「指名」(ダイレクト) の投稿でのメンションは DM として扱い、
	// 返事もリアクションもしない。投稿を読みにも行かない。
	if ev.NoteVisibility == "specified" {
		return nil
	}
	db := b.db()
	d, claimed, err := claimNotification(ctx, db, ev.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	outcome, err := b.respond(ctx, ev, d)
	if err != nil {
		if _, ferr := db.ExecContext(context.WithoutCancel(ctx),
			`UPDATE handled_notifications SET status = 'failed', outcome = $2 WHERE id = $1`, ev.ID, err.Error()); ferr != nil {
			b.log.Error("claude-bot: 通知の状態を戻せません", "err", ferr)
		}
		return err
	}
	_, err = db.ExecContext(ctx,
		`UPDATE handled_notifications SET status = 'done', outcome = $2, finished_at = $3 WHERE id = $1`, ev.ID, outcome, now())
	return err
}

// draft is a reply written by a previous failed attempt.
type draft struct {
	// Text is empty when nothing was written yet.
	Text string
	// Scope is the scope computed when Text was written.
	Scope replyScope
}

// claimNotification takes the notification for this worker. It returns the
// draft saved by a previous failed attempt, if any.
func claimNotification(ctx context.Context, db *sql.DB, id string) (d draft, claimed bool, err error) {
	t := now()
	err = db.QueryRowContext(ctx, `
INSERT INTO handled_notifications (id, status, claimed_at) VALUES ($1, 'processing', $2)
ON CONFLICT (id) DO UPDATE SET status = 'processing', claimed_at = EXCLUDED.claimed_at
WHERE handled_notifications.status = 'failed'
   OR (handled_notifications.status = 'processing' AND handled_notifications.claimed_at < $3)
RETURNING COALESCE(reply_text, ''), COALESCE(reply_visibility, ''), reply_local_only`,
		id, t, t.Add(-staleClaim)).Scan(&d.Text, &d.Scope.Visibility, &d.Scope.LocalOnly)
	if errors.Is(err, sql.ErrNoRows) {
		return draft{}, false, nil
	}
	if err != nil {
		return draft{}, false, fmt.Errorf("claim notification: %w", err)
	}
	return d, true, nil
}

// respond does the work for one notification and returns a short outcome
// for the bookkeeping. 返したエラーは通知の再試行になる。
func (b *bot) respond(ctx context.Context, ev plugin.Notification, d draft) (string, error) {
	db := b.db()
	s, err := loadSettings(ctx, db)
	if err != nil {
		return "", err
	}
	if s.Reply.Mode == "off" {
		return "off", nil
	}
	as := b.pctx.API().AsUser(ev.AccountID)

	note, err := call[noteView](ctx, as, "notes/show", map[string]any{"noteId": ev.NoteID})
	if err != nil {
		if isClientError(err) {
			// 消された・見えなくなった投稿。呼び直しても変わらない。
			return "note_unavailable", nil
		}
		return "", fmt.Errorf("notes/show: %w", err)
	}
	// 通知に公開範囲が載っていない場合と、通知の後に変わった場合のために、
	// 読んだ投稿でも確かめる。
	if note.Visibility == "specified" {
		return "dm_ignored", nil
	}
	if note.User.ID == ev.AccountID {
		return "self", nil
	}
	if note.User.IsBot {
		// bot どうしで返事をし合うと止まらない。
		b.logEvent(ctx, "info", eventSkipped, "botの印が付いた相手には返事をしません。", note.User.ID, note.ID)
		return "bot_ignored", nil
	}
	allowed, err := b.audienceAllows(ctx, as, s.Reply.Audience, note.User)
	if err != nil {
		return "", err
	}
	if !allowed {
		b.logEvent(ctx, "info", eventSkipped, "話しかけられる人の範囲の外なので、反応しませんでした。", note.User.ID, note.ID)
		return "audience", nil
	}

	// リアクションの対象はメンションされた投稿だけ (bot の投稿への返信には
	// 付けない)。
	mentioned := ev.Type == plugin.NotificationMention
	if s.Reply.Mode == "reaction" {
		if !mentioned {
			return "not_mention", nil
		}
		b.react(ctx, as, note)
		return "reacted", nil
	}

	th, err := b.thread(ctx, as, note)
	if err != nil {
		return "", err
	}
	if max := s.Reply.MaxRoundTrips; max > 0 {
		n, err := b.roundTrips(ctx, th, ev.AccountID)
		if err != nil {
			return "", err
		}
		if n >= max {
			b.logEvent(ctx, "info", eventSkipped,
				fmt.Sprintf("このスレッドでは既に%d回返事をしたので、これ以上は返事をしません。", max), note.User.ID, note.ID)
			return "round_trips", nil
		}
	}
	ctxNotes := contextFor(th.visible, note.User.ID)
	scope := replyScopeFor(s.Reply.Visibility, note, ctxNotes)

	maxNote := b.maxNoteLength(ctx)
	prefix := note.User.acct() + " "
	// 保存した下書きも無効にし直す。古い版が保存した下書きは、今の規則で崩されて
	// いないことがある (#4)。崩した @ の直後は幅の無い空白なので、2 回かけても変わらない。
	text := defangMentions(d.Text)
	if text != "" {
		// 本文を書いたときの公開範囲より広げない。再試行の間に文脈の投稿が
		// 消えると、計算し直した範囲は広がりうる。
		scope = narrower(scope, d.Scope)
	} else {
		reason, err := checkLimits(ctx, db, s, note.User.ID, note.User.remoteHost())
		if err != nil {
			return "", err
		}
		if reason != "" {
			return b.overLimit(ctx, as, s, note, prefix, reason)
		}
		// 下書きがある再試行ではClaudeを呼ばないので、画像もここでだけ取る。
		vr := b.collectImages(ctx, s.Vision, note, ctxNotes, s.Reply.ContextNotes)
		text, err = b.generate(ctx, s, generation{
			Kind:      "reply",
			UserID:    note.User.ID,
			Host:      note.User.remoteHost(),
			NoteID:    note.ID,
			System:    s.Reply.SystemPrompt,
			Prompt:    replyPrompt(ctxNotes, note, ev.AccountID, s.Reply.ContextNotes, vr),
			Images:    vr.imageList(),
			MaxChars:  s.Reply.MaxChars,
			MaxTokens: s.Reply.MaxTokens,
			PostLimit: maxNote - utf8.RuneCountInString(prefix),
			Recipient: &note.User,
		})
		if errors.Is(err, errSilent) {
			return "silent", nil
		}
		if err != nil {
			// errTransient も含めて、通知の再試行に任せる。何も投稿していない
			// ので、やり直しても二重にはならない。
			return "", err
		}
		// 投稿に失敗して再試行されたとき、Claude を呼び直さずに済むよう残す。
		if _, err := db.ExecContext(ctx, `
UPDATE handled_notifications SET reply_text = $2, reply_visibility = $3, reply_local_only = $4 WHERE id = $1`,
			ev.ID, text, scope.Visibility, scope.LocalOnly); err != nil {
			return "", fmt.Errorf("save draft: %w", err)
		}
	}
	if strings.HasPrefix(text, "```") {
		// 宛先と同じ行に続くと、パーサーはコードブロックとして読まない (行頭で
		// 始まるものだけを読む)。見た目を崩さないよう、宛先の後で改行する。
		prefix = strings.TrimRight(prefix, " ") + "\n"
	}
	outcome, err := b.postReply(ctx, as, note, scope, prefix+text, "replied")
	if err != nil {
		return "", err
	}
	if outcome == "replied" {
		// 投稿は済んでいるので、記録に失敗してもエラーにしない (再試行させると
		// 二重に投稿する)。
		if _, err := db.ExecContext(ctx, `
INSERT INTO bot_replies (notification_id, root_id, user_id, at) VALUES ($1, $2, $3, $4)
ON CONFLICT (notification_id) DO NOTHING`, ev.ID, th.rootID, note.User.ID, now()); err != nil {
			b.log.Error("claude-bot: 返事の記録を保存できません", "err", err)
		}
	}
	// 両方を選んだときのリアクションは、返事を投稿できたときだけ付ける。
	// Claude API のエラーなどで沈黙するときは、リアクションもしない。
	if s.Reply.Mode == "both" && outcome == "replied" && mentioned {
		b.react(ctx, as, note)
	}
	return outcome, nil
}

// replyScope is the visibility of a reply.
type replyScope struct {
	Visibility string
	LocalOnly  bool
}

// replyScopeFor narrows the default visibility so the reply is never wider
// than the note it answers, nor than any note whose text went to the model.
//
// 返事の本文には文脈として送った投稿の内容が混ざりうる。文脈に入れた投稿の
// うち最も狭い公開範囲に合わせ、連合しない投稿が 1 つでもあれば返事も
// 連合させない。
func replyScopeFor(def string, note noteView, ctxNotes []noteView) replyScope {
	sc := replyScope{Visibility: clampVisibility(def, note.Visibility), LocalOnly: note.LocalOnly}
	for _, n := range ctxNotes {
		sc.Visibility = clampVisibility(sc.Visibility, n.Visibility)
		sc.LocalOnly = sc.LocalOnly || n.LocalOnly
	}
	return sc
}

// narrower returns the narrower of two scopes. 空の公開範囲 (記録が無い) は
// 狭める材料にしない。
func narrower(a, b replyScope) replyScope {
	out := replyScope{Visibility: a.Visibility, LocalOnly: a.LocalOnly || b.LocalOnly}
	if b.Visibility != "" {
		out.Visibility = clampVisibility(a.Visibility, b.Visibility)
	}
	return out
}

// postReply posts text as a reply to note.
func (b *bot) postReply(ctx context.Context, as plugin.Caller, note noteView, scope replyScope, text, outcome string) (string, error) {
	params := map[string]any{
		"text":       text,
		"replyId":    note.ID,
		"visibility": scope.Visibility,
	}
	if scope.LocalOnly {
		params["localOnly"] = true
	}
	if _, err := as.Call(ctx, "notes/create", params); err != nil {
		if isClientError(err) {
			b.logEvent(ctx, "error", eventPostError, "返事を投稿できませんでした: "+err.Error(), note.User.ID, note.ID)
			return "post_rejected", nil
		}
		return "", fmt.Errorf("notes/create: %w", err)
	}
	return outcome, nil
}

// overLimit handles a mention that arrives over a cost limit.
//
// 断りの文を返す設定でも、同じ相手には 1 時間に 1 回までにする。
// 上限を超えた相手が話しかけ続けると、断りの文で埋まるため。
func (b *bot) overLimit(ctx context.Context, as plugin.Caller, s Settings, note noteView, prefix, reason string) (string, error) {
	if s.Limits.OverLimit != "message" {
		b.logEvent(ctx, "warn", eventSilenced, "費用の上限のため、返事をしませんでした: "+reason, note.User.ID, note.ID)
		return "limited", nil
	}
	var recent bool
	if err := b.db().QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM events WHERE kind = $1 AND user_id = $2 AND at > $3)`,
		eventRefused, note.User.ID, now().Add(-time.Hour)).Scan(&recent); err != nil {
		return "", fmt.Errorf("refused recently: %w", err)
	}
	if recent {
		b.logEvent(ctx, "warn", eventSilenced, "費用の上限のため、返事をしませんでした (断りの文は1時間以内に送り済み): "+reason, note.User.ID, note.ID)
		return "limited", nil
	}
	b.logEvent(ctx, "warn", eventRefused, "費用の上限のため、決まった文で断りました: "+reason, note.User.ID, note.ID)
	// 断りの文は文脈を含まないので、元の投稿だけに合わせる。
	return b.postReply(ctx, as, note, replyScopeFor(s.Reply.Visibility, note, nil), prefix+s.Limits.OverLimitMessage, "refused")
}

// audienceAllows applies "who can talk to the bot".
func (b *bot) audienceAllows(ctx context.Context, as plugin.Caller, audience string, u userLite) (bool, error) {
	switch audience {
	case "local":
		return u.isLocal(), nil
	case "followers":
		rel, err := call[struct {
			IsFollowed bool `json:"isFollowed"`
		}](ctx, as, "users/relation", map[string]any{"userId": u.ID})
		if err != nil {
			if isClientError(err) {
				return false, nil
			}
			return false, fmt.Errorf("users/relation: %w", err)
		}
		return rel.IsFollowed, nil
	default:
		return true, nil
	}
}

// emojiName matches ":name:" in a note. ":name@host:" (リモートの絵文字の
// 書き方) は @ を含むので当たらない。
var emojiName = regexp.MustCompile(`:([\p{L}\p{N}\p{M}_+-]+):`)

const defaultReaction = "👍"

// pickReaction chooses the reaction for note.
//
// メンションの本文にある絵文字と同じ名前の絵文字がこのサーバーにあれば
// それを、無ければ 👍 を使う。ロールで使える人が絞られている絵文字は、
// bot が使えるとは限らないので選ばない。
func (b *bot) pickReaction(ctx context.Context, note noteView) string {
	seen := map[string]bool{}
	for _, m := range emojiName.FindAllStringSubmatch(note.text(), -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if len(seen) > 10 {
			break
		}
		e, err := call[struct {
			Name  string   `json:"name"`
			Roles []string `json:"roleIdsThatCanBeUsedThisEmojiAsReaction"`
		}](ctx, b.pctx.API().Anonymous(), "emoji", map[string]any{"name": name})
		if err != nil {
			// 無い絵文字 (NO_SUCH_EMOJI) も、引けなかったときも、次の候補へ。
			continue
		}
		if len(e.Roles) > 0 {
			continue
		}
		return ":" + name + ":"
	}
	return defaultReaction
}

// react adds the reaction to note.
//
// リアクションは失敗しても記録だけにして、通知を再試行させない。返事を
// 投稿した後に再試行させると、返事を二重に投稿することになるため。
func (b *bot) react(ctx context.Context, as plugin.Caller, note noteView) {
	reaction := b.pickReaction(ctx, note)
	_, err := as.Call(ctx, "notes/reactions/create", map[string]any{"noteId": note.ID, "reaction": reaction})
	if err == nil || apiErrorCode(err) == "ALREADY_REACTED" {
		return
	}
	b.logEvent(ctx, "warn", eventReactError, "リアクションできませんでした: "+err.Error(), note.User.ID, note.ID)
}

// threadView is what the bot read of the thread above a note.
type threadView struct {
	// visible holds the ancestors the bot can see, oldest first.
	visible []noteView
	// rootID is the oldest ancestor found (the note itself if it has no
	// parent). 往復の上限をスレッドごとに数えるのに使う。
	rootID string
}

// threadDepth is how many ancestors are read. notes/conversation の上限。
const threadDepth = 100

// thread reads the ancestors of note as the bot.
func (b *bot) thread(ctx context.Context, as plugin.Caller, note noteView) (threadView, error) {
	th := threadView{rootID: note.ID}
	if note.ReplyID == nil {
		return th, nil
	}
	// 根を知るため、送る数に関係なく上限まで取る。
	ancestors, err := call[[]noteView](ctx, as, "notes/conversation", map[string]any{"noteId": note.ID, "limit": threadDepth})
	if err != nil {
		if isClientError(err) {
			return th, nil
		}
		return th, fmt.Errorf("notes/conversation: %w", err)
	}
	if len(ancestors) > 0 {
		th.rootID = ancestors[len(ancestors)-1].ID
	}
	// notes/conversation は近い親から順に返す。古い順に並べ直す。
	for i := len(ancestors) - 1; i >= 0; i-- {
		// bot が読めない投稿は本体が中身を消して返す (isHidden)。
		if ancestors[i].IsHidden {
			continue
		}
		th.visible = append(th.visible, ancestors[i])
	}
	return th, nil
}

// contextFor picks the ancestors whose text may be sent to the model for a
// reply to requester.
//
// **bot が読めることと、話しかけた人が読めることは違う。** bot はメンション
// されたフォロワー限定の投稿なども読めるので、そのまま文脈に入れると、
// 読む権利の無い人への返事にその内容が漏れる。入れるのは、誰でも読める
// 投稿 (パブリックかホームで、連合しないものを除く) と、話しかけた人自身の
// 投稿 (指名を除く) だけにする。bot 自身の過去の返事も同じ規則に従う。
func contextFor(visible []noteView, requester string) []noteView {
	out := make([]noteView, 0, len(visible))
	for _, n := range visible {
		if n.Visibility == "specified" {
			continue
		}
		open := (n.Visibility == "public" || n.Visibility == "home") && !n.LocalOnly
		if open || n.User.ID == requester {
			out = append(out, n)
		}
	}
	return out
}

// roundTrips counts the bot's replies in the thread.
//
// 記録した返事 (スレッドの根ごと) と、読めた祖先の中の bot の投稿の多い方を
// 使う。祖先の窓だけで数えると、相手が自分の投稿を挟んで bot の返事を窓の
// 外へ押し出せるため。
func (b *bot) roundTrips(ctx context.Context, th threadView, botID string) (int, error) {
	var stored int
	if err := b.db().QueryRowContext(ctx, `SELECT count(*) FROM bot_replies WHERE root_id = $1`, th.rootID).Scan(&stored); err != nil {
		return 0, fmt.Errorf("round trips: %w", err)
	}
	if n := countBy(th.visible, botID); n > stored {
		return n, nil
	}
	return stored, nil
}

func countBy(notes []noteView, userID string) int {
	n := 0
	for _, x := range notes {
		if x.User.ID == userID {
			n++
		}
	}
	return n
}

var promptEscaper = strings.NewReplacer("<", "&lt;", ">", "&gt;")

// promptThread returns the ancestors replyPrompt sends.
//
// 送るのは返事をする投稿を含めて contextNotes 件まで。thread は往復の
// 上限を数えるために深めに取ってあるので、新しい方を残して切る。
func promptThread(thread []noteView, contextNotes int) []noteView {
	keep := contextNotes - 1
	if keep < 0 {
		keep = 0
	}
	if len(thread) > keep {
		return thread[len(thread)-keep:]
	}
	return thread
}

// replyPrompt renders the thread as the user message.
//
// スレッドは 1 つの user メッセージにまとめる。bot の過去の投稿を
// assistant として並べると、相手の投稿が続く・bot の投稿で始まるなどで
// 役割の交互の並びを組み直す必要があり、prefill の禁止にも触れやすい。
//
// vrは送る画像(#6)。nil(visionがOFF)なら、添付は件数だけを書く。
func replyPrompt(thread []noteView, note noteView, botID string, contextNotes int, vr *visionResult) string {
	ctxNotes := promptThread(thread, contextNotes)
	var sb strings.Builder
	sb.WriteString("以下は、あなたが参加しているスレッドの投稿を古い順に並べたものです。\n")
	if len(vr.imageList()) > 0 {
		sb.WriteString("投稿に添付された画像は、このメッセージの先頭に「画像N」のラベルを付けて並べてあります。投稿の中の[画像N]がその画像です。\n")
	}
	sb.WriteString("<thread>\n")
	for _, n := range append(append([]noteView{}, ctxNotes...), note) {
		author := n.User.acct()
		if n.User.ID == botID {
			author = "あなた (" + author + ")"
		}
		body := n.text()
		if n.User.ID == botID {
			// 自分の過去の返事は、こちらで付けた宛先から始まっている。そのまま
			// 見せると Claude がその形をまねて、本文にも宛先を書く (#4)。
			body = stripLeadingMentions(body, anyMention)
		}
		if n.CW != nil && *n.CW != "" {
			body = "[注意書き: " + *n.CW + "]\n" + body
		}
		body += attachmentText(n, vr)
		fmt.Fprintf(&sb, "<post author=\"%s\">\n%s\n</post>\n", promptEscaper.Replace(author), promptEscaper.Replace(body))
	}
	fmt.Fprintf(&sb, "</thread>\n最後の投稿 (%s) への返事を書いてください。宛先のメンション (@名前) は書かないでください。こちらで付けます。", note.User.acct())
	return sb.String()
}
