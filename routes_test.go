package claudebot

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefinition_Validates(t *testing.T) {
	require.NoError(t, Plugin.Validate())
	require.Len(t, Plugin.Secrets, 1)
	assert.Equal(t, "apiKey", Plugin.Secrets[0].Name)
	assert.NotNil(t, Plugin.Notifications)
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	se, _ := plugin.ExtractStatusError(err)
	if se == nil {
		t.Fatalf("StatusError ではありません: %v", err)
	}
	return se.Status
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestRoutes_Permissions(t *testing.T) {
	e := newEnv(t)
	r := e.h.Routes(Plugin)

	_, err := r.Call(t, "POST /admin/state", plugintest.Request{UserID: "u"})
	assert.Equal(t, http.StatusForbidden, statusOf(t, err), "一般の利用者は読めない")
	_, err = r.Call(t, "POST /admin/state", plugintest.Request{UserID: "m", Moderator: true})
	assert.NoError(t, err, "モデレーターは読める")

	for _, path := range []string{"/admin/settings", "/admin/account/create", "/admin/account/update", "/admin/account/delete"} {
		_, err := r.Call(t, "POST "+path, plugintest.Request{UserID: "m", Moderator: true, Body: `{}`})
		assert.Equal(t, http.StatusForbidden, statusOf(t, err), "モデレーターは変えられない: %s", path)
	}
}

func TestRoutes_AccountLifecycle(t *testing.T) {
	db := testDB(t)
	api := newFakeAPI()
	h := plugintest.New(t).WithName(Name).WithDB(db).WithAPI(api)
	r := h.Routes(Plugin)
	admin := func(body string) plugintest.Request {
		return plugintest.Request{UserID: "admin", Administrator: true, Moderator: true, Body: body}
	}

	res, err := r.Call(t, "POST /admin/state", admin(`{}`))
	require.NoError(t, err)
	assert.Nil(t, res.(*stateResponse).Account)

	_, err = r.Call(t, "POST /admin/account/create", admin(`{"username":"bad name"}`))
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err))

	res, err = r.Call(t, "POST /admin/account/create", admin(`{"username":"claude","name":"クロード"}`))
	require.NoError(t, err)
	st := res.(*stateResponse)
	require.NotNil(t, st.Account)
	assert.Equal(t, "claude", st.Account.Username)
	accs := h.ManagedAccounts()
	require.Len(t, accs, 1)
	require.NotNil(t, accs[0].Description)
	assert.Equal(t, DefaultDescription, *accs[0].Description, "Anthropicへ送られることを既定で書く")
	assert.Contains(t, *accs[0].Description, "Anthropic")
	require.NotNil(t, accs[0].Name)
	assert.Equal(t, "クロード", *accs[0].Name)

	_, err = r.Call(t, "POST /admin/account/create", admin(`{"username":"another"}`))
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err), "botは1つだけ")

	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	_, err = r.Call(t, "POST /admin/account/update", admin(asJSON(t, map[string]any{
		"name": "新しい名前", "description": "説明", "avatar": map[string]any{"data": png, "filename": "a.png"},
	})))
	require.NoError(t, err)
	accs = h.ManagedAccounts()
	assert.Equal(t, "新しい名前", *accs[0].Name)
	assert.Equal(t, "説明", *accs[0].Description)
	require.NotNil(t, accs[0].Avatar)
	assert.Equal(t, "a.png", accs[0].Avatar.Filename)

	_, err = r.Call(t, "POST /admin/account/update", admin(`{"avatar":{"data":"!!!"}}`))
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err))

	_, err = r.Call(t, "POST /admin/account/delete", admin(`{"username":"claud"}`))
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err), "名前を打ち直さないと消さない")
	assert.False(t, h.ManagedAccounts()[0].Deleted)

	res, err = r.Call(t, "POST /admin/account/delete", admin(`{"username":"claude"}`))
	require.NoError(t, err)
	assert.Nil(t, res.(*stateResponse).Account)
	assert.True(t, h.ManagedAccounts()[0].Deleted)
}

func TestRoutes_SaveSettingsValidates(t *testing.T) {
	e := newEnv(t)
	e.api.maxNote = 500
	r := e.h.Routes(Plugin)
	admin := func(s Settings) plugintest.Request {
		return plugintest.Request{UserID: "admin", Administrator: true, Body: asJSON(t, map[string]any{"settings": s})}
	}

	s := defaultSettings()
	s.Model = "claude-sonnet-5"
	s.Reply.SystemPrompt = "返事用"
	s.Scheduled.SystemPrompt = "定時用"
	s.Reply.MaxChars = 500
	s.Scheduled.MaxChars = 100
	s.Scheduled.Enabled = true
	s.Scheduled.Times = []string{"07:00"}
	s.PriceOverrides = []PriceOverride{{Model: "claude-sonnet-5", Price: Price{Input: 1, Output: 2}}}
	res, err := r.Call(t, "POST /admin/settings", admin(s))
	require.NoError(t, err)
	saved := res.(*stateResponse).Settings
	assert.Equal(t, "claude-sonnet-5", saved.Model)
	assert.Equal(t, "返事用", saved.Reply.SystemPrompt)
	assert.Equal(t, "定時用", saved.Scheduled.SystemPrompt)
	assert.Equal(t, 500, res.(*stateResponse).MaxNoteTextLength)
	assert.NotEmpty(t, res.(*stateResponse).Prices)

	bad := []func(*Settings){
		func(s *Settings) { s.Reply.MaxChars = 501 },     // 投稿の文字数の上限より長い
		func(s *Settings) { s.Scheduled.MaxChars = 501 }, // 同上
		func(s *Settings) { s.Model = "" },
		func(s *Settings) { s.Model = "bad model id" },
		func(s *Settings) { s.Effort = "turbo" },
		func(s *Settings) { s.Reply.Visibility = "specified" },
		func(s *Settings) { s.Scheduled.Visibility = "specified" },
		func(s *Settings) { s.Reply.Mode = "shout" },
		func(s *Settings) { s.Reply.Audience = "friends" },
		func(s *Settings) { s.Scheduled.Times = []string{"25:00"} },
		func(s *Settings) { s.Scheduled.IntervalMinutes = 1 },
		func(s *Settings) { s.Scheduled.Times = nil; s.Scheduled.IntervalMinutes = 0 },
		func(s *Settings) { s.Limits.PerUserPerHour = -1 },
		func(s *Settings) { s.Limits.OverLimit = "message"; s.Limits.OverLimitMessage = "" },
		func(s *Settings) { s.Budget.MonthlyUSD = -1 },
		func(s *Settings) { s.Timezone = "Mars/Olympus" },
		func(s *Settings) { s.Reply.MaxTokens = maxTokensCap + 1 },
		func(s *Settings) {
			s.PriceOverrides = []PriceOverride{{Model: "x", Price: Price{Input: -1}}}
		},
	}
	for i, mutate := range bad {
		c := s
		c.Scheduled.Times = append([]string{}, s.Scheduled.Times...)
		mutate(&c)
		_, err := r.Call(t, "POST /admin/settings", admin(c))
		assert.Equal(t, http.StatusBadRequest, statusOf(t, err), "case %d", i)
	}

	// 弾いた設定は保存されていない。
	got, err := loadSettings(t.Context(), e.db)
	require.NoError(t, err)
	assert.Equal(t, 500, got.Reply.MaxChars)
}

func TestRoutes_StateShowsUsageAndEvents(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) { s.Budget.MonthlyUSD = 10 })
	e.addUsage("alice", now(), "claude-opus-5-5", tokens{Input: 1_000_000})
	e.api.notes["n1"] = note("n1", "alice", "alice", "", "hi", "public")
	e.claude.push(apiError(401, "authentication_error", "invalid x-api-key"))
	require.NoError(t, e.mention("x", "n1"))

	res, err := e.h.Routes(Plugin).Call(t, "POST /admin/state", plugintest.Request{Moderator: true})
	require.NoError(t, err)
	st := res.(*stateResponse)
	require.NotNil(t, st.Account)
	assert.Equal(t, e.botID, st.Account.ID)
	assert.True(t, st.Account.IsBot)
	assert.InDelta(t, 4, st.Usage.Month.CostUSD, 1e-9)
	require.NotNil(t, st.Usage.Budget)
	assert.InDelta(t, 6, st.Usage.Budget.RemainingUSD, 1e-9)
	require.NotEmpty(t, st.Events)
	assert.Equal(t, eventAPIError, st.Events[0].Kind)
	assert.Contains(t, st.Events[0].Message, "invalid x-api-key")
}

func TestLoadSettings_DefaultsFillNewFields(t *testing.T) {
	e := newEnv(t)
	// 古い版が保存した、項目の足りない設定。
	_, err := e.db.Exec(`INSERT INTO settings (id, data) VALUES (1, '{"model":"claude-haiku-4-5","reply":{"mode":"reaction"}}')`)
	require.NoError(t, err)
	s, err := loadSettings(t.Context(), e.db)
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", s.Model)
	assert.Equal(t, "reaction", s.Reply.Mode)
	assert.Equal(t, defaultSettings().Reply.MaxChars, s.Reply.MaxChars)
	assert.Equal(t, defaultSettings().Limits.PerUserPerHour, s.Limits.PerUserPerHour)
}

func TestAutoMaxTokens(t *testing.T) {
	assert.Equal(t, 200*3+8192, autoMaxTokens(200, ""), "送らないときはモデルの既定 (多くは high) で考える")
	assert.Equal(t, 200*3+1024, autoMaxTokens(200, "low"))
	assert.Equal(t, 200*3+4096, autoMaxTokens(200, "medium"))
	assert.Equal(t, 200*3+8192, autoMaxTokens(200, "high"))
	assert.Equal(t, 200*3+16384, autoMaxTokens(200, "xhigh"))
	assert.Equal(t, 200*3+16384, autoMaxTokens(200, "max"))
	assert.Equal(t, maxTokensCap, autoMaxTokens(100_000, "low"))
	assert.Greater(t, autoMaxTokens(1, "low"), 1000, "思考の分の余裕を持たせる")
}

func TestRoutes_UpdateSuspendedAccount(t *testing.T) {
	e := newEnv(t)
	e.h.SuspendAccount(e.botID)
	_, err := e.h.Routes(Plugin).Call(t, "POST /admin/account/update",
		plugintest.Request{Administrator: true, Body: `{"name":"x"}`})
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
	assert.Contains(t, err.Error(), "凍結")
}
