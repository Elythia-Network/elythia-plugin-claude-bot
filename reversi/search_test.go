package reversi

import (
	"context"
	"errors"
	"math/bits"
	"math/rand/v2"
	"testing"
	"time"
)

// brute returns the final stone difference for p with best play, without
// pruning. 読み切りの答え合わせに使う。
func brute(p, o uint64) int {
	mv := moves(p, o)
	if mv == 0 {
		if moves(o, p) == 0 {
			return bits.OnesCount64(p) - bits.OnesCount64(o)
		}
		return -brute(o, p)
	}
	best := -65
	for x := mv; x != 0; x &= x - 1 {
		np, no := play(p, o, bits.TrailingZeros64(x))
		best = max(best, -brute(no, np))
	}
	return best
}

// randomGame plays random moves from the start until empties squares are
// left, and returns a game that is not over.
func randomGame(t *testing.T, rng *rand.Rand, empties int) Game {
	t.Helper()
	for range 1000 {
		g := NewGame()
		for !g.Over && bits.OnesCount64(g.Board.Empty()) > empties {
			legal := positions(g.Board.Legal(g.Turn))
			next, err := g.Play(legal[rng.IntN(len(legal))])
			if err != nil {
				t.Fatal(err)
			}
			g = next
		}
		if !g.Over && bits.OnesCount64(g.Board.Empty()) == empties {
			return g
		}
	}
	t.Fatalf("no game with %d empty squares", empties)
	return Game{}
}

func stones(g Game) (uint64, uint64) {
	return g.Board.Stones(g.Turn), g.Board.Stones(g.Turn.Opponent())
}

func newSearcher() *searcher {
	return &searcher{ctx: context.Background(), deadline: time.Now().Add(time.Hour), bounded: true}
}

func TestSolveMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	// 総当たりは空きが1つ増えるごとに数倍遅くなるので、8 までにする。
	for i := range 40 {
		empties := 5 + i%4
		g := randomGame(t, rng, empties)
		p, o := stones(g)
		want := brute(p, o)

		if got := newSearcher().solve(p, o, -64, 64); got != want {
			t.Fatalf("position %d: solve = %d, brute = %d", i, got, want)
		}
		m, ok := newSearcher().solveRoot(p, o, moves(p, o))
		if !ok || !m.Exact || m.Score != want || m.Depth != empties {
			t.Fatalf("position %d: solveRoot = %+v, %v; want score %d", i, m, ok, want)
		}
		// 選んだ手が、本当にその値になる手であること。
		np, no := play(p, o, m.Pos)
		if got := -brute(no, np); got != want {
			t.Fatalf("position %d: the chosen move %d is worth %d, want %d", i, m.Pos, got, want)
		}
	}
}

func TestChooseSolvesTheEndgame(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for i := range 10 {
		g := randomGame(t, rng, 8)
		p, o := stones(g)
		if bits.OnesCount64(moves(p, o)) < 2 {
			continue
		}
		want := brute(p, o)
		m, err := Choose(context.Background(), g, Options{Level: 4, MaxTime: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if !m.Exact || m.Score != want {
			t.Fatalf("position %d: Choose = %+v, want exact %d", i, m, want)
		}
	}
}

func TestChooseFallsBackWhenTheSolveStops(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	g := randomGame(t, rng, 16)
	m, err := Choose(context.Background(), g, Options{Level: 5, MaxNodes: 2000, MaxTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if m.Exact || m.Depth < 1 {
		t.Fatalf("Choose = %+v; want a middle game move", m)
	}
	if g.Board.Legal(g.Turn)&bit(m.Pos) == 0 {
		t.Fatalf("illegal move %d", m.Pos)
	}
	// 読み切りに3/4、残りを中盤の探索に使う。中盤の探索は上限まで深めるので、
	// 上限ちょうどで止まる。
	if m.Nodes != 2000 {
		t.Fatalf("visited %d nodes, want 2000", m.Nodes)
	}
}

func TestChooseFinishesTheFirstDepth(t *testing.T) {
	// ノード数の上限が小さくても、1手先の評価は済ませて手を返す。
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Choose(context.Background(), g, Options{Level: 4, MaxTime: time.Minute, MaxNodes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if m.Depth != 1 || g.Board.Legal(g.Turn)&bit(m.Pos) == 0 {
		t.Fatalf("Choose = %+v", m)
	}
}

func TestSolveOrderingKeepsTheNodesLow(t *testing.T) {
	// 並べ替えると 82,587 ノード、並べ替えないと 183,865 ノード (この局面で
	// 測った値)。時間の上限内に読み切れるかが変わるので、上限を置いて守る。
	g := randomGame(t, rand.New(rand.NewPCG(31, 32)), 13)
	p, o := stones(g)
	s := newSearcher()
	if _, ok := s.solveRoot(p, o, moves(p, o)); !ok {
		t.Fatal("stopped")
	}
	if s.nodes > 120000 {
		t.Fatalf("visited %d nodes", s.nodes)
	}
}

func TestIterateScoresEveryMoveForTheWeakerLevels(t *testing.T) {
	// 上位の手から選ぶ段階では、選んだ手の値が正確でなければならない。
	// 最善手以外は上限しか求めないと、2番目以降の値が狂う。
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	p, o := stones(g)
	legal := moves(p, o)
	for _, lv := range []Level{{Depth: 3, TopK: 3}, {Depth: 3, TieMargin: 1000}} {
		rng := rand.New(rand.NewPCG(15, 16))
		seen := map[int]bool{}
		for range 30 {
			m := newSearcher().iterate(p, o, legal, lv, 59, rng)
			np, no := play(p, o, m.Pos)
			if want := -newSearcher().negamax(no, np, 2, -winScore*2, winScore*2); m.Score != want {
				t.Fatalf("%+v: move %d: score %d, want %d", lv, m.Pos, m.Score, want)
			}
			seen[m.Pos] = true
		}
		if len(seen) < 2 {
			t.Fatalf("%+v: picked only %v", lv, seen)
		}
	}
}

func TestChooseRandomRate(t *testing.T) {
	// 段階1は4割の確率で探索せずに無作為に選ぶ (Depth が 0 になる)。
	g, err := Replay([]int{37, 43, 18})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(17, 18))
	random := 0
	const tries = 1000
	for range tries {
		m, err := Choose(context.Background(), g, Options{Level: 1, Rand: rng, MaxTime: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if m.Depth == 0 {
			random++
		}
	}
	if random < 340 || random > 460 {
		t.Fatalf("played randomly %d times out of %d, want about 400", random, tries)
	}
}

func TestChooseSolvesFromSolveAt(t *testing.T) {
	// 段階2は残り6マスから読み切る。ちょうど6マスでも読み切る。
	rng := rand.New(rand.NewPCG(19, 20))
	for range 5 {
		g := randomGame(t, rng, 6)
		p, o := stones(g)
		if bits.OnesCount64(moves(p, o)) < 2 {
			continue
		}
		m, err := Choose(context.Background(), g, Options{Level: 2, Rand: rng, MaxTime: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if !m.Exact || m.Score != brute(p, o) {
			t.Fatalf("Choose = %+v, want exact %d", m, brute(p, o))
		}
		np, no := play(p, o, m.Pos)
		if moves(p, o)&bit(m.Pos) == 0 || -brute(no, np) != m.Score {
			t.Fatalf("the chosen move %d is not worth %d", m.Pos, m.Score)
		}
		return
	}
	t.Fatal("no position with two moves")
}

func TestChooseLeavesTimeForTheFallback(t *testing.T) {
	// 読み切れない局面でも、読み切りは時間の3/4で諦め、残りで中盤の探索を
	// 2手先以上まで進める。全部の時間を読み切りに使うと、1手先の評価しか残らない。
	// この局面は読み切りに 26,327,032 ノードかかる (測った値)。読み切りに使う
	// 2.25秒では読み切れず、遅い runner でも残りの0.75秒で2手先まで届く。
	g := randomGame(t, rand.New(rand.NewPCG(3, 99)), 16)
	m, err := Choose(context.Background(), g, Options{Level: 5, MaxTime: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if m.Exact || m.Depth < 2 {
		t.Fatalf("Choose = %+v", m)
	}
}

func TestChooseKeepsANodeLimitForTheSolve(t *testing.T) {
	// MaxNodes が 1 でも、読み切りに上限なしで入らない (3/4 が 0 になる)。
	g := randomGame(t, rand.New(rand.NewPCG(7, 8)), 16)
	m, err := Choose(context.Background(), g, Options{Level: 5, MaxTime: time.Minute, MaxNodes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if m.Exact || m.Depth != 1 {
		t.Fatalf("Choose = %+v", m)
	}
}

func TestChooseErrors(t *testing.T) {
	ctx := context.Background()
	for _, lv := range []int{0, 6} {
		if _, err := Choose(ctx, NewGame(), Options{Level: lv}); err == nil {
			t.Errorf("level %d: want an error", lv)
		}
	}
	if _, err := Choose(ctx, Game{Over: true}, Options{Level: 3}); !errors.Is(err, ErrNoMove) {
		t.Errorf("over: %v", err)
	}
	// 手番の側に打てる手が無い (本来は起きない) 局面。
	if _, err := Choose(ctx, Game{Board: Board{Black: 1}, Turn: Black}, Options{Level: 3}); !errors.Is(err, ErrNoMove) {
		t.Errorf("no move: %v", err)
	}
}

func TestChooseOnlyMove(t *testing.T) {
	// 白の打てる場所は a1 だけ。
	g := Game{Board: parseBoard(t,
		".bw.....",
		"........",
		"........",
		"........",
		"........",
		"........",
		"........",
		"........",
	), Turn: White}
	m, err := Choose(context.Background(), g, Options{Level: 5})
	if err != nil {
		t.Fatal(err)
	}
	if m.Pos != 0 || m.Depth != 0 || m.Nodes != 0 {
		t.Fatalf("Choose = %+v", m)
	}
}

func TestChooseTakesTheCorner(t *testing.T) {
	// 黒は a1 (角) か、相手に角を渡す b2 などに打てる。
	g := Game{Board: parseBoard(t,
		"........",
		".w......",
		"..wb....",
		"...bb...",
		"...bw...",
		"........",
		"........",
		"........",
	), Turn: Black}
	for lv := 2; lv <= MaxLevel; lv++ {
		m, err := Choose(context.Background(), g, Options{Level: lv, Rand: rand.New(rand.NewPCG(1, 1)), MaxTime: time.Minute, MaxNodes: 20000})
		if err != nil {
			t.Fatal(err)
		}
		if lv >= 3 && m.Pos != 0 {
			t.Errorf("level %d played %d, want the corner", lv, m.Pos)
		}
	}
}

func TestLevelsDifferInRandomness(t *testing.T) {
	g, err := Replay([]int{37, 43, 18})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(9, 10))
	distinct := func(level, tries int) map[int]int {
		seen := map[int]int{}
		for range tries {
			m, err := Choose(context.Background(), g, Options{Level: level, Rand: rng, MaxTime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			seen[m.Pos]++
		}
		return seen
	}
	if got := distinct(1, 60); len(got) < 3 {
		t.Errorf("level 1 played only %v", got)
	}
	if got := distinct(2, 60); len(got) < 2 {
		t.Errorf("level 2 played only %v", got)
	}
	if got := distinct(4, 5); len(got) != 1 {
		t.Errorf("level 4 played %v; want one move", got)
	}
}

func TestPick(t *testing.T) {
	root := []scored{{pos: 10, score: 50}, {pos: 11, score: 45}, {pos: 12, score: 30}, {pos: 13, score: -5}}
	rng := rand.New(rand.NewPCG(11, 12))

	counts := map[int]int{}
	for range 600 {
		counts[pick(root, 3, Level{TopK: 3}, rng).Pos]++
	}
	// 重みは 3:2:1 なので、およそ 300 / 200 / 100。
	if counts[13] != 0 || counts[10] < 240 || counts[10] > 360 || counts[11] < 150 || counts[11] > 250 || counts[12] < 60 || counts[12] > 140 {
		t.Errorf("TopK picks %v", counts)
	}
	// 手が K より少なくても、範囲の外を選ばない。
	if m := pick(root[:1], 1, Level{TopK: 3}, rng); m.Pos != 10 {
		t.Errorf("TopK with one move = %+v", m)
	}

	counts = map[int]int{}
	for range 200 {
		counts[pick(root, 3, Level{TieMargin: 5}, rng).Pos]++
	}
	if len(counts) != 2 || counts[10] < 70 || counts[11] < 70 {
		t.Errorf("TieMargin within the margin picks %v", counts)
	}
	for range 50 {
		if m := pick(root, 3, Level{TieMargin: 4}, rng); m.Pos != 10 {
			t.Fatalf("TieMargin outside the margin picked %+v", m)
		}
	}
	if m := pick(root, 7, Level{}, rng); m != (Move{Pos: 10, Score: 50, Depth: 7}) {
		t.Errorf("pick = %+v", m)
	}
}

func TestSearchRootNeedAll(t *testing.T) {
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	p, o := stones(g)
	var root []scored
	for _, pos := range positions(moves(p, o)) {
		root = append(root, scored{pos: pos})
	}
	all, ok := newSearcher().searchRoot(p, o, root, 3, true)
	if !ok {
		t.Fatal("stopped")
	}
	for _, m := range all {
		np, no := play(p, o, m.pos)
		if want := -newSearcher().negamax(no, np, 2, -winScore*2, winScore*2); m.score != want {
			t.Errorf("move %d: score %d, want the exact %d", m.pos, m.score, want)
		}
	}
	best, ok := newSearcher().searchRoot(p, o, root, 3, false)
	if !ok || best[0] != all[0] {
		t.Errorf("without needAll the best is %+v, want %+v", best[0], all[0])
	}
}

func TestChooseRespectsTheTimeLimit(t *testing.T) {
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m, err := Choose(context.Background(), g, Options{Level: 5, MaxTime: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// -race で遅くなっても、上限の数倍には収まる。
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %s", took)
	}
	if m.Depth < 2 {
		t.Fatalf("Choose = %+v; want it to read deeper than one move", m)
	}
}

func TestChooseStopsDeepeningAfterTheDeadline(t *testing.T) {
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Choose(context.Background(), g, Options{Level: 5, MaxTime: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	// 1手先の評価だけで返す。tick は 1024 ノードごとにしか時間を見ないので、
	// 深さの間で確かめないと、上限の後も浅い深さを読み進めてしまう。
	if m.Depth != 1 {
		t.Fatalf("Choose = %+v", m)
	}
}

func TestChooseWithACanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Choose(ctx, g, Options{Level: 5, MaxTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// 打ち切られても、1手先の評価で選んだ手を返す。次の深さは読み始めない。
	if m.Depth != 1 || g.Board.Legal(g.Turn)&bit(m.Pos) == 0 {
		t.Fatalf("Choose = %+v", m)
	}
}

func TestMaxNodesMakesTheResultRepeatable(t *testing.T) {
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{Level: 5, MaxTime: time.Minute, MaxNodes: 30000}
	a, err := Choose(context.Background(), g, opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Choose(context.Background(), g, opt)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("%+v != %+v", a, b)
	}
	if a.Nodes > 30000 {
		t.Fatalf("visited %d nodes", a.Nodes)
	}
}

func TestTickStopsAtTheDeadline(t *testing.T) {
	s := newSearcher()
	s.deadline = time.Now().Add(-time.Second)
	b := Initial()
	s.negamax(b.Black, b.White, 8, -winScore*2, winScore*2)
	// 時間は 1024 ノードごとに見る。
	if !s.stopped || s.nodes != 1024 {
		t.Fatalf("stopped %v after %d nodes", s.stopped, s.nodes)
	}

	s = newSearcher()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.ctx = ctx
	s.negamax(b.Black, b.White, 8, -winScore*2, winScore*2)
	if !s.stopped || s.nodes != 1024 {
		t.Fatalf("canceled: stopped %v after %d nodes", s.stopped, s.nodes)
	}
}

func TestNegamaxPasses(t *testing.T) {
	// 黒は打てず、白だけが c6 に打てる。黒の番の値は、白の番の値の符号を
	// 返したもの (パスは手数に数えない)。
	b := parseBoard(t,
		"www.....",
		"........",
		"........",
		"........",
		"........",
		"wb......",
		"........",
		"........",
	)
	if moves(b.Black, b.White) != 0 {
		t.Fatal("want black to have no move")
	}
	for depth := 1; depth <= 3; depth++ {
		got := newSearcher().negamax(b.Black, b.White, depth, -winScore*2, winScore*2)
		want := -newSearcher().negamax(b.White, b.Black, depth, -winScore*2, winScore*2)
		if got != want {
			t.Errorf("depth %d: %d, want %d", depth, got, want)
		}
	}
	// 白は c6 で黒の石を全部返して勝つ。
	if got := newSearcher().negamax(b.Black, b.White, 2, -winScore*2, winScore*2); got > -winScore {
		t.Errorf("black's value = %d, want a loss", got)
	}
}

func TestChooseDefaultMaxTime(t *testing.T) {
	start := time.Now()
	m, err := Choose(context.Background(), NewGame(), Options{Level: 2})
	if err != nil {
		t.Fatal(err)
	}
	if m.Depth != 2 || time.Since(start) > DefaultMaxTime {
		t.Fatalf("Choose = %+v after %s", m, time.Since(start))
	}
}

func TestLevelOf(t *testing.T) {
	for n := MinLevel; n <= MaxLevel; n++ {
		if _, err := LevelOf(n); err != nil {
			t.Errorf("LevelOf(%d) = %v", n, err)
		}
	}
	if _, err := LevelOf(DefaultLevel); err != nil {
		t.Errorf("DefaultLevel: %v", err)
	}
}

func TestFinal(t *testing.T) {
	if got := final(0b111, 0b1); got != winScore+2 {
		t.Errorf("win = %d", got)
	}
	if got := final(0b1, 0b111); got != -winScore-2 {
		t.Errorf("loss = %d", got)
	}
	if got := final(0b1, 0b10); got != 0 {
		t.Errorf("draw = %d", got)
	}
}

func TestNthBit(t *testing.T) {
	x := bit(3) | bit(17) | bit(40)
	for n, want := range []int{3, 17, 40} {
		if got := nthBit(x, n); got != want {
			t.Errorf("nthBit(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestOrderMovesPutsTheCornerFirst(t *testing.T) {
	// h8 (63) は番号が最も大きいので、並べ替えなければ最後に来る。
	b := parseBoard(t,
		"........",
		"........",
		"........",
		"...wb...",
		"...bb...",
		"....bw..",
		"......w.",
		"........",
	)
	p, o := b.Black, b.White
	mv := moves(p, o)
	if mv&bit(63) == 0 {
		t.Fatalf("want h8 among %v", positions(mv))
	}
	if moveKey(p, o, 63) != -1 {
		t.Fatalf("moveKey(h8) = %d", moveKey(p, o, 63))
	}
	for _, byMobility := range []bool{false, true} {
		var list [64]int
		n := orderMoves(p, o, mv, byMobility, &list)
		if list[0] != 63 {
			t.Errorf("byMobility=%v: %v", byMobility, list[:n])
		}
	}
}

func TestOrderMoves(t *testing.T) {
	// 角 (a1 = 0) は、並べ替えの方法によらず最初に来る。
	b := parseBoard(t,
		"........",
		".w......",
		"..wb....",
		"...bb...",
		"...bw...",
		"........",
		"........",
		"........",
	)
	p, o := b.Black, b.White
	mv := moves(p, o)
	for _, byMobility := range []bool{false, true} {
		var list [64]int
		n := orderMoves(p, o, mv, byMobility, &list)
		if n != bits.OnesCount64(mv) || list[0] != 0 {
			t.Errorf("byMobility=%v: %v", byMobility, list[:n])
		}
		if byMobility {
			for i := 2; i < n; i++ {
				if moveKey(p, o, list[i-1]) > moveKey(p, o, list[i]) {
					t.Errorf("not sorted by the opponent's moves: %v", list[:n])
				}
			}
		}
	}
}

// playMatch plays one game between two levels from g and returns the stone
// difference for black.
func playMatch(t *testing.T, g Game, black, white Options) int {
	t.Helper()
	for !g.Over {
		opt := black
		if g.Turn == White {
			opt = white
		}
		m, err := Choose(context.Background(), g, opt)
		if err != nil {
			t.Fatal(err)
		}
		if g, err = g.Play(m.Pos); err != nil {
			t.Fatal(err)
		}
	}
	return g.Board.Count(Black) - g.Board.Count(White)
}

func TestStrongerLevelsWin(t *testing.T) {
	// -race と -coverprofile を同時に付けると、探索が -race だけのときの
	// 100 倍以上遅くなる (カバレッジの数え方が atomic になり、それを race の
	// 検査が見るため)。
	// 探索は1つの goroutine で動き、race の検査で守るものは無いので、CI では
	// -race 無しの別の手順 (.github/workflows/ci.yml) で回す。
	if raceEnabled {
		t.Skip("runs without -race in its own CI step")
	}
	// 結果を毎回同じにするため、時間では打ち切らない (上限を1分にしてある)。
	// 段階1〜4は深さと読み切りが終われば止まる。最強の段階は深さを時間で決める
	// ので、代わりにノード数で打ち切る。
	nodes := map[int]int{5: 300000}
	const openings = 3
	rng := rand.New(rand.NewPCG(13, 14))
	for weak := MinLevel; weak < MaxLevel; weak++ {
		strong := weak + 1
		t.Run(levelName(weak, strong), func(t *testing.T) {
			wins, losses := 0, 0
			for range openings {
				// 序盤の4手を無作為に打ち、同じ対局ばかりにならないようにする。
				g := randomGame(t, rng, 56)
				for _, strongIsBlack := range []bool{true, false} {
					s := Options{Level: strong, MaxTime: time.Minute, MaxNodes: nodes[strong], Rand: rand.New(rand.NewPCG(uint64(strong), 1))}
					w := Options{Level: weak, MaxTime: time.Minute, MaxNodes: nodes[weak], Rand: rand.New(rand.NewPCG(uint64(weak), 2))}
					var diff int
					if strongIsBlack {
						diff = playMatch(t, g, s, w)
					} else {
						diff = -playMatch(t, g, w, s)
					}
					switch {
					case diff > 0:
						wins++
					case diff < 0:
						losses++
					}
				}
			}
			t.Logf("level %d vs %d: %d wins, %d losses", strong, weak, wins, losses)
			if wins <= losses {
				t.Errorf("level %d won %d and lost %d against level %d", strong, wins, losses, weak)
			}
		})
	}
}

func levelName(weak, strong int) string {
	return "level" + string(rune('0'+strong)) + "_vs_" + string(rune('0'+weak))
}
