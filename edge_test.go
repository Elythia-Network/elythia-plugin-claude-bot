package claudebot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

/*
 * 失敗の経路。本体の API や storage が落ちたときに、黙って成功扱いに
 * しないこと (再試行に任せる) と、4xx を再試行させないことを確かめる。
 */

var errBoom = errors.New("boom")

func TestReply_HostFailuresAreRetried(t *testing.T) {
	for _, endpoint := range []string{"notes/show", "notes/conversation", "users/relation", "notes/create"} {
		t.Run(endpoint, func(t *testing.T) {
			e := newEnv(t)
			e.settings(func(s *Settings) { s.Reply.Audience = "followers" })
			n := note("n1", "alice", "alice", "", "hi", "public")
			n["replyId"] = "p"
			e.api.notes["n1"] = n
			e.api.followers["alice"] = true
			e.api.fail[endpoint] = &plugin.APIError{Endpoint: endpoint, Status: 500}
			e.claude.push(message("やあ", "end_turn", 1, 1))
			err := e.mention("x", "n1")
			require.Error(t, err)
			assert.False(t, errors.Is(err, plugin.ErrNoRetry))
			// 失敗にした通知は、次の配達で取り直せる。
			var status string
			require.NoError(t, e.db.QueryRow(`SELECT status FROM handled_notifications WHERE id = 'x'`).Scan(&status))
			assert.Equal(t, "failed", status)
		})
	}
}

func TestReply_HostClientErrorsAreNotRetried(t *testing.T) {
	for _, endpoint := range []string{"notes/conversation", "users/relation"} {
		t.Run(endpoint, func(t *testing.T) {
			e := newEnv(t)
			e.settings(func(s *Settings) { s.Reply.Audience = "followers" })
			n := note("n1", "alice", "alice", "", "hi", "public")
			n["replyId"] = "p"
			e.api.notes["n1"] = n
			e.api.followers["alice"] = true
			e.api.fail[endpoint] = &plugin.APIError{Endpoint: endpoint, Status: 400}
			e.claude.push(message("やあ", "end_turn", 1, 1))
			require.NoError(t, e.mention("x", "n1"))
		})
	}
}

func TestReply_BrokenResponseIsNotRetried(t *testing.T) {
	e := newEnv(t)
	e.api.raw["notes/show"] = `[1,2,3]`
	err := e.mention("x", "n1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, plugin.ErrNoRetry), "応答の形が違うのは繰り返しても直らない")
}

func TestReply_EmojiLookupFailureFallsBack(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Reply.Mode = "reaction" })
	e.api.notes["n1"] = note("n1", "alice", "alice", "", ":blobcat:", "public")
	e.api.fail["emoji"] = errBoom
	require.NoError(t, e.mention("x", "n1"))
	assert.Equal(t, "👍", e.api.callsTo("notes/reactions/create")[0].Params["reaction"])
}

func TestReply_IgnoresMalformedNotification(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.h.Notify(plugin.Notification{ID: "x", Type: plugin.NotificationMention, AccountID: e.botID}))
	assert.Empty(t, e.api.calls)
}

func TestStorageFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("notification", func(t *testing.T) {
		e := newEnv(t)
		e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
		_, err := e.db.Exec(`DROP TABLE handled_notifications`)
		require.NoError(t, err)
		require.Error(t, e.mention("x", "n1"))
	})
	t.Run("settings", func(t *testing.T) {
		e := newEnv(t)
		e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
		_, err := e.db.Exec(`DROP TABLE settings`)
		require.NoError(t, err)
		require.Error(t, e.mention("x", "n1"))
		require.Error(t, e.h.Jobs(Plugin).Run(t, jobTick, ""))
		_, err = e.h.Routes(Plugin).Call(t, "POST /admin/state", plugintest.Request{Administrator: true})
		require.Error(t, err)
		require.Error(t, saveSettings(ctx, e.db, defaultSettings()))
	})
	t.Run("broken settings json", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.db.Exec(`INSERT INTO settings (id, data) VALUES (1, '"not an object"')`)
		require.NoError(t, err)
		s, err := loadSettings(ctx, e.db)
		require.Error(t, err)
		assert.Equal(t, defaultSettings().Model, s.Model, "読めなければ既定値を返す")
	})
	t.Run("usage and events", func(t *testing.T) {
		e := newEnv(t)
		e.settings(func(s *Settings) { s.Scheduled.Enabled = true; s.Scheduled.Times = []string{"21:00"} })
		e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
		_, err := e.db.Exec(`DROP TABLE usage_log`)
		require.NoError(t, err)
		require.Error(t, e.mention("x", "n1"), "上限を確かめられなければ呼ばない")
		assert.Empty(t, e.claude.calls())
		require.Error(t, e.h.Jobs(Plugin).Run(t, jobTick, ""))
		_, err = summarize(ctx, e.db, defaultSettings())
		require.Error(t, err)
		_, err = e.db.Exec(`DROP TABLE events`)
		require.NoError(t, err)
		_, err = recentEvents(ctx, e.db, 1)
		require.Error(t, err)
		// 記録に失敗しても処理は止めない。
		newBot(e.h.Context()).logEvent(ctx, "info", "x", "y", "", "")
	})
	t.Run("limits", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.db.Exec(`DROP TABLE usage_log`)
		require.NoError(t, err)
		for _, s := range []func(*Settings){
			func(s *Settings) { s.Limits.PerUserPerHour = 0; s.Limits.PerHostPerDay = 1 },
			func(s *Settings) { s.Limits.PerUserPerHour = 0; s.Limits.GlobalPerDay = 0; s.Limits.MonthlyTokens = 1 },
			func(s *Settings) {
				*s = Settings{Budget: BudgetSettings{MonthlyUSD: 1, StopWhenExhausted: true}, Timezone: "UTC"}
			},
		} {
			c := defaultSettings()
			c.Limits.GlobalPerDay = 0
			s(&c)
			_, err := checkLimits(ctx, e.db, c, "u", "h")
			assert.Error(t, err)
		}
	})
	t.Run("schedule slots and round trips", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.db.Exec(`DROP TABLE scheduled_runs`)
		require.NoError(t, err)
		s := defaultSettings()
		s.Scheduled.Times = []string{"21:00"}
		_, _, err = claimDueSlot(ctx, e.db, s, now())
		require.Error(t, err)
		_, err = e.db.Exec(`DROP TABLE bot_replies`)
		require.NoError(t, err)
		_, err = newBot(e.h.Context()).roundTrips(ctx, threadView{rootID: "r"}, e.botID)
		require.Error(t, err)
		require.Error(t, prune(ctx, e.db))
	})
}

func TestReply_DraftSaveFailureIsRetried(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(message("やあ", "end_turn", 1, 1))
	_, err := e.db.Exec(`ALTER TABLE handled_notifications ADD CONSTRAINT test_no_draft CHECK (reply_text IS NULL) NOT VALID`)
	require.NoError(t, err)
	require.Error(t, e.mention("x", "n1"))
	assert.Empty(t, e.api.callsTo("notes/create"))
}

func TestScheduled_PostFailureIsRecorded(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Scheduled.Enabled = true; s.Scheduled.Times = []string{"07:00"} })
	e.api.createFails = []int{500}
	e.claude.push(message("おはよう", "end_turn", 1, 1))
	setClock(t, time.Date(2026, 10, 9, 7, 0, 0, 0, jst))
	require.NoError(t, e.h.Jobs(Plugin).Run(t, jobTick, ""))
	assert.Len(t, e.events(eventPostError), 1)
}

func TestScheduled_ListAccountsFailure(t *testing.T) {
	db := testDB(t)
	h := plugintest.New(t).WithName(Name).WithDB(db).WithAPI(newFakeAPI()).WithAccounts(brokenAccounts{})
	js := h.Jobs(Plugin)
	require.NoError(t, saveSettings(context.Background(), db, func() Settings {
		s := defaultSettings()
		s.Scheduled.Enabled = true
		s.Scheduled.Times = []string{"07:00"}
		return s
	}()))
	require.Error(t, js.Run(t, jobTick, ""))
	_, err := h.Routes(Plugin).Call(t, "POST /admin/state", plugintest.Request{Administrator: true})
	require.Error(t, err)
}

// brokenAccounts fails every call. UpdateProfile returns an APIError so
// the admin page can show i/update's reason.
type brokenAccounts struct{ withAccount bool }

func (b brokenAccounts) Create(context.Context, string) (plugin.Account, error) {
	return plugin.Account{}, plugin.ErrUsernameUnavailable
}

func (b brokenAccounts) List(context.Context) ([]plugin.Account, error) {
	if b.withAccount {
		return []plugin.Account{{ID: "bot", Username: "bot"}}, nil
	}
	return nil, errBoom
}

func (b brokenAccounts) UpdateProfile(context.Context, string, plugin.ProfileUpdate) error {
	return &plugin.APIError{Endpoint: "i/update", Status: 400,
		Body: json.RawMessage(`{"error":{"code":"AVATAR_NOT_AN_IMAGE","message":"not an image"}}`)}
}

func (b brokenAccounts) Delete(context.Context, string) error { return errBoom }

func TestRoutes_ErrorPaths(t *testing.T) {
	admin := func(body string) plugintest.Request {
		return plugintest.Request{Administrator: true, Body: body}
	}
	t.Run("without account", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, e.h.Context().Accounts().Delete(t.Context(), e.botID))
		r := e.h.Routes(Plugin)
		for _, path := range []string{"/admin/account/update", "/admin/account/delete"} {
			_, err := r.Call(t, "POST "+path, admin(`{}`))
			assert.Equal(t, http.StatusBadRequest, statusOf(t, err), path)
		}
		_, err := r.Call(t, "POST /admin/account/create", admin(`{"username":"claudebot"}`))
		assert.Equal(t, http.StatusBadRequest, statusOf(t, err), "削除した名前は使えない")
	})
	t.Run("bad bodies", func(t *testing.T) {
		e := newEnv(t)
		r := e.h.Routes(Plugin)
		for _, path := range []string{"/admin/settings", "/admin/account/create", "/admin/account/update", "/admin/account/delete"} {
			_, err := r.Call(t, "POST "+path, admin(`{`))
			assert.Equal(t, http.StatusBadRequest, statusOf(t, err), path)
		}
		long := strings.Repeat("あ", 51)
		_, err := r.Call(t, "POST /admin/account/update", admin(asJSON(t, map[string]any{"name": long})))
		assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
		_, err = r.Call(t, "POST /admin/account/update", admin(asJSON(t, map[string]any{"description": strings.Repeat("あ", 1501)})))
		assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
		big := base64.StdEncoding.EncodeToString(make([]byte, maxAvatarBytes+1))
		_, err = r.Call(t, "POST /admin/account/update", admin(asJSON(t, map[string]any{"avatar": map[string]any{"data": big}})))
		assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
		s := defaultSettings()
		s.Reply.SystemPrompt = strings.Repeat("あ", maxPromptChars+1)
		_, err = r.Call(t, "POST /admin/settings", admin(asJSON(t, map[string]any{"settings": s})))
		assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
	})
	t.Run("broken accounts", func(t *testing.T) {
		db := testDB(t)
		h := plugintest.New(t).WithName(Name).WithDB(db).WithAPI(newFakeAPI()).WithAccounts(brokenAccounts{withAccount: true})
		r := h.Routes(Plugin)
		_, err := r.Call(t, "POST /admin/account/update", admin(`{"name":"x"}`))
		assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
		assert.Contains(t, err.Error(), "AVATAR_NOT_AN_IMAGE", "i/update の理由を見せる")
		_, err = r.Call(t, "POST /admin/account/delete", admin(`{"username":"bot"}`))
		require.ErrorIs(t, err, errBoom)
	})
	t.Run("create when list fails", func(t *testing.T) {
		db := testDB(t)
		h := plugintest.New(t).WithName(Name).WithDB(db).WithAPI(newFakeAPI()).WithAccounts(brokenAccounts{})
		_, err := h.Routes(Plugin).Call(t, "POST /admin/account/create", admin(`{"username":"x"}`))
		require.ErrorIs(t, err, errBoom)
	})
	t.Run("profile unreadable", func(t *testing.T) {
		e := newEnv(t)
		e.api.fail["i"] = errBoom
		res, err := e.h.Routes(Plugin).Call(t, "POST /admin/state", admin(`{}`))
		require.NoError(t, err, "プロフィールが読めなくても画面は出す")
		assert.Nil(t, res.(*stateResponse).Account.Name)
	})
}

func TestProfileError(t *testing.T) {
	assert.ErrorIs(t, profileError(errBoom), errBoom)
	assert.Contains(t, profileError(plugin.ErrAccountSuspended).Error(), "凍結")
	var apierr *plugin.APIError
	assert.ErrorAs(t, profileError(&plugin.APIError{Status: 500}), &apierr, "5xx は理由を見せずにそのまま返す")
}

func TestSmallHelpers(t *testing.T) {
	assert.Equal(t, "", noteView{}.text())
	assert.Equal(t, "", apiErrorCode(errBoom))
	assert.Equal(t, "boom", describeAPIError(errBoom))
	assert.Equal(t, time.UTC, Settings{Timezone: "Nowhere/Nope"}.location())

	// meta が読めなければ upstream の既定値を使う。
	e := newEnv(t)
	e.api.fail["meta"] = errBoom
	assert.Equal(t, defaultMaxNoteLength, newBot(e.h.Context()).maxNoteLength(t.Context()))
	noAPI := plugintest.New(t).WithName(Name)
	assert.Equal(t, defaultMaxNoteLength, newBot(noAPI.Context()).maxNoteLength(t.Context()))

	// API のエラーの本文は長ければ切る。
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(apiError(400, "invalid_request_error", strings.Repeat("x", 1000)))
	require.NoError(t, e.mention("x", "n1"))
	assert.Contains(t, e.events(eventAPIError)[0].Message, "…")
}
