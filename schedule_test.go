package claudebot

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var jst = func() *time.Location {
	l, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		panic(err)
	}
	return l
}()

func TestJobs_RegistersMinuteTick(t *testing.T) {
	e := newEnv(t)
	js := e.h.Jobs(Plugin)
	require.Len(t, js.Schedules, 1)
	assert.Equal(t, "* * * * *", js.Schedules[0].Cron)
	assert.Equal(t, jobTick, js.Schedules[0].Name)
}

func TestScheduled_PostsAtConfiguredTimeWithVisibility(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) {
		s.Scheduled.Enabled = true
		s.Scheduled.Times = []string{"07:00", "19:30"}
		s.Scheduled.Visibility = "followers"
		s.Scheduled.MaxChars = 80
	})
	js := e.h.Jobs(Plugin)
	tick := func(at time.Time) {
		t.Helper()
		setClock(t, at)
		require.NoError(t, js.Run(t, jobTick, ""))
	}

	tick(time.Date(2026, 10, 9, 6, 59, 30, 0, jst))
	assert.Empty(t, e.claude.calls(), "まだ時刻ではない")

	e.claude.push(message("おはようございます", "end_turn", 10, 10))
	tick(time.Date(2026, 10, 9, 7, 0, 10, 0, jst))
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1)
	assert.Equal(t, e.botID, creates[0].As)
	assert.Equal(t, "おはようございます", creates[0].Params["text"])
	assert.Equal(t, "followers", creates[0].Params["visibility"])
	_, isReply := creates[0].Params["replyId"]
	assert.False(t, isReply)
	calls := e.claude.calls()
	require.Len(t, calls, 1)
	assert.True(t, strings.HasSuffix(systemOf(calls[0]), lengthInstruction(80)))
	assert.True(t, strings.HasPrefix(systemOf(calls[0]), defaultSettings().Scheduled.SystemPrompt))
	assert.Contains(t, promptOf(calls[0]), "2026年10月9日(金) 07:00")

	// 同じ枠ではもう投稿しない (次の tick や二重の起動)。Claude も呼ばない。
	e.claude.push(message("二度目", "end_turn", 1, 1), message("三度目", "end_turn", 1, 1))
	tick(time.Date(2026, 10, 9, 7, 1, 0, 0, jst))
	tick(time.Date(2026, 10, 9, 7, 0, 50, 0, jst))
	assert.Len(t, e.claude.calls(), 1)
	assert.Len(t, e.api.callsTo("notes/create"), 1)
	e.claude.replies = nil

	// 遅れた tick でも 5 分以内なら出す。
	e.claude.push(message("こんばんは", "end_turn", 10, 10))
	tick(time.Date(2026, 10, 9, 19, 34, 0, 0, jst))
	assert.Len(t, e.api.callsTo("notes/create"), 2)
}

func TestScheduled_StaleSlotIsDropped(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) {
		s.Scheduled.Enabled = true
		s.Scheduled.Times = []string{"07:00"}
	})
	js := e.h.Jobs(Plugin)
	setClock(t, time.Date(2026, 10, 9, 7, 5, 0, 0, jst))
	require.NoError(t, js.Run(t, jobTick, ""))
	assert.Empty(t, e.claude.calls(), "5 分を過ぎた枠は出さない")
}

func TestScheduled_Interval(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) {
		s.Scheduled.Enabled = true
		s.Scheduled.IntervalMinutes = 90
	})
	js := e.h.Jobs(Plugin)
	for _, c := range []struct {
		at   time.Time
		want int
	}{
		{time.Date(2026, 10, 9, 1, 29, 0, 0, jst), 0},
		{time.Date(2026, 10, 9, 1, 30, 0, 0, jst), 1},
		{time.Date(2026, 10, 9, 2, 0, 0, 0, jst), 1},
		{time.Date(2026, 10, 9, 3, 0, 0, 0, jst), 2},
	} {
		e.claude.push(message("定時", "end_turn", 1, 1))
		setClock(t, c.at)
		require.NoError(t, js.Run(t, jobTick, ""))
		assert.Len(t, e.api.callsTo("notes/create"), c.want, c.at)
	}
}

func TestScheduled_DisabledOrNoAccount(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Scheduled.Times = []string{"07:00"} })
	js := e.h.Jobs(Plugin)
	setClock(t, time.Date(2026, 10, 9, 7, 0, 0, 0, jst))
	require.NoError(t, js.Run(t, jobTick, ""))
	assert.Empty(t, e.claude.calls(), "無効なら出さない")

	require.NoError(t, e.h.Context().Accounts().Delete(t.Context(), e.botID))
	e.settings(func(s *Settings) { s.Scheduled.Enabled = true; s.Scheduled.Times = []string{"07:00"} })
	require.NoError(t, js.Run(t, jobTick, ""))
	assert.Empty(t, e.claude.calls(), "アカウントが無ければ出さない")
}

func TestScheduled_SilentOnAPIErrorAndLimits(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) {
		s.Scheduled.Enabled = true
		s.Scheduled.Times = []string{"07:00", "08:00"}
		s.Limits.GlobalPerDay = 1
	})
	js := e.h.Jobs(Plugin)
	e.claude.push(apiError(529, "overloaded_error", "Overloaded"))
	setClock(t, time.Date(2026, 10, 9, 7, 0, 0, 0, jst))
	require.NoError(t, js.Run(t, jobTick, ""))
	assert.Empty(t, e.api.callsTo("notes/create"))
	assert.Len(t, e.events(eventAPIError), 1)

	e.addUsage("", now(), "claude-opus-5-5", tokens{Input: 1})
	setClock(t, time.Date(2026, 10, 9, 8, 0, 0, 0, jst))
	require.NoError(t, js.Run(t, jobTick, ""))
	assert.Len(t, e.claude.calls(), 1, "1 日の上限に達したら定時の投稿もしない")
}

func TestDueSlots_AcrossMidnight(t *testing.T) {
	s := ScheduledSettings{Times: []string{"23:58", "00:01", "bad"}}
	got := dueSlots(s, jst, time.Date(2026, 10, 10, 0, 2, 0, 0, jst))
	assert.Equal(t, []time.Time{
		time.Date(2026, 10, 9, 23, 58, 0, 0, jst),
		time.Date(2026, 10, 10, 0, 1, 0, 0, jst),
	}, got)
}

func TestScheduled_LatestOfManyDueSlots(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) {
		s.Scheduled.Enabled = true
		s.Scheduled.Times = []string{"07:00", "07:02"}
	})
	js := e.h.Jobs(Plugin)
	e.claude.push(message("x", "end_turn", 1, 1), message("y", "end_turn", 1, 1))
	setClock(t, time.Date(2026, 10, 9, 7, 3, 0, 0, jst))
	require.NoError(t, js.Run(t, jobTick, ""))
	require.Len(t, e.claude.calls(), 1, "溜まった枠は最新の 1 つだけ出す")
	assert.Contains(t, promptOf(e.claude.calls()[0]), "07:02")
	setClock(t, time.Date(2026, 10, 9, 7, 4, 0, 0, jst))
	require.NoError(t, js.Run(t, jobTick, ""))
	assert.Len(t, e.claude.calls(), 1)
}

func TestTick_Prunes(t *testing.T) {
	e := newEnv(t)
	_, err := e.db.Exec(`INSERT INTO events (at, level, kind, message) VALUES ($1, 'info', 'x', 'old'), ($2, 'info', 'x', 'new')`,
		now().AddDate(0, 0, -91), now())
	require.NoError(t, err)
	e.addUsage("a", now().AddDate(0, 0, -401), "m", tokens{Input: 1})
	e.addUsage("a", now().AddDate(0, 0, -399), "m", tokens{Input: 1})
	require.NoError(t, e.h.Jobs(Plugin).Run(t, jobTick, ""))
	assert.Len(t, e.events("x"), 1)
	assert.Equal(t, 1, e.usageRows())
}
