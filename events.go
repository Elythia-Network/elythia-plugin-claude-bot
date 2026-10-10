package claudebot

import (
	"context"
	"database/sql"
	"fmt"
	"time"
	"unicode/utf8"
)

// Event kinds shown on the admin page.
const (
	eventAPIError   = "api_error"
	eventSilenced   = "silenced"
	eventRefused    = "refused"
	eventSkipped    = "skipped"
	eventPostError  = "post_error"
	eventConfig     = "config"
	eventReactError = "reaction_error"
	eventImage      = "image"
)

// event is one line of the admin page's log.
type event struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
	UserID  string    `json:"userId,omitempty"`
	NoteID  string    `json:"noteId,omitempty"`
}

const maxEventMessage = 2000

// logEvent records an event for the admin page and the server log.
//
// 記録に失敗しても処理は止めない (slog には残る)。返事の流れの途中で
// 記録の失敗をエラーとして返すと、通知が再試行されて二重に動く。
func (b *bot) logEvent(ctx context.Context, level, kind, msg, userID, noteID string) {
	if utf8.RuneCountInString(msg) > maxEventMessage {
		msg = string([]rune(msg)[:maxEventMessage]) + "…"
	}
	attrs := []any{"kind", kind, "message", msg, "userId", userID, "noteId", noteID}
	switch level {
	case "error":
		b.log.Error("claude-bot", attrs...)
	case "warn":
		b.log.Warn("claude-bot", attrs...)
	default:
		b.log.Info("claude-bot", attrs...)
	}
	_, err := b.db().ExecContext(ctx, `
INSERT INTO events (at, level, kind, message, user_id, note_id) VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''))`,
		now(), level, kind, msg, userID, noteID)
	if err != nil {
		b.log.Error("claude-bot: 記録を保存できません", "err", err)
	}
}

func recentEvents(ctx context.Context, db *sql.DB, limit int) ([]event, error) {
	rows, err := db.QueryContext(ctx, `
SELECT at, level, kind, message, COALESCE(user_id, ''), COALESCE(note_id, '')
FROM events ORDER BY at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	defer rows.Close() //nolint:errcheck // 読み捨て
	out := []event{}
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.At, &e.Level, &e.Kind, &e.Message, &e.UserID, &e.NoteID); err != nil {
			return nil, fmt.Errorf("events scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// prune drops old bookkeeping rows.
//
// 使った量は予算の計算に今月分しか使わないが、画面で前の月と比べられる
// よう 400 日残す。
func prune(ctx context.Context, db *sql.DB) error {
	t := now()
	for _, q := range []struct {
		sql    string
		before time.Time
	}{
		{`DELETE FROM events WHERE at < $1`, t.AddDate(0, 0, -90)},
		{`DELETE FROM handled_notifications WHERE claimed_at < $1`, t.AddDate(0, 0, -30)},
		{`DELETE FROM scheduled_runs WHERE slot < $1`, t.AddDate(0, 0, -30)},
		{`DELETE FROM usage_log WHERE at < $1`, t.AddDate(0, 0, -400)},
		{`DELETE FROM bot_replies WHERE at < $1`, t.AddDate(0, 0, -90)},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.before); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	return nil
}
