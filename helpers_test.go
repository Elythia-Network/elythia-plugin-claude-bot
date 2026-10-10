package claudebot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
	_ "github.com/jackc/pgx/v5/stdlib"
)

/*
 * テストの道具。
 *
 * DB は本物の PostgreSQL を使う (フェイクの DB は本物とずれる)。本体の API と
 * Claude API はフェイクにする。Claude API は HTTP の層で差し替えるので、
 * 公式 SDK が組み立てるリクエストと、応答の読み取りまで本物が通る。
 */

const testSchema = "plugin_claude_bot_test"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func dbUnavailable(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("MK_PLUGIN_TESTS_REQUIRE_DB") != "" {
		t.Fatalf("PostgreSQL に接続できません (MK_PLUGIN_TESTS_REQUIRE_DB が設定されているので skip しません): %v", err)
	}
	t.Skipf("PostgreSQL に接続できません: %v", err)
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		envOr("TEST_DB_HOST", "localhost"), envOr("TEST_DB_PORT", "5432"),
		envOr("TEST_DB_USER", "mk"), envOr("TEST_DB_PASS", "mk"),
		envOr("TEST_DB_NAME", "misskey_test"))

	admin, err := sql.Open("pgx", base)
	if err != nil {
		dbUnavailable(t, err)
	}
	defer admin.Close() //nolint:errcheck // 使い捨て
	if err := admin.Ping(); err != nil {
		dbUnavailable(t, err)
	}
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`,
		`CREATE SCHEMA ` + testSchema,
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("pgx", base+" search_path="+testSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if a, err := sql.Open("pgx", base); err == nil {
			_, _ = a.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
			_ = a.Close()
		}
	})
	return db
}

// setClock fixes now() for the test.
func setClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = prev })
}

// ---- Claude API のフェイク ----

type claudeReply struct {
	status int
	body   string
	// netErr makes the round trip fail before any HTTP response.
	netErr error
}

// fakeClaude answers POST /v1/messages from a queue of replies.
//
// 添付の画像の取得も同じ ctx.HTTP() を通るので、Claude API 以外のホストへの
// リクエストは files から返す (#6)。
type fakeClaude struct {
	mu       sync.Mutex
	replies  []claudeReply
	requests []capturedRequest
	// files maps a URL to its response. 無い URL は 404 を返す。
	files map[string]fileReply
	// fetched lists the URLs requested outside Claude API, in order.
	fetched []string
}

// fileReply is the response to a GET for an attachment. status 0 fails the
// round trip before any HTTP response.
type fileReply struct {
	status int
	body   []byte
}

// serveFile makes url return body with 200.
func (f *fakeClaude) serveFile(url string, body []byte) {
	f.serveFileStatus(url, http.StatusOK, body)
}

func (f *fakeClaude) serveFileStatus(url string, status int, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string]fileReply{}
	}
	f.files[url] = fileReply{status: status, body: body}
}

func (f *fakeClaude) fetchedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetched...)
}

func (f *fakeClaude) serveAttachment(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	url := req.URL.String()
	f.fetched = append(f.fetched, url)
	r, ok := f.files[url]
	f.mu.Unlock()
	if !ok {
		r = fileReply{status: http.StatusNotFound, body: []byte("not found")}
	}
	if r.status == 0 {
		return nil, errors.New("connection refused (test)")
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:       io.NopCloser(bytes.NewReader(r.body)),
		Request:    req,
	}, nil
}

type capturedRequest struct {
	Header http.Header
	Path   string
	Body   map[string]any
}

func (f *fakeClaude) push(r ...claudeReply) {
	f.mu.Lock()
	f.replies = append(f.replies, r...)
	f.mu.Unlock()
}

func (f *fakeClaude) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "api.anthropic.com" {
		return f.serveAttachment(req)
	}
	raw, _ := io.ReadAll(req.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.requests = append(f.requests, capturedRequest{Header: req.Header.Clone(), Path: req.URL.Path, Body: body})
	var r claudeReply
	if len(f.replies) == 0 {
		r = claudeReply{status: 500, body: `{"type":"error","error":{"type":"api_error","message":"no reply queued in test"}}`}
	} else {
		r = f.replies[0]
		f.replies = f.replies[1:]
	}
	f.mu.Unlock()
	if r.netErr != nil {
		return nil, r.netErr
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(r.body)),
		Request:    req,
	}, nil
}

func (f *fakeClaude) calls() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRequest(nil), f.requests...)
}

func message(text, stop string, in, out int) claudeReply {
	content := []map[string]any{
		// 思考のブロックは本文に入れないことを確かめるため、毎回付ける。
		{"type": "thinking", "thinking": "", "signature": "sig"},
	}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	body, _ := json.Marshal(map[string]any{
		"id":            "msg_test",
		"type":          "message",
		"role":          "assistant",
		"model":         "claude-opus-5-5",
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                in,
			"output_tokens":               out,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	})
	return claudeReply{status: 200, body: string(body)}
}

func apiError(status int, typ, msg string) claudeReply {
	return claudeReply{status: status, body: fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)}
}

// ---- 本体の API のフェイク ----

type apiCall struct {
	As       string
	Endpoint string
	Params   map[string]any
}

// fakeAPI answers the endpoints the bot uses.
type fakeAPI struct {
	mu    sync.Mutex
	calls []apiCall

	notes        map[string]map[string]any
	conversation map[string][]map[string]any
	// emojis maps a local emoji name to the roles that can use it.
	emojis      map[string][]string
	followers   map[string]bool
	maxNote     int
	createFails []int  // notes/create の応答の status を順に返す (0 は成功)
	reactError  string // notes/reactions/create が返すエラーの code (空なら成功)
	// fail makes an endpoint return the given error.
	fail map[string]error
	// raw makes an endpoint return the given body as is.
	raw map[string]string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		notes:        map[string]map[string]any{},
		conversation: map[string][]map[string]any{},
		emojis:       map[string][]string{},
		followers:    map[string]bool{},
		maxNote:      3000,
		fail:         map[string]error{},
		raw:          map[string]string{},
	}
}

func (a *fakeAPI) Anonymous() plugin.Caller       { return &fakeCaller{api: a} }
func (a *fakeAPI) AsUser(id string) plugin.Caller { return &fakeCaller{api: a, as: id} }

func (a *fakeAPI) callsTo(endpoint string) []apiCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []apiCall
	for _, c := range a.calls {
		if c.Endpoint == endpoint {
			out = append(out, c)
		}
	}
	return out
}

type fakeCaller struct {
	api *fakeAPI
	as  string
}

func apiErr(endpoint string, status int, code string) error {
	return &plugin.APIError{Endpoint: endpoint, Status: status,
		Body: json.RawMessage(fmt.Sprintf(`{"error":{"code":%q,"message":"test"}}`, code))}
}

func (c *fakeCaller) Call(_ context.Context, endpoint string, params any) (json.RawMessage, error) {
	a := c.api
	raw, _ := json.Marshal(params)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, apiCall{As: c.as, Endpoint: endpoint, Params: p})
	if err, ok := a.fail[endpoint]; ok {
		return nil, err
	}
	if body, ok := a.raw[endpoint]; ok {
		return json.RawMessage(body), nil
	}

	reply := func(v any) (json.RawMessage, error) {
		b, err := json.Marshal(v)
		return b, err
	}
	switch endpoint {
	case "meta":
		return reply(map[string]any{"maxNoteTextLength": a.maxNote})
	case "notes/show":
		n, ok := a.notes[fmt.Sprint(p["noteId"])]
		if !ok {
			return nil, apiErr(endpoint, 400, "NO_SUCH_NOTE")
		}
		return reply(n)
	case "notes/conversation":
		return reply(append([]map[string]any{}, a.conversation[fmt.Sprint(p["noteId"])]...))
	case "emoji":
		roles, ok := a.emojis[fmt.Sprint(p["name"])]
		if !ok {
			return nil, apiErr(endpoint, 400, "NO_SUCH_EMOJI")
		}
		return reply(map[string]any{"name": p["name"], "roleIdsThatCanBeUsedThisEmojiAsReaction": roles})
	case "users/relation":
		return reply(map[string]any{"id": p["userId"], "isFollowed": a.followers[fmt.Sprint(p["userId"])]})
	case "notes/create":
		if len(a.createFails) > 0 {
			st := a.createFails[0]
			a.createFails = a.createFails[1:]
			if st != 0 {
				return nil, apiErr(endpoint, st, "TEST_FAILURE")
			}
		}
		return reply(map[string]any{"createdNote": map[string]any{"id": "created"}})
	case "notes/reactions/create":
		if a.reactError != "" {
			return nil, apiErr(endpoint, 400, a.reactError)
		}
		return nil, nil
	case "i":
		return reply(map[string]any{"id": c.as, "name": "Claude", "description": DefaultDescription, "avatarUrl": nil, "isBot": true})
	}
	return nil, fmt.Errorf("fakeAPI: %s は用意していません", endpoint)
}

// note builds a packed note.
func note(id, userID, username, host, text, visibility string) map[string]any {
	var h any
	if host != "" {
		h = host
	}
	return map[string]any{
		"id":         id,
		"text":       text,
		"cw":         nil,
		"visibility": visibility,
		"localOnly":  false,
		"user":       map[string]any{"id": userID, "username": username, "host": h, "isBot": false},
		"replyId":    nil,
		"fileIds":    []string{},
	}
}

// ---- ひとまとまりの環境 ----

func plugintestHarness(t *testing.T, db *sql.DB, api *fakeAPI, fc *fakeClaude, secrets map[string]string) *plugintest.Harness {
	t.Helper()
	h := plugintest.New(t).WithName(Name).WithDB(db).WithAPI(api).WithHTTPClient(&http.Client{Transport: fc})
	if secrets != nil {
		h.WithSecrets(secrets)
	}
	h.Notifications(Plugin)
	return h
}

type env struct {
	t      *testing.T
	h      *plugintest.Harness
	db     *sql.DB
	api    *fakeAPI
	claude *fakeClaude
	botID  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testDB(t)
	api := newFakeAPI()
	fc := &fakeClaude{}
	h := plugintestHarness(t, db, api, fc, map[string]string{secretAPIKey: "sk-ant-test-key-0000"})
	acc := h.SeedAccount(Name, "claudebot")
	setClock(t, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	return &env{t: t, h: h, db: db, api: api, claude: fc, botID: acc.ID}
}

func (e *env) settings(mutate func(*Settings)) {
	e.t.Helper()
	s := defaultSettings()
	if mutate != nil {
		mutate(&s)
	}
	if err := saveSettings(context.Background(), e.db, s); err != nil {
		e.t.Fatal(err)
	}
}

// mention delivers a mention notification for noteID.
func (e *env) mention(id, noteID string) error {
	e.t.Helper()
	// 本体と同じく、通知を作った時点の公開範囲を載せる。
	vis := ""
	e.api.mu.Lock()
	if n, ok := e.api.notes[noteID]; ok {
		vis, _ = n["visibility"].(string)
	}
	e.api.mu.Unlock()
	return e.h.Notify(plugin.Notification{ID: id, Type: plugin.NotificationMention, AccountID: e.botID, UserID: "alice", NoteID: noteID, NoteVisibility: vis})
}

func (e *env) events(kind string) []event {
	e.t.Helper()
	evs, err := recentEvents(context.Background(), e.db, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []event
	for _, ev := range evs {
		if kind == "" || ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env) usageRows() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM usage_log`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) addUsage(userID string, at time.Time, model string, tk tokens) {
	e.t.Helper()
	var uid any
	if userID != "" {
		uid = userID
	}
	if _, err := e.db.Exec(`
INSERT INTO usage_log (at, kind, user_id, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
VALUES ($1, 'reply', $2, $3, $4, $5, $6, $7)`, at, uid, model, tk.Input, tk.Output, tk.CacheCreation, tk.CacheRead); err != nil {
		e.t.Fatal(err)
	}
}

func systemOf(r capturedRequest) string {
	blocks, _ := r.Body["system"].([]any)
	var sb strings.Builder
	for _, b := range blocks {
		m, _ := b.(map[string]any)
		s, _ := m["text"].(string)
		sb.WriteString(s)
	}
	return sb.String()
}

func promptOf(r capturedRequest) string {
	msgs, _ := r.Body["messages"].([]any)
	var sb strings.Builder
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		content, _ := mm["content"].([]any)
		for _, c := range content {
			cm, _ := c.(map[string]any)
			s, _ := cm["text"].(string)
			sb.WriteString(s)
		}
	}
	return sb.String()
}
