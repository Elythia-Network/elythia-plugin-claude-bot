package claudebot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/plugin"

	"github.com/elythia-network/elythia-plugin-claude-bot/reversi"
)

/*
 * リバーシの対局 (#24)。
 *
 * 招待と対局の進みは通知で届き、手は本体の REST (reversi/ready・put-stone・
 * claim-time-is-up) で打つ (本体の #3942)。通知は中身を持たず、重複も順序の
 * 入れ替わりもあるので、届くたびに対局を読み直し、その時点で要ることだけを
 * する。届いた順に手を数えない。
 *
 * **受けると決めるまで reversi/show-game を呼ばない。** 本体の show-game は、
 * 招待された側が始まる前の対局を開くと、連合の招待者へ受諾 (Join) を配る。
 * 受けるかを決めるまでは、副作用の無い reversi/games で読む。受けると決めたら、
 * この show-game で受ける。reversi/match では受けない。招待が読んだ後で消えて
 * いると、match は bot から相手への新しい招待を作ってしまう。
 *
 * 探索は通知の handler では行わず、job に分ける。handler は短く保たないと、
 * 同時に 2 本しか動かないキューで他の通知を待たせる。job も同じく読み直し、
 * 積んだときの手数 (ply) からずれていたら何もしない。
 *
 * 記録 (reversi_games) の無い対局には何もしない。botが受けた対局だけを進める。
 * 記録を閉じた後も本体で対局が続いていたら (掃除や競合で閉じたとき)、準備を
 * 外すか投了して、本体の側も終わらせる。
 */

const (
	jobReversiMove         = "reversi-move"
	jobReversiTimeout      = "reversi-timeout"
	jobReversiReadyTimeout = "reversi-ready-timeout"
)

// The statuses of a reversi_games row.
const (
	reversiWaiting  = "waiting"
	reversiPlaying  = "playing"
	reversiEnded    = "ended"
	reversiCanceled = "canceled"
)

const (
	// reversiTimeoutGrace is added to the end of a turn before the bot claims
	// the opponent ran out of time. 早すぎた申告は本体が何もしないだけ。
	reversiTimeoutGrace = 5 * time.Second
	// reversiTimeoutRetry is how long the bot waits to claim again when the
	// claim did not end the game.
	reversiTimeoutRetry = 10 * time.Second
	// reversiTimeoutAttempts caps the claims for one turn.
	reversiTimeoutAttempts = 3
	// reversiJobAttempts is how many times a reversi job is tried. 本体は既定で
	// job を再試行しないので、5xx が 1 回出ただけで bot が打たなくなる。
	reversiJobAttempts = 3
	// reversiMoveDedup suppresses the same move job enqueued again by another
	// notification for the same turn. 重複の印は job が終わった後も期限まで
	// 残るので、短くする。
	reversiMoveDedup = time.Minute
	// reversiMinThink is the least time the bot thinks, even when the turn is
	// almost over. 探索は 1 手先の評価だけは必ず済ませる。
	reversiMinThink = 50 * time.Millisecond
	// reversiStale is how long a game may stay without any change before the
	// tick gives up its record. 通知や job が失われても、同時の対局の枠が
	// 埋まり続けないようにする。
	reversiStale = 24 * time.Hour
	// reversiMinTurnSeconds and reversiMaxTurnSeconds bound the time limit of
	// a turn the bot plays with. 短いと、通知とキューの遅れで bot が時間切れに
	// なる。0 は本体がタイマーを張らないので、相手が申告するとすぐに bot の
	// 負けになる。長いと、相手が手を止めるだけで同時の対局の枠を埋められ、
	// 掃除 (reversiStale) とも合わなくなる。
	reversiMinTurnSeconds = 15
	reversiMaxTurnSeconds = 3600
	// reversiGamesScan is how many of the bot's recent games are read to find
	// an invitation.
	reversiGamesScan = 100
)

// reversiGameView is the part of the host's packed reversi game the bot reads.
type reversiGameView struct {
	ID                   string          `json:"id"`
	User1ID              string          `json:"user1Id"`
	User2ID              string          `json:"user2Id"`
	User1Ready           bool            `json:"user1Ready"`
	User2Ready           bool            `json:"user2Ready"`
	Black                *int            `json:"black"`
	IsStarted            bool            `json:"isStarted"`
	IsEnded              bool            `json:"isEnded"`
	StartedAt            *time.Time      `json:"startedAt"`
	WinnerID             *string         `json:"winnerId"`
	TimeLimitForEachTurn int             `json:"timeLimitForEachTurn"`
	Logs                 json.RawMessage `json:"logs"`
	Map                  []string        `json:"map"`
	IsLlotheo            bool            `json:"isLlotheo"`
	CanPutEverywhere     bool            `json:"canPutEverywhere"`
	LoopedBoard          bool            `json:"loopedBoard"`
	User1                *userLite       `json:"user1"`
	User2                *userLite       `json:"user2"`
}

func (g reversiGameView) ready(id string) bool {
	if id == g.User1ID {
		return g.User1Ready
	}
	return g.User2Ready
}

// playable reports whether the bot can play g: the standard board, and a time
// limit within reversiMinTurnSeconds..reversiMaxTurnSeconds.
func (g reversiGameView) playable() error {
	r := reversi.Rules{Map: g.Map, IsLlotheo: g.IsLlotheo, CanPutEverywhere: g.CanPutEverywhere, LoopedBoard: g.LoopedBoard}
	if err := reversi.Supports(r); err != nil {
		return err
	}
	if g.TimeLimitForEachTurn < reversiMinTurnSeconds || g.TimeLimitForEachTurn > reversiMaxTurnSeconds {
		return fmt.Errorf("%w: the time limit of a turn is %d seconds (%d..%d)",
			reversi.ErrUnsupported, g.TimeLimitForEachTurn, reversiMinTurnSeconds, reversiMaxTurnSeconds)
	}
	return nil
}

// color returns the color of player id. black は黒になる利用者の番号 (1 か 2)。
func (g reversiGameView) color(id string) (reversi.Color, error) {
	if g.Black == nil || (*g.Black != 1 && *g.Black != 2) {
		return reversi.Black, fmt.Errorf("reversi game %s: black is not set", g.ID)
	}
	if (*g.Black == 1) == (id == g.User1ID) {
		return reversi.Black, nil
	}
	return reversi.White, nil
}

// logs decodes the logs.
func (g reversiGameView) logs() ([][]int, error) {
	if len(g.Logs) == 0 || string(g.Logs) == "null" {
		return nil, nil
	}
	var logs [][]int
	if err := json.Unmarshal(g.Logs, &logs); err != nil {
		return nil, fmt.Errorf("reversi game %s: decode logs: %w", g.ID, err)
	}
	return logs, nil
}

// positions reads the moves from the logs.
//
// 本体の形は [timeDelta, player, operation(0 = 置く), pos]。古い [pos, color]
// の形も本体は読むので、ここでも読む。
func (g reversiGameView) positions() ([]int, error) {
	logs, err := g.logs()
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(logs))
	for _, e := range logs {
		switch {
		case len(e) >= 4 && e[2] == 0:
			out = append(out, e[3])
		case len(e) == 2:
			out = append(out, e[0])
		}
	}
	return out, nil
}

// turnDeadline returns when the current turn runs out.
//
// 手番は最後の手 (無ければ開始) から持ち時間だけ続く。本体は各手の時刻を、
// 最初の手は Unix ミリ秒、以降は前の手からの差で記録する。古い形の手が混じって
// いて時刻が分からないときと、開始の時刻も無いときは、今から数える。
func (g reversiGameView) turnDeadline() time.Time {
	limit := time.Duration(g.TimeLimitForEachTurn) * time.Second
	start := now()
	if g.StartedAt != nil {
		start = *g.StartedAt
	}
	logs, err := g.logs()
	if err == nil && len(logs) > 0 {
		var ms int64
		timed := true
		for _, e := range logs {
			if len(e) < 4 {
				timed = false
				break
			}
			ms += int64(e[0])
		}
		if timed {
			start = time.UnixMilli(ms)
		}
	}
	return start.Add(limit)
}

// replay rebuilds the board. ply is the number of moves played.
func (g reversiGameView) replay() (reversi.Game, int, error) {
	pos, err := g.positions()
	if err != nil {
		return reversi.Game{}, 0, err
	}
	game, err := reversi.Replay(pos)
	if err != nil {
		return reversi.Game{}, 0, fmt.Errorf("reversi game %s: %w", g.ID, err)
	}
	return game, len(pos), nil
}

// reversiRecord is a reversi_games row.
type reversiRecord struct {
	GameID string
	Level  int
	Status string
}

func (r reversiRecord) open() bool { return r.Status == reversiWaiting || r.Status == reversiPlaying }

func loadReversiRecord(ctx context.Context, db *sql.DB, gameID string) (reversiRecord, bool, error) {
	r := reversiRecord{GameID: gameID}
	err := db.QueryRowContext(ctx, `SELECT level, status FROM reversi_games WHERE game_id = $1`, gameID).Scan(&r.Level, &r.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("load reversi game: %w", err)
	}
	return r, true, nil
}

// reversiMoveJob is the payload of jobReversiMove and jobReversiTimeout.
type reversiMoveJob struct {
	AccountID string `json:"accountId"`
	GameID    string `json:"gameId"`
	// Ply is the number of moves played when the job was enqueued.
	Ply int `json:"ply"`
	// Attempt counts the claims (jobReversiTimeout only).
	Attempt int `json:"attempt,omitempty"`
}

// reversiReadyJob is the payload of jobReversiReadyTimeout.
type reversiReadyJob struct {
	AccountID string `json:"accountId"`
	GameID    string `json:"gameId"`
}

// reversiJobPayload is a payload of the reversi jobs.
type reversiJobPayload interface {
	reversiMoveJob | reversiReadyJob
}

// decodeReversiJob decodes the payload of job name. 形の違う job は繰り返しても
// 直らないので、再試行しない。
func decodeReversiJob[T reversiJobPayload](name string, raw json.RawMessage) (T, error) {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, plugin.NoRetry(fmt.Errorf("decode %s: %w", name, err))
	}
	var account, game string
	switch v := any(p).(type) {
	case reversiMoveJob:
		account, game = v.AccountID, v.GameID
	case reversiReadyJob:
		account, game = v.AccountID, v.GameID
	}
	if account == "" || game == "" {
		return p, plugin.NoRetry(fmt.Errorf("%s: accountId or gameId is empty", name))
	}
	return p, nil
}

// enqueueReversi enqueues a reversi job that is tried reversiJobAttempts
// times.
func (b *bot) enqueueReversi(ctx context.Context, name string, payload any, opts ...plugin.EnqueueOption) error {
	return b.pctx.Queue().Enqueue(ctx, name, payload, append(opts, plugin.WithMaxAttempts(reversiJobAttempts))...)
}

// showGame reads a game the bot accepted. 招待された側が始まる前の対局を読むと、
// 本体は連合の招待者へ受諾を配るので、受けると決める前には呼ばないこと。
func (b *bot) showGame(ctx context.Context, as plugin.Caller, gameID string) (reversiGameView, error) {
	return call[reversiGameView](ctx, as, "reversi/show-game", map[string]any{"gameId": gameID})
}

// onReversi handles a reversi notification.
func (b *bot) onReversi(ctx context.Context, ev plugin.Notification) error {
	if ev.GameID == "" || ev.AccountID == "" {
		return nil
	}
	rec, ok, err := loadReversiRecord(ctx, b.db(), ev.GameID)
	if err != nil {
		return err
	}
	if !ok {
		if ev.Type != plugin.NotificationReversiInvited {
			return nil
		}
		return b.acceptReversi(ctx, ev.AccountID, ev.GameID)
	}
	return b.readAndAdvance(ctx, ev.AccountID, rec)
}

// readAndAdvance reads a recorded game and does what it needs now.
func (b *bot) readAndAdvance(ctx context.Context, accountID string, rec reversiRecord) error {
	as := b.pctx.API().AsUser(accountID)
	g, err := b.showGame(ctx, as, rec.GameID)
	if err != nil {
		if isClientError(err) {
			// 取り消された対局は読めない (本体は行を消す)。
			_, err := b.closeReversi(ctx, rec.GameID, reversiCanceled)
			return err
		}
		return fmt.Errorf("reversi/show-game: %w", err)
	}
	if !rec.open() {
		return b.settleClosed(ctx, as, accountID, g)
	}
	return b.advanceReversi(ctx, as, accountID, rec, g)
}

// findInvitation reads the game gameID from the bot's recent games, without
// the side effect of reversi/show-game. 見つからなければ ok は false。
func (b *bot) findInvitation(ctx context.Context, as plugin.Caller, gameID string) (reversiGameView, bool, error) {
	games, err := call[[]reversiGameView](ctx, as, "reversi/games", map[string]any{"my": true, "limit": reversiGamesScan})
	if err != nil {
		return reversiGameView{}, false, fmt.Errorf("reversi/games: %w", err)
	}
	for _, g := range games {
		if g.ID == gameID {
			return g, true, nil
		}
	}
	return reversiGameView{}, false, nil
}

// acceptReversi decides whether to accept the invitation to gameID, and
// accepts it when it does.
func (b *bot) acceptReversi(ctx context.Context, accountID, gameID string) error {
	db := b.db()
	s, err := loadSettings(ctx, db)
	if err != nil {
		return err
	}
	rs := s.Reversi
	if !rs.Enabled {
		return nil
	}
	as := b.pctx.API().AsUser(accountID)
	g, found, err := b.findInvitation(ctx, as, gameID)
	if err != nil || !found {
		return err
	}
	if g.IsStarted || g.IsEnded || g.User2ID != accountID {
		return nil
	}
	opp, err := b.reversiOpponent(ctx, as, g)
	if err != nil {
		return err
	}
	ok, err := b.audienceAllows(ctx, as, rs.Audience, opp)
	if err != nil {
		return err
	}
	if !ok {
		b.logEvent(ctx, "info", eventReversi, reversiGameMessage("招待を受ける相手の範囲の外なので、リバーシの招待を受けませんでした", g.ID), opp.ID, "")
		return nil
	}
	if err := g.playable(); err != nil {
		b.logEvent(ctx, "info", eventReversi, reversiGameMessage("対応していない対局なので、リバーシの招待を受けませんでした: "+err.Error(), g.ID), opp.ID, "")
		return nil
	}
	inserted, reason, err := insertReversiGame(ctx, db, accountID, g.ID, opp, rs)
	if err != nil || !inserted {
		if reason != "" {
			b.logEvent(ctx, "info", eventReversi, reversiGameMessage(reason, g.ID), opp.ID, "")
		}
		// 理由が無いのは、同じ招待の通知が並行して届き、もう一方が先に記録した
		// とき。受けるのはそちらに任せる。
		return err
	}
	b.logEvent(ctx, "info", eventReversi, reversiGameMessage(fmt.Sprintf("%s からのリバーシの招待を受けました (強さ %d)", opp.acct(), rs.Level), g.ID), opp.ID, "")
	err = b.enqueueReversi(ctx, jobReversiReadyTimeout, reversiReadyJob{AccountID: accountID, GameID: g.ID},
		plugin.WithDelay(time.Duration(rs.ReadyTimeoutMinutes)*time.Minute))
	if err != nil {
		// 積めなくても対局は進められる。始まらない対局は tick の掃除が閉じる。
		b.logEvent(ctx, "warn", eventReversi, reversiGameMessage("準備を待つ期限のjobを積めませんでした: "+err.Error(), g.ID), opp.ID, "")
	}
	// 読み直すと、本体が招待者へ受諾を配る。失敗したら通知の再試行で、記録の
	// ある対局として読み直す。
	return b.readAndAdvance(ctx, accountID, reversiRecord{GameID: g.ID, Level: rs.Level, Status: reversiWaiting})
}

// reversiOpponent returns the inviter of g (the bot is user2).
func (b *bot) reversiOpponent(ctx context.Context, as plugin.Caller, g reversiGameView) (userLite, error) {
	if g.User1 != nil {
		return *g.User1, nil
	}
	got, err := call[userLite](ctx, as, "users/show", map[string]any{"userId": g.User1ID})
	if err != nil {
		return userLite{}, fmt.Errorf("users/show: %w", err)
	}
	return got, nil
}

// insertReversiGame records an accepted game. When it did not, reason says
// why it refused, or is "" when the game was already recorded.
//
// 同時の対局の数は、数えてから足すまでの間に別の招待が割り込まないよう、
// advisory lock で直列にする。相手ごとに 1 局は部分 unique index でも守る。
func insertReversiGame(ctx context.Context, db *sql.DB, accountID, gameID string, opp userLite, rs ReversiSettings) (inserted bool, reason string, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Commit の後は何もしない
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('claude-bot:reversi'))`); err != nil {
		return false, "", fmt.Errorf("lock reversi games: %w", err)
	}
	var active int
	var withOpp bool
	if err := tx.QueryRowContext(ctx, `
SELECT count(*), coalesce(bool_or(opponent_id = $1), false) FROM reversi_games WHERE status IN ('waiting', 'playing')`,
		opp.ID).Scan(&active, &withOpp); err != nil {
		return false, "", fmt.Errorf("count reversi games: %w", err)
	}
	if withOpp {
		return false, "同じ相手と対局中なので、リバーシの招待を受けませんでした", nil
	}
	if active >= rs.MaxGames {
		return false, fmt.Sprintf("同時に進める対局の数 (%d) に達しているので、リバーシの招待を受けませんでした", rs.MaxGames), nil
	}
	t := now()
	res, err := tx.ExecContext(ctx, `
INSERT INTO reversi_games (game_id, account_id, opponent_id, opponent_acct, level, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 'waiting', $6, $6) ON CONFLICT (game_id) DO NOTHING`,
		gameID, accountID, opp.ID, opp.acct(), rs.Level, t)
	if err != nil {
		return false, "", fmt.Errorf("insert reversi game: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, "", err
	}
	if err := tx.Commit(); err != nil {
		return false, "", fmt.Errorf("commit: %w", err)
	}
	return true, "", nil
}

// setReady sets the bot's ready state. 4xx (始まった・取り消されたなど) は、
// 次の通知で読み直すので成功として扱う。
func setReady(ctx context.Context, as plugin.Caller, gameID string, ready bool) error {
	if _, err := as.Call(ctx, "reversi/ready", map[string]any{"gameId": gameID, "ready": ready}); err != nil && !isClientError(err) {
		return fmt.Errorf("reversi/ready: %w", err)
	}
	return nil
}

// surrender gives up a game in progress.
func (b *bot) surrender(ctx context.Context, as plugin.Caller, g reversiGameView, why string) error {
	b.logEvent(ctx, "warn", eventReversi, reversiGameMessage(why, g.ID), g.User1ID, "")
	if _, err := as.Call(ctx, "reversi/surrender", map[string]any{"gameId": g.ID}); err != nil && !isClientError(err) {
		return fmt.Errorf("reversi/surrender: %w", err)
	}
	return nil
}

// reversiGameMessage appends the game ID to an event message, so the admin
// can find the game the event is about.
func reversiGameMessage(msg, gameID string) string {
	return fmt.Sprintf("%s (対局 %s)", msg, gameID)
}

// pauseReversi waits on a game that has not started and whose settings the
// bot cannot play, without readiness.
//
// 招待された側は本体の REST で対局を取り消せない (cancel-match は自分が送った
// 招待だけ)。招待した人が盤に誤って触れただけのこともあるので、記録は閉じずに
// 待ち、受けられる設定に戻れば準備し直す。待つのは準備の期限まで。
func (b *bot) pauseReversi(ctx context.Context, as plugin.Caller, accountID string, g reversiGameView, why error) error {
	res, err := b.db().ExecContext(ctx, `
UPDATE reversi_games SET paused = true WHERE game_id = $1 AND status = 'waiting' AND NOT paused`, g.ID)
	if err != nil {
		return fmt.Errorf("pause reversi game: %w", err)
	}
	// 同時に届いた通知のうち、印を立てた方だけが記録を残す。updated_at は
	// 変えない。設定を切り替え続けられても、掃除 (pruneReversi) で閉じられるように。
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		b.logEvent(ctx, "info", eventReversi, reversiGameMessage("受けられない設定に変わったので、リバーシの準備をせずに待ちます。受けられる設定に戻れば準備します: "+why.Error(), g.ID), g.User1ID, "")
	}
	if !g.ready(accountID) {
		return nil
	}
	return setReady(ctx, as, g.ID, false)
}

// resumeReversi clears the mark of pauseReversi, so the next change to
// settings the bot cannot play is recorded again.
func resumeReversi(ctx context.Context, db *sql.DB, gameID string) error {
	if _, err := db.ExecContext(ctx, `UPDATE reversi_games SET paused = false WHERE game_id = $1 AND status = 'waiting' AND paused`, gameID); err != nil {
		return fmt.Errorf("resume reversi game: %w", err)
	}
	return nil
}

// settleClosed ends on the host a game whose record is closed.
//
// 記録は、掃除 (pruneReversi) や、準備と設定の変更の競合で、本体の対局より先に
// 閉じることがある。準備したまま放っておくと、相手が準備した時点で始まり、
// bot は打たずに時間切れで負ける。
func (b *bot) settleClosed(ctx context.Context, as plugin.Caller, accountID string, g reversiGameView) error {
	switch {
	case g.IsEnded:
		return nil
	case !g.IsStarted:
		if !g.ready(accountID) {
			return nil
		}
		return setReady(ctx, as, g.ID, false)
	default:
		return b.surrender(ctx, as, g, "やめたリバーシの対局が始まっていたので、投了しました")
	}
}

// advanceReversi does what g needs now from the bot.
func (b *bot) advanceReversi(ctx context.Context, as plugin.Caller, accountID string, rec reversiRecord, g reversiGameView) error {
	if g.IsEnded {
		return b.finishReversi(ctx, accountID, g)
	}
	playable := g.playable()
	if !g.IsStarted {
		if playable != nil {
			return b.pauseReversi(ctx, as, accountID, g, playable)
		}
		if err := resumeReversi(ctx, b.db(), g.ID); err != nil {
			return err
		}
		if g.ready(accountID) {
			return nil
		}
		return setReady(ctx, as, g.ID, true)
	}
	if playable != nil {
		// 準備と設定の変更が交差して、対応していない設定で始まった。投了する。
		return b.surrender(ctx, as, g, "対応していない設定で始まったので、リバーシの対局を投了しました: "+playable.Error())
	}
	color, err := g.color(accountID)
	if err != nil {
		return plugin.NoRetry(err)
	}
	game, ply, err := g.replay()
	if err != nil {
		b.logEvent(ctx, "error", eventReversi, reversiGameMessage("リバーシの手順を再現できません: "+err.Error(), g.ID), g.User1ID, "")
		return plugin.NoRetry(err)
	}
	if rec.Status == reversiWaiting {
		if _, err := b.db().ExecContext(ctx, `
UPDATE reversi_games SET status = 'playing', color = $2, started_at = $3, updated_at = $3 WHERE game_id = $1 AND status = 'waiting'`,
			g.ID, color.String(), now()); err != nil {
			return fmt.Errorf("start reversi game: %w", err)
		}
	}
	if game.Over {
		// 終局の通知 (ended) で記録を閉じる。
		return nil
	}
	job := reversiMoveJob{AccountID: accountID, GameID: g.ID, Ply: ply}
	if game.Turn == color {
		return b.enqueueReversi(ctx, jobReversiMove, job, plugin.WithDedup(reversiMoveDedup))
	}
	delay := max(g.turnDeadline().Sub(now()), 0) + reversiTimeoutGrace
	return b.enqueueReversi(ctx, jobReversiTimeout, job, plugin.WithDelay(delay), plugin.WithDedup(delay+reversiMoveDedup))
}

// finishReversi closes the record of an ended game with its result.
func (b *bot) finishReversi(ctx context.Context, accountID string, g reversiGameView) error {
	result := "draw"
	switch {
	case g.WinnerID == nil:
	case *g.WinnerID == accountID:
		result = "win"
	default:
		result = "lose"
	}
	var color sql.NullString
	var mine, theirs sql.NullInt64
	if c, err := g.color(accountID); err == nil {
		color = sql.NullString{String: c.String(), Valid: true}
		// 対応していない盤の対局は、標準の盤として数えても意味が無い。
		if game, _, err := g.replay(); err == nil && g.playable() == nil {
			mine = sql.NullInt64{Int64: int64(game.Board.Count(c)), Valid: true}
			theirs = sql.NullInt64{Int64: int64(game.Board.Count(c.Opponent())), Valid: true}
		}
	}
	t := now()
	_, err := b.db().ExecContext(ctx, `
UPDATE reversi_games SET status = 'ended', result = $2, bot_stones = $3, opponent_stones = $4,
	ended_at = $5, updated_at = $5, started_at = coalesce(started_at, $5), color = coalesce(color, $6)
WHERE game_id = $1 AND status IN ('waiting', 'playing')`, g.ID, result, mine, theirs, t, color)
	if err != nil {
		return fmt.Errorf("finish reversi game: %w", err)
	}
	return nil
}

// closeReversi closes an open record without a result. closed is false when
// the record was not open.
func (b *bot) closeReversi(ctx context.Context, gameID, status string) (closed bool, err error) {
	t := now()
	res, err := b.db().ExecContext(ctx, `
UPDATE reversi_games SET status = $2, ended_at = $3, updated_at = $3
WHERE game_id = $1 AND status IN ('waiting', 'playing')`, gameID, status, t)
	if err != nil {
		return false, fmt.Errorf("close reversi game: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("close reversi game: %w", err)
	}
	return n > 0, nil
}

// reversiJobGame reads the game for a job. ok is false when there is nothing
// to do (記録が閉じた、対局が消えた、始まっていない、終わった、手数がずれた)。
func (b *bot) reversiJobGame(ctx context.Context, p reversiMoveJob) (rec reversiRecord, g reversiGameView, game reversi.Game, color reversi.Color, ok bool, err error) {
	rec, found, err := loadReversiRecord(ctx, b.db(), p.GameID)
	if err != nil || !found || rec.Status != reversiPlaying {
		return rec, g, game, color, false, err
	}
	g, err = b.showGame(ctx, b.pctx.API().AsUser(p.AccountID), p.GameID)
	if err != nil {
		if isClientError(err) {
			_, err = b.closeReversi(ctx, p.GameID, reversiCanceled)
			return rec, g, game, color, false, err
		}
		return rec, g, game, color, false, fmt.Errorf("reversi/show-game: %w", err)
	}
	if g.IsEnded {
		// 終局の通知が失われても、記録を結果つきで閉じる。
		return rec, g, game, color, false, b.finishReversi(ctx, p.AccountID, g)
	}
	if !g.IsStarted {
		return rec, g, game, color, false, nil
	}
	if color, err = g.color(p.AccountID); err != nil {
		return rec, g, game, color, false, plugin.NoRetry(err)
	}
	game, ply, err := g.replay()
	if err != nil {
		return rec, g, game, color, false, plugin.NoRetry(err)
	}
	if ply != p.Ply || game.Over {
		return rec, g, game, color, false, nil
	}
	return rec, g, game, color, true, nil
}

// reversiMove searches and puts a stone.
func (b *bot) reversiMove(ctx context.Context, p reversiMoveJob) error {
	rec, g, game, color, ok, err := b.reversiJobGame(ctx, p)
	if err != nil || !ok || game.Turn != color {
		return err
	}
	s, err := loadSettings(ctx, b.db())
	if err != nil {
		return err
	}
	// 残りの持ち時間の 1/3 までにする。キューで待たされた分は、手番の始まりから
	// 数えるので引かれる。残りは本体への往復の分。
	think := min(time.Duration(s.Reversi.MaxThinkMs)*time.Millisecond, g.turnDeadline().Sub(now())/3)
	think = max(think, reversiMinThink)
	mv, err := reversi.Choose(ctx, game, reversi.Options{Level: rec.Level, MaxTime: think})
	if err != nil {
		return plugin.NoRetry(err)
	}
	if _, err := b.pctx.API().AsUser(p.AccountID).Call(ctx, "reversi/put-stone", map[string]any{"gameId": p.GameID, "pos": mv.Pos}); err != nil {
		if isClientError(err) {
			// 盤面が進んでいた (NOT_YOUR_TURN / INVALID_MOVE)、終わったなど。
			// 次の通知で読み直す。
			return nil
		}
		return fmt.Errorf("reversi/put-stone: %w", err)
	}
	_, err = b.db().ExecContext(ctx, `UPDATE reversi_games SET updated_at = $2 WHERE game_id = $1`, p.GameID, now())
	return err
}

// reversiTimeout claims that the opponent ran out of time.
func (b *bot) reversiTimeout(ctx context.Context, p reversiMoveJob) error {
	_, _, game, color, ok, err := b.reversiJobGame(ctx, p)
	if err != nil || !ok || game.Turn == color {
		return err
	}
	as := b.pctx.API().AsUser(p.AccountID)
	if _, err := as.Call(ctx, "reversi/claim-time-is-up", map[string]any{"gameId": p.GameID}); err != nil {
		if isClientError(err) {
			return nil
		}
		return fmt.Errorf("reversi/claim-time-is-up: %w", err)
	}
	// 時間が残っていれば本体は何もしない (時計のずれで早すぎた)。少し待って
	// もう一度だけ確かめる。終わっていれば、終局の通知で記録が閉じる。
	if p.Attempt+1 >= reversiTimeoutAttempts {
		return nil
	}
	if _, _, _, _, still, err := b.reversiJobGame(ctx, p); err != nil || !still {
		return err
	}
	next := p
	next.Attempt++
	return b.enqueueReversi(ctx, jobReversiTimeout, next, plugin.WithDelay(reversiTimeoutRetry))
}

// reversiReadyTimeout gives up a game that did not start in time.
func (b *bot) reversiReadyTimeout(ctx context.Context, p reversiReadyJob) error {
	// 取りやめた記録 (canceled) も読む。前の試行が記録を閉じた後、準備を外すのに
	// 失敗していたら、再試行で外す。
	rec, found, err := loadReversiRecord(ctx, b.db(), p.GameID)
	if err != nil || !found || (rec.Status != reversiWaiting && rec.Status != reversiCanceled) {
		return err
	}
	as := b.pctx.API().AsUser(p.AccountID)
	g, err := b.showGame(ctx, as, p.GameID)
	if err != nil {
		if isClientError(err) {
			_, err := b.closeReversi(ctx, p.GameID, reversiCanceled)
			return err
		}
		return fmt.Errorf("reversi/show-game: %w", err)
	}
	if rec.Status == reversiWaiting {
		if g.IsStarted || g.IsEnded {
			return b.advanceReversi(ctx, as, p.AccountID, rec, g)
		}
		// 先に記録を閉じる。準備を外した後で閉じると、その間に届いた通知が準備し直す。
		closed, err := b.closeReversi(ctx, p.GameID, reversiCanceled)
		if err != nil {
			return err
		}
		if closed {
			why := "相手が準備しないまま時間が過ぎたので、リバーシの対局をやめました"
			// 印ではなく今の設定で決める。印は、通知の競合で立ったまま残ることがある。
			if g.playable() != nil {
				why = "受けられない設定のまま準備の期限が過ぎたので、リバーシの対局をやめました"
			}
			b.logEvent(ctx, "info", eventReversi, reversiGameMessage(why, g.ID), g.User1ID, "")
		}
	}
	return b.settleClosed(ctx, as, p.AccountID, g)
}

// pruneReversi closes the records of games that did not change for a long
// time, and drops old closed records.
func pruneReversi(ctx context.Context, db *sql.DB) error {
	t := now()
	if _, err := db.ExecContext(ctx, `
UPDATE reversi_games SET status = 'canceled', ended_at = $1, updated_at = $1
WHERE status IN ('waiting', 'playing') AND updated_at < $2`, t, t.Add(-reversiStale)); err != nil {
		return fmt.Errorf("prune reversi games: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
DELETE FROM reversi_games WHERE status IN ('ended', 'canceled') AND updated_at < $1`, t.AddDate(0, 0, -400)); err != nil {
		return fmt.Errorf("prune reversi games: %w", err)
	}
	return nil
}

// reversiGameRow is one game shown on the admin page.
type reversiGameRow struct {
	GameID         string     `json:"gameId"`
	OpponentID     string     `json:"opponentId"`
	OpponentAcct   string     `json:"opponentAcct"`
	Level          int        `json:"level"`
	Color          *string    `json:"color"`
	Status         string     `json:"status"`
	Result         *string    `json:"result"`
	BotStones      *int       `json:"botStones"`
	OpponentStones *int       `json:"opponentStones"`
	CreatedAt      time.Time  `json:"createdAt"`
	EndedAt        *time.Time `json:"endedAt"`
}

// reversiRecordSummary counts the results of ended games.
type reversiRecordSummary struct {
	Wins   int `json:"wins"`
	Losses int `json:"losses"`
	Draws  int `json:"draws"`
}

func listReversiGames(ctx context.Context, db *sql.DB, limit int) ([]reversiGameRow, reversiRecordSummary, error) {
	var sum reversiRecordSummary
	if err := db.QueryRowContext(ctx, `
SELECT count(*) FILTER (WHERE result = 'win'), count(*) FILTER (WHERE result = 'lose'), count(*) FILTER (WHERE result = 'draw')
FROM reversi_games WHERE status = 'ended'`).Scan(&sum.Wins, &sum.Losses, &sum.Draws); err != nil {
		return nil, sum, fmt.Errorf("reversi summary: %w", err)
	}
	// 進行中の対局を先に、それぞれ新しい順に出す。
	rows, err := db.QueryContext(ctx, `
SELECT game_id, opponent_id, opponent_acct, level, color, status, result, bot_stones, opponent_stones, created_at, ended_at
FROM reversi_games
ORDER BY (status IN ('waiting', 'playing')) DESC, created_at DESC, game_id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, sum, fmt.Errorf("reversi games: %w", err)
	}
	defer rows.Close() //nolint:errcheck // 読み捨て
	out := []reversiGameRow{}
	for rows.Next() {
		var r reversiGameRow
		if err := rows.Scan(&r.GameID, &r.OpponentID, &r.OpponentAcct, &r.Level, &r.Color, &r.Status, &r.Result,
			&r.BotStones, &r.OpponentStones, &r.CreatedAt, &r.EndedAt); err != nil {
			return nil, sum, fmt.Errorf("reversi games scan: %w", err)
		}
		out = append(out, r)
	}
	return out, sum, rows.Err()
}
