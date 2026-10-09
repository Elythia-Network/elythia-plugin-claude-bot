package claudebot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/elythia-network/elythia/plugin"
)

const jobTick = "tick"

// catchUp is how far back a missed slot is still posted.
//
// 再起動やキューの遅れで tick が数分ずれても、その枠の投稿は落とさない。
// それより古い枠は、遅れて出すと時刻に合わない投稿になるので捨てる。
const catchUp = 5 * time.Minute

func jobs(pctx plugin.Context, j plugin.Jobs) error {
	b := newBot(pctx)
	j.Handle(jobTick, func(ctx context.Context, _ json.RawMessage) error {
		return b.tick(ctx)
	})
	// 予定は管理画面で変わるので、cron には固定の 1 分ごとの tick だけを
	// 登録し、投稿するかどうかは tick の中で設定を見て決める。予定ごとに
	// Schedule を呼ぶと、変えたときに起動し直すまで反映されない。
	j.Schedule("* * * * *", jobTick, nil)
	return nil
}

// tick runs every minute: posts a scheduled note if one is due, and prunes
// old bookkeeping rows.
func (b *bot) tick(ctx context.Context) error {
	db := b.db()
	if err := prune(ctx, db); err != nil {
		return err
	}
	s, err := loadSettings(ctx, db)
	if err != nil {
		return err
	}
	if !s.Scheduled.Enabled {
		return nil
	}
	acc, err := b.botAccount(ctx)
	if err != nil {
		return err
	}
	if acc == nil {
		return nil
	}
	slot, ok, err := claimDueSlot(ctx, db, s, now())
	if err != nil || !ok {
		return err
	}
	return b.postScheduled(ctx, s, acc.ID, slot)
}

// dueSlots returns the scheduled times in (t-catchUp, t].
func dueSlots(s ScheduledSettings, loc *time.Location, t time.Time) []time.Time {
	l := t.In(loc)
	var out []time.Time
	from := l.Add(-catchUp)
	add := func(c time.Time) {
		if !c.After(l) && c.After(from) {
			out = append(out, c)
		}
	}
	// 窓が日付をまたぐ (00:02 に 23:59 の枠を拾う) ので、前日の分も見る。
	for _, day := range []time.Time{
		time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc),
		time.Date(l.Year(), l.Month(), l.Day()-1, 0, 0, 0, 0, loc),
	} {
		for _, hm := range s.Times {
			h, m, ok := parseHM(hm)
			if !ok {
				continue
			}
			add(time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc))
		}
		if s.IntervalMinutes > 0 {
			for m := 0; m < 24*60; m += s.IntervalMinutes {
				add(time.Date(day.Year(), day.Month(), day.Day(), 0, m, 0, 0, loc))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

func parseHM(s string) (int, int, bool) {
	if !timePattern.MatchString(s) {
		return 0, 0, false
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h, m, true
}

// claimDueSlot claims every due slot and returns the latest newly claimed
// one.
//
// 投稿する前に枠を取る。取った後に落ちるとその枠は投稿されないが、
// 定時の投稿は 1 回抜けても困らず、二重に投稿する方が目立つため。
// 遅れて複数の枠が溜まっていても、投稿は最新の 1 つだけにする。
func claimDueSlot(ctx context.Context, db *sql.DB, s Settings, t time.Time) (time.Time, bool, error) {
	var latest time.Time
	found := false
	for _, slot := range dueSlots(s.Scheduled, s.location(), t) {
		var got time.Time
		err := db.QueryRowContext(ctx, `
INSERT INTO scheduled_runs (slot, claimed_at) VALUES ($1, $2)
ON CONFLICT (slot) DO NOTHING RETURNING slot`, slot, t).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return time.Time{}, false, fmt.Errorf("claim slot: %w", err)
		}
		latest, found = slot, true
	}
	return latest, found, nil
}

var weekdays = [...]string{"日", "月", "火", "水", "木", "金", "土"}

func scheduledPrompt(slot time.Time, tz string) string {
	return fmt.Sprintf("現在の日時は%d年%d月%d日(%s) %02d:%02d (%s) です。この時刻に合った投稿を1件書いてください。",
		slot.Year(), int(slot.Month()), slot.Day(), weekdays[slot.Weekday()], slot.Hour(), slot.Minute(), tz)
}

func (b *bot) postScheduled(ctx context.Context, s Settings, accountID string, slot time.Time) error {
	db := b.db()
	reason, err := checkLimits(ctx, db, s, "", "")
	if err != nil {
		return err
	}
	if reason != "" {
		b.logEvent(ctx, "warn", eventSilenced, "費用の上限のため、定時の投稿をしませんでした: "+reason, "", "")
		return nil
	}
	text, err := b.generate(ctx, s, generation{
		Kind:      "scheduled",
		System:    s.Scheduled.SystemPrompt,
		Prompt:    scheduledPrompt(slot.In(s.location()), s.Timezone),
		MaxChars:  s.Scheduled.MaxChars,
		MaxTokens: s.Scheduled.MaxTokens,
		PostLimit: b.maxNoteLength(ctx),
	})
	// 一時的なエラーでも、定時の投稿はやり直さない (枠は取ってあり、遅れて
	// 出すと時刻に合わない)。記録は generate が残している。
	if errors.Is(err, errSilent) || errors.Is(err, errTransient) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = b.pctx.API().AsUser(accountID).Call(ctx, "notes/create", map[string]any{
		"text":       text,
		"visibility": s.Scheduled.Visibility,
	})
	if err != nil {
		// 枠は取ってあるので、ジョブを失敗させても再試行はされない (cron は
		// 再試行しない)。記録だけ残す。
		b.logEvent(ctx, "error", eventPostError, "定時の投稿に失敗しました: "+err.Error(), "", "")
	}
	return nil
}
