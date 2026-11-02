package claudebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"

	"github.com/elythia-network/elythia-plugin-claude-bot/reversi"
)

// ---- 本体のリバーシのフェイク ----

// fakeGame is one game on the fake host.
type fakeGame struct {
	ID, User1, User2 string
	Ready1, Ready2   bool
	// Black is 1 or 2 once started (黒になる利用者の番号)。
	Black     int
	Started   bool
	Ended     bool
	Winner    *string
	Limit     int
	Positions []int
	Map       []string
	Llotheo   bool
	// Expired makes claim-time-is-up end the game.
	Expired bool
	Claims  int
	// LegacyLogs writes the logs in the old [pos, color] shape.
	LegacyLogs bool
	// StartedAt is when the game started. Times are when the moves were put
	// (Unix ms). 足りない分は StartedAt にする。
	StartedAt time.Time
	Times     []int64
	// Joins counts the acceptances the host sent to a remote inviter
	// (show-game by user2 before the start)。
	Joins int
}

// fakeHost simulates the host's reversi endpoints on a fakeAPI.
type fakeHost struct {
	api   *fakeAPI
	games map[string]*fakeGame
	users map[string]userLite
	// startBlack is the black the next started game gets.
	startBlack int
}

func newFakeHost(api *fakeAPI) *fakeHost {
	h := &fakeHost{api: api, games: map[string]*fakeGame{}, users: map[string]userLite{}, startBlack: 2}
	api.handlers["reversi/show-game"] = h.showGame
	api.handlers["reversi/games"] = h.list
	api.handlers["reversi/match"] = func(string, map[string]any) (any, error) {
		// 本体の match は、招待が消えていると相手への新しい招待を作る。bot は呼ばない。
		return nil, fmt.Errorf("fakeHost: reversi/match must not be called")
	}
	api.handlers["reversi/ready"] = h.ready
	api.handlers["reversi/put-stone"] = h.putStone
	api.handlers["reversi/claim-time-is-up"] = h.claim
	api.handlers["reversi/surrender"] = h.surrender
	api.handlers["users/show"] = func(_ string, p map[string]any) (any, error) {
		u, ok := h.users[fmt.Sprint(p["userId"])]
		if !ok {
			return nil, apiErr("users/show", 400, "NO_SUCH_USER")
		}
		return u, nil
	}
	return h
}

func (h *fakeHost) addUser(id, username, host string) {
	var hp *string
	if host != "" {
		hp = &host
	}
	h.users[id] = userLite{ID: id, Username: username, Host: hp}
}

// invite creates a game from user1 to the bot, like reversi/match does.
func (h *fakeHost) invite(id, from, to string) *fakeGame {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	g := &fakeGame{ID: id, User1: from, User2: to, Limit: 90}
	h.games[id] = g
	return g
}

func (h *fakeHost) game(id string) *fakeGame {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	return h.games[id]
}

// with runs fn on a game while holding the lock.
func (h *fakeHost) with(id string, fn func(g *fakeGame)) {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	fn(h.games[id])
}

func (h *fakeHost) view(g *fakeGame) map[string]any {
	logs := make([][]int, 0, len(g.Positions))
	var prev int64
	for i, pos := range g.Positions {
		if g.LegacyLogs {
			logs = append(logs, []int{pos, 1})
			continue
		}
		at := g.StartedAt.UnixMilli()
		if i < len(g.Times) {
			at = g.Times[i]
		}
		// 本体と同じく、最初の手は Unix ミリ秒、以降は前の手からの差。
		logs = append(logs, []int{int(at - prev), 1, 0, pos})
		prev = at
	}
	var startedAt any
	if g.Started {
		startedAt = g.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	var black any
	if g.Black != 0 {
		black = g.Black
	}
	m := g.Map
	if m == nil {
		m = []string{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"}
	}
	v := map[string]any{
		"id": g.ID, "user1Id": g.User1, "user2Id": g.User2, "user1Ready": g.Ready1, "user2Ready": g.Ready2,
		"black": black, "isStarted": g.Started, "isEnded": g.Ended, "winnerId": g.Winner,
		"startedAt": startedAt, "timeLimitForEachTurn": g.Limit, "logs": logs, "map": m, "isLlotheo": g.Llotheo,
		"canPutEverywhere": false, "loopedBoard": false,
	}
	if u, ok := h.users[g.User1]; ok {
		v["user1"] = u
	}
	if u, ok := h.users[g.User2]; ok {
		v["user2"] = u
	}
	return v
}

func (h *fakeHost) find(endpoint string, p map[string]any) (*fakeGame, error) {
	g, ok := h.games[fmt.Sprint(p["gameId"])]
	if !ok {
		return nil, apiErr(endpoint, 400, "NO_SUCH_GAME")
	}
	return g, nil
}

func (h *fakeHost) showGame(as string, p map[string]any) (any, error) {
	g, err := h.find("reversi/show-game", p)
	if err != nil {
		return nil, err
	}
	// 本体は、招待された側が始まる前の対局を開くと、招待者へ受諾を配る。
	if as == g.User2 && !g.Started && !g.Ended {
		g.Joins++
	}
	return h.view(g), nil
}

// list answers reversi/games with my: true (順序は問わない)。
func (h *fakeHost) list(as string, p map[string]any) (any, error) {
	if p["my"] != true {
		return nil, fmt.Errorf("fakeHost: reversi/games without my")
	}
	out := []map[string]any{}
	for _, g := range h.games {
		if g.User1 == as || g.User2 == as {
			out = append(out, h.view(g))
		}
	}
	return out, nil
}

func (h *fakeHost) ready(as string, p map[string]any) (any, error) {
	g, err := h.find("reversi/ready", p)
	if err != nil {
		return nil, err
	}
	if g.Started {
		return nil, apiErr("reversi/ready", 400, "ALREADY_STARTED")
	}
	r, _ := p["ready"].(bool)
	if as == g.User1 {
		g.Ready1 = r
	} else {
		g.Ready2 = r
	}
	if g.Ready1 && g.Ready2 {
		g.Started, g.Black, g.StartedAt = true, h.startBlack, now()
	}
	return nil, nil
}

func (g *fakeGame) colorOf(id string) reversi.Color {
	if (g.Black == 1) == (id == g.User1) {
		return reversi.Black
	}
	return reversi.White
}

func (g *fakeGame) userOf(c reversi.Color) string {
	if g.colorOf(g.User1) == c {
		return g.User1
	}
	return g.User2
}

func (h *fakeHost) putStone(as string, p map[string]any) (any, error) {
	g, err := h.find("reversi/put-stone", p)
	if err != nil {
		return nil, err
	}
	if !g.Started || g.Ended {
		return nil, apiErr("reversi/put-stone", 400, "NOT_STARTED")
	}
	cur, err := reversi.Replay(g.Positions)
	if err != nil {
		return nil, err
	}
	if cur.Turn != g.colorOf(as) {
		return nil, apiErr("reversi/put-stone", 400, "NOT_YOUR_TURN")
	}
	pos := int(p["pos"].(float64))
	next, err := cur.Play(pos)
	if err != nil {
		return nil, apiErr("reversi/put-stone", 400, "INVALID_MOVE")
	}
	for len(g.Times) < len(g.Positions) {
		g.Times = append(g.Times, g.StartedAt.UnixMilli())
	}
	g.Positions = append(g.Positions, pos)
	g.Times = append(g.Times, now().UnixMilli())
	if next.Over {
		g.Ended = true
		b, w := next.Board.Count(reversi.Black), next.Board.Count(reversi.White)
		switch {
		case b > w:
			id := g.userOf(reversi.Black)
			g.Winner = &id
		case w > b:
			id := g.userOf(reversi.White)
			g.Winner = &id
		}
	}
	return nil, nil
}

func (h *fakeHost) claim(as string, p map[string]any) (any, error) {
	g, err := h.find("reversi/claim-time-is-up", p)
	if err != nil {
		return nil, err
	}
	g.Claims++
	if g.Expired && g.Started && !g.Ended {
		g.Ended = true
		id := as
		g.Winner = &id
	}
	return nil, nil
}

func (h *fakeHost) surrender(as string, p map[string]any) (any, error) {
	g, err := h.find("reversi/surrender", p)
	if err != nil {
		return nil, err
	}
	g.Ended = true
	w := g.User1
	if as == g.User1 {
		w = g.User2
	}
	g.Winner = &w
	return nil, nil
}

// ---- 環境 ----

type reversiEnv struct {
	*env
	host *fakeHost
	jobs *plugintest.JobSet
}

func newReversiEnv(t *testing.T, mutate func(*ReversiSettings)) *reversiEnv {
	t.Helper()
	e := newEnv(t)
	host := newFakeHost(e.api)
	host.addUser("alice", "alice", "")
	host.addUser("bob", "bob", "")
	host.addUser("carol", "carol", "remote.example")
	host.addUser(e.botID, "claudebot", "")
	e.settings(func(s *Settings) {
		s.Reversi.Enabled = true
		if mutate != nil {
			mutate(&s.Reversi)
		}
	})
	return &reversiEnv{env: e, host: host, jobs: e.h.Jobs(Plugin)}
}

func (e *reversiEnv) invited(gameID, from string) error {
	e.t.Helper()
	return e.h.Notify(plugin.Notification{Type: plugin.NotificationReversiInvited, AccountID: e.botID, UserID: from, GameID: gameID})
}

func (e *reversiEnv) progressed(gameID, event string) error {
	e.t.Helper()
	g := e.host.game(gameID)
	from := ""
	if g != nil {
		from = g.User1
	}
	return e.h.Notify(plugin.Notification{Type: plugin.NotificationReversiGame, AccountID: e.botID, UserID: from, GameID: gameID, GameEvent: event})
}

// record returns the status and result of a game's row ("" when missing).
func (e *reversiEnv) record(gameID string) (status, result string) {
	e.t.Helper()
	var r *string
	err := e.db.QueryRow(`SELECT status, result FROM reversi_games WHERE game_id = $1`, gameID).Scan(&status, &r)
	if err != nil {
		return "", ""
	}
	if r != nil {
		result = *r
	}
	return status, result
}

// enqueued returns the jobs of name enqueued so far.
func (e *reversiEnv) enqueued(name string) []plugintest.Enqueued {
	var out []plugintest.Enqueued
	for _, q := range e.h.Enqueues() {
		if q.Name == name {
			out = append(out, q)
		}
	}
	return out
}

func (e *reversiEnv) lastJob(name string) (plugintest.Enqueued, reversiMoveJob) {
	e.t.Helper()
	qs := e.enqueued(name)
	require.NotEmpty(e.t, qs, "no %s job", name)
	var p reversiMoveJob
	require.NoError(e.t, json.Unmarshal(qs[len(qs)-1].Payload, &p))
	return qs[len(qs)-1], p
}

func (e *reversiEnv) callsAs(endpoint string) []apiCall { return e.api.callsTo(endpoint) }

// accept takes an invitation from alice and gets the bot ready.
func (e *reversiEnv) accept(gameID string) *fakeGame {
	e.t.Helper()
	g := e.host.invite(gameID, "alice", e.botID)
	require.NoError(e.t, e.invited(gameID, "alice"))
	return g
}

// start gets alice ready, which starts the game, and tells the bot.
func (e *reversiEnv) start(gameID string, black int) {
	e.t.Helper()
	e.host.with(gameID, func(g *fakeGame) {
		g.Ready1, g.Started, g.Black = true, true, black
		if g.StartedAt.IsZero() {
			g.StartedAt = now()
		}
	})
	require.NoError(e.t, e.progressed(gameID, plugin.ReversiGameStarted))
}

// firstLegalGame plays up to n moves from the start, each on the first legal
// square, and returns them with the game after them.
func firstLegalGame(t *testing.T, n int) ([]int, reversi.Game) {
	t.Helper()
	g := reversi.NewGame()
	var pos []int
	for range n {
		if g.Over {
			break
		}
		legal := g.Board.Legal(g.Turn)
		p := 0
		for legal&(1<<uint(p)) == 0 {
			p++
		}
		next, err := g.Play(p)
		require.NoError(t, err)
		g = next
		pos = append(pos, p)
	}
	return pos, g
}

// blackFor returns the black (1 or 2) that makes the bot (user2) the color to
// play in g.
func blackFor(g reversi.Game) int {
	if g.Turn == reversi.Black {
		return 2
	}
	return 1
}

// ---- 招待 ----

func TestReversiAcceptsInvitation(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")

	status, _ := e.record("g1")
	assert.Equal(t, reversiWaiting, status)
	// 受けるかは reversi/games で決め、受けたら show-game で受ける (本体が受諾を配る)。
	games := e.callsAs("reversi/games")
	require.Len(t, games, 1)
	assert.Equal(t, e.botID, games[0].As)
	assert.Equal(t, 1, e.host.game("g1").Joins)
	ready := e.callsAs("reversi/ready")
	require.Len(t, ready, 1)
	assert.Equal(t, true, ready[0].Params["ready"])
	assert.True(t, e.host.game("g1").Ready2)

	q := e.enqueued(jobReversiReadyTimeout)
	require.Len(t, q, 1)
	assert.Equal(t, 10*time.Minute, q[0].Options.Delay)
	assert.Equal(t, reversiJobAttempts, q[0].Options.MaxAttempts)
	assert.JSONEq(t, fmt.Sprintf(`{"accountId":%q,"gameId":"g1"}`, e.botID), string(q[0].Payload))

	var level int
	var acct string
	require.NoError(t, e.db.QueryRow(`SELECT level, opponent_acct FROM reversi_games WHERE game_id = 'g1'`).Scan(&level, &acct))
	assert.Equal(t, reversi.DefaultLevel, level)
	assert.Equal(t, "@alice", acct)
	assert.NotEmpty(t, e.events(eventReversi))
	// 相手は一覧に載っているので、読みに行かない。
	assert.Empty(t, e.callsAs("users/show"))
}

func TestReversiReadyTimeoutIsShorterThanTheCleanup(t *testing.T) {
	// 準備の期限は掃除より先に来ること。掃除が先に記録を閉じると、準備を外す
	// のが次の通知まで遅れる。
	assert.Less(t, time.Duration(maxReversiReadyTimeout)*time.Minute, reversiStale)
}

func TestReversiDuplicateInvitation(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	require.NoError(t, e.invited("g1", "alice"))

	// 2回目は記録のある対局として読み直すだけ。記録も準備も増やさない。
	assert.Len(t, e.callsAs("reversi/games"), 1)
	assert.Len(t, e.callsAs("reversi/show-game"), 2)
	assert.Len(t, e.callsAs("reversi/ready"), 1)
	assert.Len(t, e.enqueued(jobReversiReadyTimeout), 1)
	var n int
	require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM reversi_games`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestReversiRefusesInvitations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReversiSettings)
		setup  func(e *reversiEnv)
		from   string
	}{
		{name: "disabled", mutate: func(r *ReversiSettings) { r.Enabled = false }, from: "alice"},
		{name: "remote under local", from: "carol"},
		{name: "not a follower", mutate: func(r *ReversiSettings) { r.Audience = "followers" }, from: "alice"},
		{name: "llotheo", from: "alice", setup: func(e *reversiEnv) {
			e.host.with("g1", func(g *fakeGame) { g.Llotheo = true })
		}},
		{name: "no time limit", from: "alice", setup: func(e *reversiEnv) {
			e.host.with("g1", func(g *fakeGame) { g.Limit = 0 })
		}},
		{name: "short time limit", from: "alice", setup: func(e *reversiEnv) {
			e.host.with("g1", func(g *fakeGame) { g.Limit = reversiMinTurnSeconds - 1 })
		}},
		{name: "long time limit", from: "alice", setup: func(e *reversiEnv) {
			e.host.with("g1", func(g *fakeGame) { g.Limit = reversiMaxTurnSeconds + 1 })
		}},
		{name: "same opponent", from: "alice", setup: func(e *reversiEnv) {
			e.accept("g0")
		}},
		{name: "max games", mutate: func(r *ReversiSettings) { r.MaxGames = 1 }, from: "alice", setup: func(e *reversiEnv) {
			e.host.invite("g0", "bob", e.botID)
			require.NoError(e.t, e.invited("g0", "bob"))
		}},
		{name: "not found", from: "alice", setup: func(e *reversiEnv) {
			e.api.handlers["reversi/games"] = func(string, map[string]any) (any, error) { return []any{}, nil }
		}},
		{name: "already started", from: "alice", setup: func(e *reversiEnv) {
			e.host.with("g1", func(g *fakeGame) { g.Started, g.Black = true, 1 })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newReversiEnv(t, tt.mutate)
			e.host.invite("g1", tt.from, e.botID)
			if tt.setup != nil {
				tt.setup(e)
			}
			require.NoError(t, e.invited("g1", tt.from))
			// 受けない招待は show-game で読まない。読むと本体が招待者へ受諾を配る。
			assert.Zero(t, e.host.game("g1").Joins, "must not accept")
			assert.False(t, e.host.game("g1").Ready2)
			status, _ := e.record("g1")
			assert.Empty(t, status)
		})
	}
}

func TestReversiDisabledReadsNothing(t *testing.T) {
	e := newReversiEnv(t, func(r *ReversiSettings) { r.Enabled = false })
	e.host.invite("g1", "alice", e.botID)
	require.NoError(t, e.invited("g1", "alice"))
	assert.Empty(t, e.callsAs("reversi/games"))
}

func TestReversiAcceptBoundaries(t *testing.T) {
	for _, limit := range []int{reversiMinTurnSeconds, reversiMaxTurnSeconds} {
		e := newReversiEnv(t, nil)
		e.host.invite("g1", "alice", e.botID)
		e.host.with("g1", func(g *fakeGame) { g.Limit = limit })
		require.NoError(t, e.invited("g1", "alice"))
		status, _ := e.record("g1")
		assert.Equal(t, reversiWaiting, status, limit)
	}
}

func TestReversiAudience(t *testing.T) {
	t.Run("followers", func(t *testing.T) {
		e := newReversiEnv(t, func(r *ReversiSettings) { r.Audience = "followers" })
		e.api.followers["alice"] = true
		e.accept("g1")
		status, _ := e.record("g1")
		assert.Equal(t, reversiWaiting, status)
	})
	t.Run("everyone", func(t *testing.T) {
		e := newReversiEnv(t, func(r *ReversiSettings) { r.Audience = "everyone" })
		e.host.invite("g1", "carol", e.botID)
		require.NoError(t, e.invited("g1", "carol"))
		var acct string
		require.NoError(t, e.db.QueryRow(`SELECT opponent_acct FROM reversi_games`).Scan(&acct))
		assert.Equal(t, "@carol@remote.example", acct)
	})
	t.Run("relation fails", func(t *testing.T) {
		e := newReversiEnv(t, func(r *ReversiSettings) { r.Audience = "followers" })
		e.api.fail["users/relation"] = apiErr("users/relation", 500, "INTERNAL_ERROR")
		e.host.invite("g1", "alice", e.botID)
		require.Error(t, e.invited("g1", "alice"))
		assert.Zero(t, e.host.game("g1").Joins)
	})
}

func TestReversiOpponentFromUsersShow(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.host.invite("g1", "dave", e.botID)
	e.host.addUser("dave", "dave", "")
	// 一覧が相手を載せないとき (本体が読み込まなかった) は users/show で読む。
	e.api.handlers["reversi/games"] = func(as string, p map[string]any) (any, error) {
		v, err := e.host.list(as, p)
		for _, m := range v.([]map[string]any) {
			delete(m, "user1")
		}
		return v, err
	}
	require.NoError(t, e.invited("g1", "dave"))
	assert.Len(t, e.callsAs("users/show"), 1)
	var acct string
	require.NoError(t, e.db.QueryRow(`SELECT opponent_acct FROM reversi_games`).Scan(&acct))
	assert.Equal(t, "@dave", acct)

	e.host.invite("g2", "erin", e.botID)
	require.Error(t, e.invited("g2", "erin"), "an unknown opponent is an error")
}

func TestReversiInvitationNotAddressedToTheBot(t *testing.T) {
	e := newReversiEnv(t, nil)
	// bot が招待した側 (user1) の対局と、bot の居ない対局は受けない。
	e.host.invite("g1", e.botID, "alice")
	require.NoError(t, e.invited("g1", "alice"))
	e.host.invite("g2", "alice", "bob")
	require.NoError(t, e.invited("g2", "alice"))
	assert.Empty(t, e.callsAs("reversi/show-game"))
	assert.Empty(t, e.callsAs("reversi/ready"))
}

func TestReversiAcceptFails(t *testing.T) {
	t.Run("listing fails", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.host.invite("g1", "alice", e.botID)
		e.api.fail["reversi/games"] = apiErr("reversi/games", 500, "INTERNAL_ERROR")
		require.Error(t, e.invited("g1", "alice"))
		status, _ := e.record("g1")
		assert.Empty(t, status)
	})
	t.Run("the game is gone after recording", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.host.invite("g1", "alice", e.botID)
		e.api.fail["reversi/show-game"] = apiErr("reversi/show-game", 400, "NO_SUCH_GAME")
		require.NoError(t, e.invited("g1", "alice"))
		status, _ := e.record("g1")
		assert.Equal(t, reversiCanceled, status)
		assert.Empty(t, e.callsAs("reversi/ready"))
	})
	t.Run("server error is retried", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.host.invite("g1", "alice", e.botID)
		e.api.fail["reversi/show-game"] = apiErr("reversi/show-game", 500, "INTERNAL_ERROR")
		require.Error(t, e.invited("g1", "alice"))
		delete(e.api.fail, "reversi/show-game")
		// 再試行では、記録が残っているので読み直して (受けて) から準備する。
		require.NoError(t, e.invited("g1", "alice"))
		assert.Equal(t, 1, e.host.game("g1").Joins)
		assert.Len(t, e.callsAs("reversi/ready"), 1)
		assert.Len(t, e.enqueued(jobReversiReadyTimeout), 1)
	})
}

func TestReversiShowGameFails(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.api.fail["reversi/show-game"] = apiErr("reversi/show-game", 500, "INTERNAL_ERROR")
	require.Error(t, e.progressed("g1", plugin.ReversiGameReadyStates))
	status, _ := e.record("g1")
	assert.Equal(t, reversiWaiting, status)
}

func TestReversiReadyFails(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.host.invite("g1", "alice", e.botID)
	e.api.fail["reversi/ready"] = apiErr("reversi/ready", 500, "INTERNAL_ERROR")
	require.Error(t, e.invited("g1", "alice"))
	e.api.fail["reversi/ready"] = apiErr("reversi/ready", 400, "ALREADY_STARTED")
	require.NoError(t, e.invited("g1", "alice"))
}

// ---- 開始前 ----

func TestReversiReadyAgainAfterSettingsChange(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	// 相手が設定を変えると、本体は両者の準備を外す。
	e.host.with("g1", func(g *fakeGame) { g.Ready2 = false })
	require.NoError(t, e.progressed("g1", plugin.ReversiGameSettings))
	assert.Len(t, e.callsAs("reversi/ready"), 2)
	assert.True(t, e.host.game("g1").Ready2)
}

func TestReversiGivesUpNonStandardBoard(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.host.with("g1", func(g *fakeGame) { g.Llotheo = true })
	require.NoError(t, e.progressed("g1", plugin.ReversiGameSettings))

	ready := e.callsAs("reversi/ready")
	require.Len(t, ready, 2)
	assert.Equal(t, false, ready[1].Params["ready"])
	assert.False(t, e.host.game("g1").Ready2)
	status, _ := e.record("g1")
	assert.Equal(t, reversiCanceled, status)
	// やめた後で盤が標準に戻っても、準備し直さない。
	e.host.with("g1", func(g *fakeGame) { g.Llotheo = false })
	require.NoError(t, e.progressed("g1", plugin.ReversiGameSettings))
	assert.Len(t, e.callsAs("reversi/ready"), 2)

	// 準備していなければ、外しに行かない。
	e.host.invite("g2", "bob", e.botID)
	require.NoError(t, e.invited("g2", "bob"))
	e.host.with("g2", func(g *fakeGame) { g.Ready2, g.Map = false, []string{"--------"} })
	require.NoError(t, e.progressed("g2", plugin.ReversiGameSettings))
	assert.Len(t, e.callsAs("reversi/ready"), 3)
	status, _ = e.record("g2")
	assert.Equal(t, reversiCanceled, status)
}

func TestReversiGiveUpReadyFails(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.host.with("g1", func(g *fakeGame) { g.Llotheo = true })
	e.api.fail["reversi/ready"] = apiErr("reversi/ready", 500, "INTERNAL_ERROR")
	require.Error(t, e.progressed("g1", plugin.ReversiGameSettings))
	// 記録は先に閉じる。準備を外すのは、通知の再試行が閉じた記録として行う。
	status, _ := e.record("g1")
	assert.Equal(t, reversiCanceled, status)
	assert.True(t, e.host.game("g1").Ready2)
	delete(e.api.fail, "reversi/ready")
	require.NoError(t, e.progressed("g1", plugin.ReversiGameSettings))
	assert.False(t, e.host.game("g1").Ready2)
}

func TestReversiSettlesClosedGames(t *testing.T) {
	t.Run("ready after closing", func(t *testing.T) {
		// 記録を閉じた後で準備した (並行した通知の競合) か、掃除が準備を外さずに
		// 閉じた。相手が準備したときの通知で、準備を外す。
		e := newReversiEnv(t, nil)
		e.accept("g1")
		_, err := e.db.Exec(`UPDATE reversi_games SET status = 'canceled'`)
		require.NoError(t, err)
		require.NoError(t, e.progressed("g1", plugin.ReversiGameReadyStates))
		ready := e.callsAs("reversi/ready")
		require.Len(t, ready, 2)
		assert.Equal(t, false, ready[1].Params["ready"])
		// もう準備していなければ何もしない。
		require.NoError(t, e.progressed("g1", plugin.ReversiGameReadyStates))
		assert.Len(t, e.callsAs("reversi/ready"), 2)
	})
	t.Run("started after closing", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		_, err := e.db.Exec(`UPDATE reversi_games SET status = 'canceled'`)
		require.NoError(t, err)
		e.host.with("g1", func(g *fakeGame) { g.Ready1, g.Started, g.Black, g.StartedAt = true, true, 2, now() })
		require.NoError(t, e.progressed("g1", plugin.ReversiGameStarted))
		assert.Len(t, e.callsAs("reversi/surrender"), 1)
		assert.Empty(t, e.enqueued(jobReversiMove))
		// 終わった対局には何もしない。
		require.NoError(t, e.progressed("g1", plugin.ReversiGameEnded))
		assert.Len(t, e.callsAs("reversi/surrender"), 1)
		status, _ := e.record("g1")
		assert.Equal(t, reversiCanceled, status)
	})
	t.Run("surrender fails", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		_, err := e.db.Exec(`UPDATE reversi_games SET status = 'canceled'`)
		require.NoError(t, err)
		e.host.with("g1", func(g *fakeGame) { g.Ready1, g.Started, g.Black, g.StartedAt = true, true, 2, now() })
		e.api.fail["reversi/surrender"] = apiErr("reversi/surrender", 500, "INTERNAL_ERROR")
		require.Error(t, e.progressed("g1", plugin.ReversiGameStarted))
		e.api.fail["reversi/surrender"] = apiErr("reversi/surrender", 400, "ALREADY_ENDED")
		require.NoError(t, e.progressed("g1", plugin.ReversiGameStarted))
	})
}

func TestReversiIgnoresGamesItDidNotAccept(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.host.invite("g1", "alice", e.botID)
	require.NoError(t, e.progressed("g1", plugin.ReversiGameReadyStates))
	assert.Empty(t, e.callsAs("reversi/ready"))
	assert.Empty(t, e.h.Enqueues())
	// 読むと本体が招待者へ受諾を配るので、受けていない対局は読まない。
	assert.Empty(t, e.callsAs("reversi/show-game"))
}

func TestReversiCanceledGame(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.host.with("g1", func(*fakeGame) { delete(e.host.games, "g1") })
	require.NoError(t, e.progressed("g1", plugin.ReversiGameCanceled))
	status, _ := e.record("g1")
	assert.Equal(t, reversiCanceled, status)
}

func TestReversiReadyTimeout(t *testing.T) {
	payload := func(e *reversiEnv, id string) string {
		return fmt.Sprintf(`{"accountId":%q,"gameId":%q}`, e.botID, id)
	}
	t.Run("not started", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		require.NoError(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
		assert.False(t, e.host.game("g1").Ready2)
		status, _ := e.record("g1")
		assert.Equal(t, reversiCanceled, status)
	})
	t.Run("started", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		// 開始の通知が届く前に期限が来ても、取りやめずに対局を進める。
		e.host.with("g1", func(g *fakeGame) { g.Ready1, g.Started, g.Black = true, true, 2 })
		require.NoError(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
		status, _ := e.record("g1")
		assert.Equal(t, reversiPlaying, status)
		assert.Len(t, e.enqueued(jobReversiMove), 1)
	})
	t.Run("already playing", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.start("g1", 2)
		require.NoError(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
		assert.Len(t, e.callsAs("reversi/show-game"), 2, "must not read the game again")
	})
	t.Run("canceled", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.host.with("g1", func(*fakeGame) { delete(e.host.games, "g1") })
		require.NoError(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
		status, _ := e.record("g1")
		assert.Equal(t, reversiCanceled, status)
	})
	t.Run("not ready", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.host.with("g1", func(g *fakeGame) { g.Ready2 = false })
		require.NoError(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
		assert.Len(t, e.callsAs("reversi/ready"), 1, "nothing to undo")
		status, _ := e.record("g1")
		assert.Equal(t, reversiCanceled, status)
	})
	t.Run("errors", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.api.fail["reversi/ready"] = apiErr("reversi/ready", 500, "INTERNAL_ERROR")
		require.Error(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
		status, _ := e.record("g1")
		assert.Equal(t, reversiCanceled, status)
		// 再試行は閉じた記録でも読み直し、準備を外す。
		delete(e.api.fail, "reversi/ready")
		require.NoError(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
		assert.False(t, e.host.game("g1").Ready2)
		e.api.fail["reversi/show-game"] = apiErr("reversi/show-game", 500, "INTERNAL_ERROR")
		require.Error(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
	})
	t.Run("ended record", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		_, err := e.db.Exec(`UPDATE reversi_games SET status = 'ended'`)
		require.NoError(t, err)
		n := len(e.callsAs("reversi/show-game"))
		require.NoError(t, e.jobs.Run(t, jobReversiReadyTimeout, payload(e, "g1")))
		assert.Len(t, e.callsAs("reversi/show-game"), n)
	})
	t.Run("bad payload", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		err := e.jobs.Run(t, jobReversiReadyTimeout, `{"gameId":"g1"}`)
		require.Error(t, err)
		assert.True(t, errors.Is(err, plugin.ErrNoRetry))
		err = e.jobs.Run(t, jobReversiReadyTimeout, `[`)
		require.Error(t, err)
		assert.True(t, errors.Is(err, plugin.ErrNoRetry))
	})
}

// ---- 対局 ----

func TestReversiBotPlaysWhenItIsItsTurn(t *testing.T) {
	e := newReversiEnv(t, func(r *ReversiSettings) { r.Level = 5 })
	e.accept("g1")
	e.start("g1", 2) // bot (user2) が黒、先手

	var color string
	require.NoError(t, e.db.QueryRow(`SELECT color FROM reversi_games WHERE game_id = 'g1'`).Scan(&color))
	assert.Equal(t, "black", color)
	status, _ := e.record("g1")
	assert.Equal(t, reversiPlaying, status)

	q, p := e.lastJob(jobReversiMove)
	assert.Equal(t, 0, p.Ply)
	assert.Equal(t, reversiMoveDedup, q.Options.DedupTTL)
	assert.Equal(t, reversiJobAttempts, q.Options.MaxAttempts)
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	assert.Len(t, e.host.game("g1").Positions, 1)
	assert.Empty(t, e.enqueued(jobReversiTimeout))

	// 自分の手の log が届くと、相手の番なので時間切れの確認を積む。
	require.NoError(t, e.progressed("g1", plugin.ReversiGameLog))
	q, p = e.lastJob(jobReversiTimeout)
	assert.Equal(t, 1, p.Ply)
	assert.Equal(t, 90*time.Second+reversiTimeoutGrace, q.Options.Delay)
	assert.Equal(t, q.Options.Delay+reversiMoveDedup, q.Options.DedupTTL)
	assert.Equal(t, reversiJobAttempts, q.Options.MaxAttempts)
}

func TestReversiBotIsWhite(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.start("g1", 1) // alice が黒
	assert.Empty(t, e.enqueued(jobReversiMove))
	_, p := e.lastJob(jobReversiTimeout)
	assert.Equal(t, 0, p.Ply)
	var color string
	require.NoError(t, e.db.QueryRow(`SELECT color FROM reversi_games WHERE game_id = 'g1'`).Scan(&color))
	assert.Equal(t, "white", color)

	e.host.with("g1", func(g *fakeGame) { g.Positions = []int{19} })
	require.NoError(t, e.progressed("g1", plugin.ReversiGameLog))
	_, p = e.lastJob(jobReversiMove)
	assert.Equal(t, 1, p.Ply)
}

func TestReversiTimeoutCountsFromTheLastMove(t *testing.T) {
	// 確かめる時刻は、通知が届いた時刻ではなく、本体が記録した手の時刻から数える。
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.host.with("g1", func(g *fakeGame) {
		g.StartedAt = now().Add(-time.Minute)
		g.Positions, g.Times = []int{19}, []int64{now().Add(-30 * time.Second).UnixMilli()}
	})
	e.start("g1", 2) // bot が黒で、黒が19に打った後なので白 (相手) の番
	q, _ := e.lastJob(jobReversiTimeout)
	assert.Equal(t, 60*time.Second+reversiTimeoutGrace, q.Options.Delay)

	// 持ち時間を過ぎていれば、すぐに確かめる。
	e.host.with("g1", func(g *fakeGame) { g.Times = []int64{now().Add(-2 * time.Minute).UnixMilli()} })
	require.NoError(t, e.progressed("g1", plugin.ReversiGameLog))
	q, _ = e.lastJob(jobReversiTimeout)
	assert.Equal(t, reversiTimeoutGrace, q.Options.Delay)
}

func TestReversiTurnDeadline(t *testing.T) {
	setClock(t, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	start := now().Add(-time.Hour)
	ms := func(t time.Time) int { return int(t.UnixMilli()) }
	tests := []struct {
		name    string
		started *time.Time
		logs    string
		want    time.Time
	}{
		{name: "no moves", started: &start, logs: `[]`, want: start.Add(90 * time.Second)},
		{name: "moves", started: &start, logs: fmt.Sprintf(`[[%d,1,0,19],[5000,0,0,18]]`, ms(start.Add(time.Second))), want: start.Add(6*time.Second + 90*time.Second)},
		{name: "legacy logs", started: &start, logs: `[[19,1]]`, want: start.Add(90 * time.Second)},
		{name: "no start", logs: `[]`, want: now().Add(90 * time.Second)},
		{name: "broken logs", started: &start, logs: `{}`, want: start.Add(90 * time.Second)},
	}
	for _, tt := range tests {
		g := reversiGameView{ID: "g", StartedAt: tt.started, TimeLimitForEachTurn: 90, Logs: json.RawMessage(tt.logs)}
		assert.True(t, tt.want.Equal(g.turnDeadline()), "%s: %v != %v", tt.name, g.turnDeadline(), tt.want)
	}
}

func TestReversiLegacyLogs(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.host.with("g1", func(g *fakeGame) { g.LegacyLogs, g.Positions = true, []int{19} })
	e.start("g1", 1)
	_, p := e.lastJob(jobReversiMove)
	assert.Equal(t, 1, p.Ply)
}

func TestReversiBrokenGame(t *testing.T) {
	t.Run("illegal logs", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.host.with("g1", func(g *fakeGame) { g.Positions = []int{0} })
		e.host.with("g1", func(g *fakeGame) { g.Ready1, g.Started, g.Black = true, true, 2 })
		err := e.progressed("g1", plugin.ReversiGameStarted)
		require.Error(t, err)
		assert.True(t, errors.Is(err, plugin.ErrNoRetry))
	})
	t.Run("undecodable logs", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.host.with("g1", func(g *fakeGame) { g.Ready1, g.Started, g.Black = true, true, 2 })
		e.api.handlers["reversi/show-game"] = func(as string, p map[string]any) (any, error) {
			v, err := e.host.showGame(as, p)
			v.(map[string]any)["logs"] = "x"
			return v, err
		}
		err := e.progressed("g1", plugin.ReversiGameStarted)
		require.Error(t, err)
		assert.True(t, errors.Is(err, plugin.ErrNoRetry))
	})
	for _, black := range []int{0, 3} {
		t.Run(fmt.Sprintf("black=%d", black), func(t *testing.T) {
			e := newReversiEnv(t, nil)
			e.accept("g1")
			e.host.with("g1", func(g *fakeGame) { g.Ready1, g.Started, g.Black = true, true, black })
			err := e.progressed("g1", plugin.ReversiGameStarted)
			require.Error(t, err)
			assert.True(t, errors.Is(err, plugin.ErrNoRetry))
		})
	}
}

func TestReversiStartedOnNonStandardBoard(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.host.with("g1", func(g *fakeGame) { g.Llotheo = true })
	e.start("g1", 2)
	require.Len(t, e.callsAs("reversi/surrender"), 1)
	assert.True(t, e.host.game("g1").Ended)
	require.NoError(t, e.progressed("g1", plugin.ReversiGameEnded))
	status, result := e.record("g1")
	assert.Equal(t, reversiEnded, status)
	assert.Equal(t, "lose", result)
	// 対応していない盤の石の数は、標準の盤として数えても意味が無いので書かない。
	var stones *int
	var color string
	require.NoError(t, e.db.QueryRow(`SELECT bot_stones, color FROM reversi_games WHERE game_id = 'g1'`).Scan(&stones, &color))
	assert.Nil(t, stones)
	assert.Equal(t, "black", color)

	e2 := newReversiEnv(t, nil)
	e2.accept("g1")
	e2.host.with("g1", func(g *fakeGame) { g.Llotheo = true })
	e2.api.fail["reversi/surrender"] = apiErr("reversi/surrender", 500, "INTERNAL_ERROR")
	e2.host.with("g1", func(g *fakeGame) { g.Ready1, g.Started, g.Black = true, true, 2 })
	require.Error(t, e2.progressed("g1", plugin.ReversiGameStarted))
}

func TestReversiMoveJobSkipsStaleTurns(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.start("g1", 2)
	q, _ := e.lastJob(jobReversiMove)
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	// 同じ手数の job がもう一度動いても、手数がずれているので打たない。
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	assert.Len(t, e.callsAs("reversi/put-stone"), 1)

	// 手数が合っていても相手の番なら打たない。
	stale := fmt.Sprintf(`{"accountId":%q,"gameId":"g1","ply":1}`, e.botID)
	require.NoError(t, e.jobs.Run(t, jobReversiMove, stale))
	assert.Len(t, e.callsAs("reversi/put-stone"), 1)

	// 再び自分の番 (手数2) になっても、手数0の古い job では打たない。
	pos, game := firstLegalGame(t, 2)
	require.Equal(t, reversi.Black, game.Turn)
	e.host.with("g1", func(g *fakeGame) { g.Positions = pos })
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	assert.Len(t, e.callsAs("reversi/put-stone"), 1)
}

func TestReversiMoveJobWithoutAnOpenRecord(t *testing.T) {
	e := newReversiEnv(t, nil)
	payload := fmt.Sprintf(`{"accountId":%q,"gameId":"g1","ply":0}`, e.botID)
	require.NoError(t, e.jobs.Run(t, jobReversiMove, payload))
	e.accept("g1")
	// 始まる前 (waiting) にも打たない。
	require.NoError(t, e.jobs.Run(t, jobReversiMove, payload))
	assert.Empty(t, e.callsAs("reversi/put-stone"))
	assert.Len(t, e.callsAs("reversi/show-game"), 1, "a job without a playing record reads nothing")
}

func TestReversiMoveJobAfterTheGameEnded(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.start("g1", 2)
	q, _ := e.lastJob(jobReversiMove)
	e.host.with("g1", func(g *fakeGame) { g.Ended, g.Winner = true, &e.botID })
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	assert.Empty(t, e.callsAs("reversi/put-stone"))
	// 終局の通知が届かなくても、job が結果つきで閉じる。
	status, result := e.record("g1")
	assert.Equal(t, reversiEnded, status)
	assert.Equal(t, "win", result)
}

func TestReversiMoveJobAfterTheGameWasCanceled(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.start("g1", 2)
	q, _ := e.lastJob(jobReversiMove)
	e.host.with("g1", func(*fakeGame) { delete(e.host.games, "g1") })
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	status, _ := e.record("g1")
	assert.Equal(t, reversiCanceled, status)
}

func TestReversiMoveJobErrors(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.start("g1", 2)
	q, _ := e.lastJob(jobReversiMove)

	e.api.fail["reversi/put-stone"] = apiErr("reversi/put-stone", 400, "NOT_YOUR_TURN")
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	e.api.fail["reversi/put-stone"] = apiErr("reversi/put-stone", 500, "INTERNAL_ERROR")
	require.Error(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	delete(e.api.fail, "reversi/put-stone")

	e.api.fail["reversi/show-game"] = apiErr("reversi/show-game", 500, "INTERNAL_ERROR")
	require.Error(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	delete(e.api.fail, "reversi/show-game")

	_, err := e.db.Exec(`UPDATE settings SET data = '{"reversi":1}'`)
	require.NoError(t, err)
	require.Error(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)), "unreadable settings")
	e.settings(nil)

	e.host.with("g1", func(g *fakeGame) { g.Black = 0 })
	err = e.jobs.Run(t, jobReversiMove, string(q.Payload))
	require.Error(t, err)
	assert.True(t, errors.Is(err, plugin.ErrNoRetry))
	e.host.with("g1", func(g *fakeGame) { g.Black, g.Positions = 2, []int{0} })
	err = e.jobs.Run(t, jobReversiMove, string(q.Payload))
	require.Error(t, err)
	assert.True(t, errors.Is(err, plugin.ErrNoRetry))

	_, err = e.db.Exec(`UPDATE reversi_games SET level = 9`)
	require.NoError(t, err)
	e.host.with("g1", func(g *fakeGame) { g.Positions = nil })
	err = e.jobs.Run(t, jobReversiMove, string(q.Payload))
	require.Error(t, err)
	assert.True(t, errors.Is(err, plugin.ErrNoRetry), "an unknown level never works")

	err = e.jobs.Run(t, jobReversiMove, `{"accountId":"x"}`)
	require.Error(t, err)
	assert.True(t, errors.Is(err, plugin.ErrNoRetry))
}

func TestReversiThinkTime(t *testing.T) {
	// 残りの持ち時間の 1/3 と設定の上限の小さい方で探索する。上限を大きくしても、
	// 手番の残りが 1 秒なら 1/3 秒ほどで打つ。キューで待たされた分も引かれる。
	e := newReversiEnv(t, func(r *ReversiSettings) { r.Level = 5; r.MaxThinkMs = 10000 })
	e.accept("g1")
	// 打てる場所の多い中盤 (段階5は時間の上限まで読み続ける)。
	pos, game := firstLegalGame(t, 14)
	e.host.with("g1", func(g *fakeGame) {
		// 残り3秒。その1/3の1秒で打つ (残りを全部使うと3秒)。
		g.Limit, g.Positions = reversiMinTurnSeconds, pos
		g.StartedAt = now().Add(-time.Duration(reversiMinTurnSeconds-3) * time.Second)
	})
	e.start("g1", blackFor(game))
	q, _ := e.lastJob(jobReversiMove)
	start := time.Now()
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Len(t, e.callsAs("reversi/put-stone"), 1)
}

func TestReversiTimeoutJob(t *testing.T) {
	t.Run("claims and the game ends", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.start("g1", 1)
		q, _ := e.lastJob(jobReversiTimeout)
		e.host.with("g1", func(g *fakeGame) { g.Expired = true })
		require.NoError(t, e.jobs.Run(t, jobReversiTimeout, string(q.Payload)))
		assert.Equal(t, 1, e.host.game("g1").Claims)
		assert.Len(t, e.enqueued(jobReversiTimeout), 1, "no retry once it ended")

		require.NoError(t, e.progressed("g1", plugin.ReversiGameEnded))
		status, result := e.record("g1")
		assert.Equal(t, reversiEnded, status)
		assert.Equal(t, "win", result)
	})
	t.Run("too early claims again", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.start("g1", 1)
		q, p := e.lastJob(jobReversiTimeout)
		payload := string(q.Payload)
		for i := 1; i < reversiTimeoutAttempts; i++ {
			require.NoError(t, e.jobs.Run(t, jobReversiTimeout, payload))
			var next plugintest.Enqueued
			next, p = e.lastJob(jobReversiTimeout)
			assert.Equal(t, i, p.Attempt)
			assert.Equal(t, reversiTimeoutRetry, next.Options.Delay)
			payload = string(next.Payload)
		}
		n := len(e.enqueued(jobReversiTimeout))
		require.NoError(t, e.jobs.Run(t, jobReversiTimeout, payload))
		assert.Len(t, e.enqueued(jobReversiTimeout), n, "gives up after the last attempt")
		assert.Equal(t, reversiTimeoutAttempts, e.host.game("g1").Claims)
	})
	t.Run("the opponent moved", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.start("g1", 1)
		q, _ := e.lastJob(jobReversiTimeout)
		e.host.with("g1", func(g *fakeGame) { g.Positions = []int{19} })
		require.NoError(t, e.jobs.Run(t, jobReversiTimeout, string(q.Payload)))
		assert.Zero(t, e.host.game("g1").Claims)
	})
	t.Run("own turn", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.start("g1", 2)
		payload := fmt.Sprintf(`{"accountId":%q,"gameId":"g1","ply":0}`, e.botID)
		require.NoError(t, e.jobs.Run(t, jobReversiTimeout, payload))
		assert.Zero(t, e.host.game("g1").Claims)
	})
	t.Run("errors", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.start("g1", 1)
		q, _ := e.lastJob(jobReversiTimeout)
		e.api.fail["reversi/claim-time-is-up"] = apiErr("reversi/claim-time-is-up", 400, "NOT_STARTED")
		require.NoError(t, e.jobs.Run(t, jobReversiTimeout, string(q.Payload)))
		e.api.fail["reversi/claim-time-is-up"] = apiErr("reversi/claim-time-is-up", 500, "INTERNAL_ERROR")
		require.Error(t, e.jobs.Run(t, jobReversiTimeout, string(q.Payload)))
		err := e.jobs.Run(t, jobReversiTimeout, `{}`)
		require.Error(t, err)
		assert.True(t, errors.Is(err, plugin.ErrNoRetry))
	})
	t.Run("rereading fails", func(t *testing.T) {
		e := newReversiEnv(t, nil)
		e.accept("g1")
		e.start("g1", 1)
		q, _ := e.lastJob(jobReversiTimeout)
		calls := 0
		e.api.handlers["reversi/show-game"] = func(as string, p map[string]any) (any, error) {
			calls++
			if calls > 1 {
				return nil, apiErr("reversi/show-game", 500, "INTERNAL_ERROR")
			}
			return e.host.showGame(as, p)
		}
		require.Error(t, e.jobs.Run(t, jobReversiTimeout, string(q.Payload)))
	})
}

func TestReversiResults(t *testing.T) {
	tests := []struct {
		name   string
		winner func(e *reversiEnv) *string
		want   string
	}{
		{name: "win", winner: func(e *reversiEnv) *string { return &e.botID }, want: "win"},
		{name: "lose", winner: func(*reversiEnv) *string { s := "alice"; return &s }, want: "lose"},
		{name: "draw", winner: func(*reversiEnv) *string { return nil }, want: "draw"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newReversiEnv(t, nil)
			e.accept("g1")
			e.start("g1", 2)
			e.host.with("g1", func(g *fakeGame) {
				g.Positions = []int{19}
				g.Ended, g.Winner = true, tt.winner(e)
			})
			require.NoError(t, e.progressed("g1", plugin.ReversiGameEnded))
			status, result := e.record("g1")
			assert.Equal(t, reversiEnded, status)
			assert.Equal(t, tt.want, result)
			var mine, theirs int
			require.NoError(t, e.db.QueryRow(`SELECT bot_stones, opponent_stones FROM reversi_games`).Scan(&mine, &theirs))
			assert.Equal(t, 4, mine) // 黒が19に置くと、黒4・白1
			assert.Equal(t, 1, theirs)
			// 閉じた後の通知では何もしない。
			require.NoError(t, e.progressed("g1", plugin.ReversiGameEnded))
		})
	}
}

func TestReversiEndedBeforeStarted(t *testing.T) {
	// 始まる前に終わった (取り消しの扱いが本体で変わったなど) 対局も閉じる。
	// 色が無いので石の数は書かない。
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.host.with("g1", func(g *fakeGame) { g.Ended = true })
	require.NoError(t, e.progressed("g1", plugin.ReversiGameEnded))
	status, result := e.record("g1")
	assert.Equal(t, reversiEnded, status)
	assert.Equal(t, "draw", result)
	var stones *int
	require.NoError(t, e.db.QueryRow(`SELECT bot_stones FROM reversi_games`).Scan(&stones))
	assert.Nil(t, stones)
}

func TestReversiGameOverWithoutTheEndedEvent(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	// 最後の手の log が ended より先に届いた。打つ手が無いので何も積まない。
	pos, game := firstLegalGame(t, 100)
	require.True(t, game.Over, "the fixture must be a finished game")
	e.host.with("g1", func(g *fakeGame) {
		g.Positions = pos
		g.Ready1, g.Started, g.Black = true, true, 2
	})
	require.NoError(t, e.progressed("g1", plugin.ReversiGameLog))
	assert.Empty(t, e.enqueued(jobReversiMove))
	assert.Empty(t, e.enqueued(jobReversiTimeout))

	payload := fmt.Sprintf(`{"accountId":%q,"gameId":"g1","ply":%d}`, e.botID, len(pos))
	require.NoError(t, e.jobs.Run(t, jobReversiMove, payload))
	assert.Empty(t, e.callsAs("reversi/put-stone"))
}

// TestReversiFullGame plays a whole game: the bot against an opponent that
// always puts on the first legal square.
func TestReversiFullGame(t *testing.T) {
	for _, black := range []int{1, 2} {
		t.Run(fmt.Sprintf("black=%d", black), func(t *testing.T) {
			e := newReversiEnv(t, func(r *ReversiSettings) { r.Level = 1; r.MaxThinkMs = 100 })
			e.accept("g1")
			e.start("g1", black)
			for range 200 {
				g := e.host.game("g1")
				if g.Ended {
					break
				}
				game, err := reversi.Replay(g.Positions)
				require.NoError(t, err)
				if game.Turn == g.colorOf("alice") {
					legal := game.Board.Legal(game.Turn)
					pos := 0
					for legal&(1<<uint(pos)) == 0 {
						pos++
					}
					_, err := e.api.AsUser("alice").Call(context.Background(), "reversi/put-stone", map[string]any{"gameId": "g1", "pos": pos})
					require.NoError(t, err)
				} else {
					_, p := e.lastJob(jobReversiMove)
					require.Equal(t, len(g.Positions), p.Ply)
					raw, _ := json.Marshal(p)
					require.NoError(t, e.jobs.Run(t, jobReversiMove, string(raw)))
				}
				event := plugin.ReversiGameLog
				if e.host.game("g1").Ended {
					event = plugin.ReversiGameEnded
				}
				require.NoError(t, e.progressed("g1", event))
			}
			g := e.host.game("g1")
			require.True(t, g.Ended)
			status, result := e.record("g1")
			assert.Equal(t, reversiEnded, status)
			assert.NotEmpty(t, result)
			var mine, theirs int
			require.NoError(t, e.db.QueryRow(`SELECT bot_stones, opponent_stones FROM reversi_games`).Scan(&mine, &theirs))
			final, err := reversi.Replay(g.Positions)
			require.NoError(t, err)
			assert.Equal(t, final.Board.Count(g.colorOf(e.botID)), mine)
			assert.Equal(t, final.Board.Count(g.colorOf("alice")), theirs)
		})
	}
}

// ---- 同時の対局の数 ----

func TestReversiMaxGamesUnderConcurrency(t *testing.T) {
	e := newReversiEnv(t, func(r *ReversiSettings) { r.MaxGames = 2 })
	rs := defaultSettings().Reversi
	rs.MaxGames = 2
	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opp := userLite{ID: fmt.Sprintf("u%d", i), Username: fmt.Sprintf("u%d", i)}
			ok, _, err := insertReversiGame(context.Background(), e.db, e.botID, fmt.Sprintf("g%d", i), opp, rs)
			assert.NoError(t, err)
			results[i] = ok
		}()
	}
	wg.Wait()
	n := 0
	for _, ok := range results {
		if ok {
			n++
		}
	}
	assert.Equal(t, 2, n)
}

func TestReversiInsertDuplicate(t *testing.T) {
	e := newReversiEnv(t, nil)
	rs := defaultSettings().Reversi
	alice := userLite{ID: "alice", Username: "alice"}
	ok, reason, err := insertReversiGame(context.Background(), e.db, e.botID, "g1", alice, rs)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, reason)
	// 同じ対局は、記録済みとして理由なしで断る。
	_, err = e.db.Exec(`UPDATE reversi_games SET status = 'ended'`)
	require.NoError(t, err)
	ok, reason, err = insertReversiGame(context.Background(), e.db, e.botID, "g1", alice, rs)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, reason)
}

// ---- 掃除・管理画面・アカウント ----

func TestPruneReversi(t *testing.T) {
	e := newReversiEnv(t, nil)
	at := now()
	rows := []struct {
		id, status string
		updated    time.Time
	}{
		{"fresh", reversiPlaying, at.Add(-time.Hour)},
		{"stale", reversiPlaying, at.Add(-reversiStale - time.Minute)},
		{"stale-waiting", reversiWaiting, at.Add(-reversiStale - time.Minute)},
		{"old", reversiEnded, at.AddDate(0, 0, -401)},
		{"kept", reversiEnded, at.AddDate(0, 0, -399)},
	}
	for i, r := range rows {
		_, err := e.db.Exec(`
INSERT INTO reversi_games (game_id, account_id, opponent_id, opponent_acct, level, status, created_at, updated_at)
VALUES ($1, 'bot', $2, '@x', 3, $3, $4, $4)`, r.id, fmt.Sprintf("u%d", i), r.status, r.updated)
		require.NoError(t, err)
	}
	require.NoError(t, e.jobs.Run(t, jobTick, ""))
	for id, want := range map[string]string{"fresh": reversiPlaying, "stale": reversiCanceled, "stale-waiting": reversiCanceled, "old": "", "kept": reversiEnded} {
		status, _ := e.record(id)
		assert.Equal(t, want, status, id)
	}
}

func TestReversiGamesRoute(t *testing.T) {
	e := newReversiEnv(t, nil)
	routes := e.h.Routes(Plugin)
	_, err := routes.Call(t, "POST /admin/reversi/games", plugintest.Request{Moderator: true})
	require.Error(t, err, "moderators cannot read the record")

	e.accept("g1")
	e.host.invite("g2", "bob", e.botID)
	require.NoError(t, e.invited("g2", "bob"))
	e.start("g2", 2)
	e.host.with("g2", func(g *fakeGame) { g.Ended, g.Winner, g.Positions = true, &e.botID, []int{19} })
	require.NoError(t, e.progressed("g2", plugin.ReversiGameEnded))
	for i, result := range []string{"lose", "draw"} {
		_, err := e.db.Exec(`
INSERT INTO reversi_games (game_id, account_id, opponent_id, opponent_acct, level, status, result, created_at, updated_at)
VALUES ($1, $2, $3, '@x', 3, 'ended', $4, $5, $5)`, fmt.Sprintf("old%d", i), e.botID, fmt.Sprintf("o%d", i), result, now().Add(-time.Hour))
		require.NoError(t, err)
	}

	res, err := routes.Call(t, "POST /admin/reversi/games", plugintest.Request{Administrator: true})
	require.NoError(t, err)
	got := res.(*reversiGamesResponse)
	assert.Equal(t, reversiRecordSummary{Wins: 1, Losses: 1, Draws: 1}, got.Summary)
	require.Len(t, got.Games, 4)
	assert.Equal(t, "g1", got.Games[0].GameID, "games in progress come first")
	assert.Equal(t, reversiWaiting, got.Games[0].Status)
	assert.Equal(t, "g2", got.Games[1].GameID)
	require.NotNil(t, got.Games[1].Result)
	assert.Equal(t, "win", *got.Games[1].Result)
	require.NotNil(t, got.Games[1].BotStones)
	assert.Equal(t, 4, *got.Games[1].BotStones)

	_, err = e.db.Exec(`DROP TABLE reversi_games`)
	require.NoError(t, err)
	_, err = routes.Call(t, "POST /admin/reversi/games", plugintest.Request{Administrator: true})
	require.Error(t, err)
}

func TestDeleteAccountForgetsReversiGames(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	routes := e.h.Routes(Plugin)
	_, err := routes.Call(t, "POST /admin/account/delete", plugintest.Request{Administrator: true, Body: `{"username":"claudebot"}`})
	require.NoError(t, err)
	var n int
	require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM reversi_games`).Scan(&n))
	assert.Zero(t, n)
}

func TestReversiGameViewPositions(t *testing.T) {
	tests := []struct {
		logs string
		want []int
	}{
		{logs: ``, want: nil},
		{logs: `null`, want: nil},
		{logs: `[[100,1,0,19],[5,0,0,18]]`, want: []int{19, 18}},
		{logs: `[[19,1]]`, want: []int{19}},
		// 置く以外の操作と形の違う要素は読み飛ばす。
		{logs: `[[100,1,1,19],[1,2,3],[100,0,0,18]]`, want: []int{18}},
	}
	for _, tt := range tests {
		g := reversiGameView{ID: "g", Logs: json.RawMessage(tt.logs)}
		got, err := g.positions()
		require.NoError(t, err, tt.logs)
		if tt.want == nil {
			assert.Empty(t, got, tt.logs)
		} else {
			assert.Equal(t, tt.want, got, tt.logs)
		}
	}
	_, err := reversiGameView{ID: "g", Logs: json.RawMessage(`{}`)}.positions()
	assert.ErrorContains(t, err, "decode logs")
}

func TestReversiSettingsCheck(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReversiSettings)
		want   string
	}{
		{name: "audience", mutate: func(r *ReversiSettings) { r.Audience = "none" }, want: "範囲"},
		{name: "level low", mutate: func(r *ReversiSettings) { r.Level = reversi.MinLevel - 1 }, want: "強さ"},
		{name: "level high", mutate: func(r *ReversiSettings) { r.Level = reversi.MaxLevel + 1 }, want: "強さ"},
		{name: "think low", mutate: func(r *ReversiSettings) { r.MaxThinkMs = minReversiThinkMs - 1 }, want: "考える時間"},
		{name: "think high", mutate: func(r *ReversiSettings) { r.MaxThinkMs = maxReversiThinkMs + 1 }, want: "考える時間"},
		{name: "games low", mutate: func(r *ReversiSettings) { r.MaxGames = 0 }, want: "対局の数"},
		{name: "games high", mutate: func(r *ReversiSettings) { r.MaxGames = maxReversiGames + 1 }, want: "対局の数"},
		{name: "ready low", mutate: func(r *ReversiSettings) { r.ReadyTimeoutMinutes = 0 }, want: "準備"},
		{name: "ready high", mutate: func(r *ReversiSettings) { r.ReadyTimeoutMinutes = maxReversiReadyTimeout + 1 }, want: "準備"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := defaultSettings()
			tt.mutate(&s.Reversi)
			_, err := s.validate(3000)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
	edges := []func(*ReversiSettings){
		func(r *ReversiSettings) {
			r.Level, r.MaxThinkMs, r.MaxGames, r.ReadyTimeoutMinutes = reversi.MinLevel, minReversiThinkMs, 1, 1
			r.Audience = "followers"
		},
		func(r *ReversiSettings) {
			r.Level, r.MaxThinkMs, r.MaxGames, r.ReadyTimeoutMinutes = reversi.MaxLevel, maxReversiThinkMs, maxReversiGames, maxReversiReadyTimeout
			r.Audience = "everyone"
		},
	}
	for _, m := range edges {
		s := defaultSettings()
		m(&s.Reversi)
		_, err := s.validate(3000)
		assert.NoError(t, err)
	}
}

func TestReversiSettingsDefaultsForOldData(t *testing.T) {
	e := newEnv(t)
	// リバーシより前に保存した設定にも、既定値が入る。
	_, err := e.db.Exec(`INSERT INTO settings (id, data) VALUES (1, '{"model":"claude-opus-5-5"}')`)
	require.NoError(t, err)
	s, err := loadSettings(context.Background(), e.db)
	require.NoError(t, err)
	assert.Equal(t, defaultSettings().Reversi, s.Reversi)
	assert.False(t, s.Reversi.Enabled)
	_, err = s.validate(3000)
	require.NoError(t, err)
}

func TestReversiDatabaseErrors(t *testing.T) {
	e := newReversiEnv(t, nil)
	e.accept("g1")
	e.start("g1", 2)
	q, _ := e.lastJob(jobReversiMove)
	e.host.invite("g2", "bob", e.botID)
	_, err := e.db.Exec(`DROP TABLE reversi_games`)
	require.NoError(t, err)

	require.Error(t, e.invited("g2", "bob"), "counting the games fails")
	require.Error(t, e.progressed("g1", plugin.ReversiGameLog))
	require.Error(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	require.Error(t, e.jobs.Run(t, jobReversiReadyTimeout, fmt.Sprintf(`{"accountId":%q,"gameId":"g1"}`, e.botID)))
	require.Error(t, e.jobs.Run(t, jobTick, ""))

	e.host.with("g1", func(*fakeGame) { delete(e.host.games, "g1") })
	require.Error(t, e.progressed("g1", plugin.ReversiGameCanceled), "closing the record fails")

	// アカウントは消せる。戦績を消せなかったことは記録に残す。
	routes := e.h.Routes(Plugin)
	_, err = routes.Call(t, "POST /admin/account/delete", plugintest.Request{Administrator: true, Body: `{"username":"claudebot"}`})
	require.NoError(t, err)
	assert.NotEmpty(t, e.events(eventReversi))
}

func TestReversiRecordWritesFail(t *testing.T) {
	// 記録は読めるが書けない (列の制約で落とす) ときも、エラーを返して再試行させる。
	e := newReversiEnv(t, nil)
	e.accept("g1")
	_, err := e.db.Exec(`ALTER TABLE reversi_games ADD CONSTRAINT no_change CHECK (status = 'waiting' AND updated_at = created_at)`)
	require.NoError(t, err)
	e.host.with("g1", func(g *fakeGame) { g.Ready1, g.Started, g.Black = true, true, 2 })
	require.Error(t, e.progressed("g1", plugin.ReversiGameStarted), "starting the record fails")
	e.host.with("g1", func(g *fakeGame) { g.Ended = true })
	require.Error(t, e.progressed("g1", plugin.ReversiGameEnded), "finishing the record fails")
}

func TestReversiGameViewAsUser1(t *testing.T) {
	black := 1
	g := reversiGameView{ID: "g", User1ID: "a", User2ID: "b", User1Ready: true, Black: &black,
		User2: &userLite{ID: "b", Username: "b"}}
	assert.True(t, g.ready("a"))
	assert.False(t, g.ready("b"))
	c, err := g.color("a")
	require.NoError(t, err)
	assert.Equal(t, reversi.Black, c)
	c, err = g.color("b")
	require.NoError(t, err)
	assert.Equal(t, reversi.White, c)
}

func TestReversiThinkTimeFloor(t *testing.T) {
	// 手番を過ぎていても、1手先の評価だけはして打つ。
	e := newReversiEnv(t, func(r *ReversiSettings) { r.Level = 5 })
	e.accept("g1")
	pos, game := firstLegalGame(t, 14)
	e.host.with("g1", func(g *fakeGame) { g.Positions, g.StartedAt = pos, now().Add(-time.Hour) })
	e.start("g1", blackFor(game))
	q, _ := e.lastJob(jobReversiMove)
	start := time.Now()
	require.NoError(t, e.jobs.Run(t, jobReversiMove, string(q.Payload)))
	assert.Less(t, time.Since(start), time.Second)
	assert.Len(t, e.host.game("g1").Positions, len(pos)+1)
}
