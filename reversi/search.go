package reversi

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"slices"
	"time"
)

// Level is how strongly the engine plays.
type Level struct {
	// Depth is how many moves ahead the engine reads in the middle game.
	// 0 は時間の上限まで深める。
	Depth int
	// SolveAt is the number of empty squares at or below which the engine
	// reads to the end of the game. 0 は読み切らない。
	SolveAt int
	// RandomRate is the probability of playing a random legal move instead
	// of searching.
	RandomRate float64
	// TopK, when above 1, picks among the best TopK moves, the better ones
	// more often.
	TopK int
	// TieMargin, when above 0, picks either of the two best moves when
	// their scores differ by at most this much.
	TieMargin int
}

// The range of levels.
const (
	MinLevel     = 1
	MaxLevel     = 5
	DefaultLevel = 3
)

// levels are the strength settings, weakest first. 値は初期値で、段階どうしの
// 自己対戦で、上の段階が勝ち越すことを確かめている (search_test.go)。
var levels = [MaxLevel]Level{
	{Depth: 1, RandomRate: 0.4},
	{Depth: 2, SolveAt: 6, TopK: 3},
	{Depth: 4, SolveAt: 10, TieMargin: 10},
	{Depth: 6, SolveAt: 14},
	{Depth: 0, SolveAt: 16},
}

// LevelOf returns level n (MinLevel..MaxLevel).
func LevelOf(n int) (Level, error) {
	if n < MinLevel || n > MaxLevel {
		return Level{}, fmt.Errorf("reversi: level %d is out of %d..%d", n, MinLevel, MaxLevel)
	}
	return levels[n-1], nil
}

// DefaultMaxTime is the time limit for one move when Options.MaxTime is 0.
const DefaultMaxTime = 2 * time.Second

// Options control one search.
type Options struct {
	// Level is the strength (MinLevel..MaxLevel).
	Level int
	// MaxTime caps the time for one move. 0 なら DefaultMaxTime。上限に
	// 達したら、それまでに読み終えた深さの最善手を返す。
	MaxTime time.Duration
	// MaxNodes caps the positions visited. 0 は上限なし。時間と違い結果が
	// 変わらないので、テストで使う。
	MaxNodes int
	// Rand is the random source for the weaker levels. nil なら毎回違う種で作る。
	// *rand.Rand は複数の goroutine から同時に使えないので、並行して呼ぶ
	// Choose どうしで共有しないこと。
	Rand *rand.Rand
}

// Move is the move the engine chose.
type Move struct {
	// Pos is where to put the stone (y*8+x).
	Pos int
	// Score is the value of the move for the player. Exact なら最終的な石差。
	// そうでなければ評価値で、中盤の探索が終局まで読めた手は、勝ちなら
	// winScore (100000) + 石差、負けなら -winScore - (石差の絶対値)、引き分けなら
	// 0 になる。探索しなかった手 (Depth が 0) では意味を持たず、0 になる。
	Score int
	// Depth is how many moves ahead the engine read. 0 は探索しなかった
	// (打てる場所が1つ、または無作為に選んだ)。
	Depth int
	// Exact is true when the engine read to the end of the game.
	Exact bool
	// Nodes is the number of positions visited.
	Nodes int
}

// ErrNoMove means the game is over, so there is nothing to play.
var ErrNoMove = errors.New("reversi: no move to play")

// Choose picks the move for the color to play in g.
//
// ctx を打ち切ると、時間の上限と同じく、そこまでの最善手を返す。
// どの深さも読み終えられなくても、1手先の評価だけは必ず済ませる。
func Choose(ctx context.Context, g Game, opt Options) (Move, error) {
	lv, err := LevelOf(opt.Level)
	if err != nil {
		return Move{}, err
	}
	if g.Over {
		return Move{}, ErrNoMove
	}
	p, o := g.Board.Stones(g.Turn), g.Board.Stones(g.Turn.Opponent())
	legal := moves(p, o)
	if legal == 0 {
		return Move{}, ErrNoMove
	}
	if bits.OnesCount64(legal) == 1 {
		return Move{Pos: bits.TrailingZeros64(legal)}, nil
	}
	rng := opt.Rand
	if rng == nil {
		rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if lv.RandomRate > 0 && rng.Float64() < lv.RandomRate {
		return Move{Pos: nthBit(legal, rng.IntN(bits.OnesCount64(legal)))}, nil
	}

	maxTime := opt.MaxTime
	if maxTime <= 0 {
		maxTime = DefaultMaxTime
	}
	start := time.Now()
	s := &searcher{ctx: ctx}

	empties := bits.OnesCount64(^(p | o))
	if lv.SolveAt > 0 && empties <= lv.SolveAt && ctx.Err() == nil {
		// 読み切りに時間とノード数の3/4を使い、終わらなければ残りで中盤と同じ
		// 探索をする。
		s.deadline = start.Add(maxTime * 3 / 4)
		s.maxNodes = opt.MaxNodes * 3 / 4
		if opt.MaxNodes > 0 && s.maxNodes == 0 {
			// 0 は上限なしの意味になるので、1 ノードでも上限を残す。
			s.maxNodes = 1
		}
		if m, ok := s.solveRoot(p, o, legal); ok {
			m.Nodes = s.nodes
			return m, nil
		}
		s.stopped = false
	}
	s.deadline = start.Add(maxTime)
	s.maxNodes = opt.MaxNodes
	m := s.iterate(p, o, legal, lv, empties, rng)
	m.Nodes = s.nodes
	return m, nil
}

// nthBit returns the position of the n-th (0-based) set bit of x.
func nthBit(x uint64, n int) int {
	for range n {
		x &= x - 1
	}
	return bits.TrailingZeros64(x)
}

// searcher holds the limits and counters of one search.
type searcher struct {
	ctx      context.Context
	deadline time.Time
	maxNodes int
	nodes    int
	stopped  bool
	// bounded is false while the first iteration runs, which always
	// finishes so that there is a move to return.
	bounded bool
}

// tick counts a node and reports whether the search must stop.
func (s *searcher) tick() bool {
	s.nodes++
	if !s.bounded {
		return false
	}
	if s.maxNodes > 0 && s.nodes >= s.maxNodes {
		s.stopped = true
	} else if s.nodes&1023 == 0 && (time.Now().After(s.deadline) || s.ctx.Err() != nil) {
		s.stopped = true
	}
	return s.stopped
}

// expired reports whether the time ran out or ctx was done. ノード数の上限は
// tick が最初のノードで確かめるので、ここでは見ない。
func (s *searcher) expired() bool {
	return time.Now().After(s.deadline) || s.ctx.Err() != nil
}

// winScore puts any finished game above every evaluation, keeping the
// stone difference so that a bigger win is better.
const winScore = 100000

// final scores a finished game for p.
func final(p, o uint64) int {
	diff := bits.OnesCount64(p) - bits.OnesCount64(o)
	switch {
	case diff > 0:
		return winScore + diff
	case diff < 0:
		return -winScore + diff
	}
	return 0
}

type scored struct {
	pos   int
	score int
}

// iterate deepens the middle game search until the level's depth, the end of
// the game or the limits, and picks the move.
func (s *searcher) iterate(p, o, legal uint64, lv Level, empties int, rng *rand.Rand) Move {
	root := make([]scored, 0, bits.OnesCount64(legal))
	for x := legal; x != 0; x &= x - 1 {
		root = append(root, scored{pos: bits.TrailingZeros64(x)})
	}
	sortRoot(p, o, root)

	needAll := lv.TopK > 1 || lv.TieMargin > 0
	maxDepth := lv.Depth
	if maxDepth == 0 || maxDepth > empties {
		maxDepth = empties
	}
	var done []scored
	depth := 0
	for d := 1; d <= maxDepth; d++ {
		// 上限に達した後は、次の深さを読み始めない。tick は 1024 ノードごとに
		// しか時間を見ないので、浅い深さが上限の後も続いてしまう。
		if d > 1 && s.expired() {
			break
		}
		s.bounded = d > 1
		next, ok := s.searchRoot(p, o, root, d, needAll)
		if !ok {
			break
		}
		done, depth = next, d
		// 次の深さでは、今の深さで良かった手から調べる (αβの枝刈りが効く)。
		root = slices.Clone(next)
	}
	return pick(done, depth, lv, rng)
}

// sortRoot puts corners first, then the moves that leave the opponent the
// fewest replies.
func sortRoot(p, o uint64, root []scored) {
	for i := range root {
		root[i].score = -moveKey(p, o, root[i].pos)
	}
	slices.SortStableFunc(root, func(a, b scored) int { return b.score - a.score })
}

// searchRoot scores the root moves at depth d, best first. needAll のときは
// 全ての手の正確な値を求める (弱い段階が上位の手から選ぶため)。それ以外は
// 最善手だけが正確で、他は上限になる。
func (s *searcher) searchRoot(p, o uint64, root []scored, d int, needAll bool) ([]scored, bool) {
	out := make([]scored, len(root))
	alpha := -winScore * 2
	for i, m := range root {
		np, no := play(p, o, m.pos)
		lower := alpha
		if needAll {
			lower = -winScore * 2
		}
		v := -s.negamax(no, np, d-1, -winScore*2, -lower)
		if s.stopped {
			return nil, false
		}
		out[i] = scored{pos: m.pos, score: v}
		if v > alpha {
			alpha = v
		}
	}
	slices.SortStableFunc(out, func(a, b scored) int { return b.score - a.score })
	return out, true
}

// negamax returns the value of the position for p, searching depth moves.
func (s *searcher) negamax(p, o uint64, depth, alpha, beta int) int {
	if s.tick() {
		return 0
	}
	mv := moves(p, o)
	if mv == 0 {
		if moves(o, p) == 0 {
			return final(p, o)
		}
		// パスは手数に数えない。相手には打てる手があるので、無限には続かない。
		return -s.negamax(o, p, depth, -beta, -alpha)
	}
	if depth == 0 {
		return evaluate(p, o)
	}
	var list [64]int
	n := orderMoves(p, o, mv, depth >= 3, &list)
	best := -winScore * 2
	for _, pos := range list[:n] {
		np, no := play(p, o, pos)
		v := -s.negamax(no, np, depth-1, -beta, -alpha)
		if s.stopped {
			return 0
		}
		if v > best {
			best = v
		}
		if v > alpha {
			alpha = v
		}
		if alpha >= beta {
			break
		}
	}
	return best
}

// solveRoot reads to the end of the game and returns the best move, or false
// when the limits stopped it.
func (s *searcher) solveRoot(p, o, legal uint64) (Move, bool) {
	s.bounded = true
	var list [64]int
	n := orderMoves(p, o, legal, true, &list)
	best := Move{Pos: -1, Score: -65, Exact: true, Depth: bits.OnesCount64(^(p | o))}
	for _, pos := range list[:n] {
		np, no := play(p, o, pos)
		v := -s.solve(no, np, -64, -best.Score)
		if s.stopped {
			return Move{}, false
		}
		if v > best.Score {
			best.Pos, best.Score = pos, v
		}
	}
	return best, true
}

// solve returns the final stone difference for p with best play from both
// sides, within the window (alpha, beta).
func (s *searcher) solve(p, o uint64, alpha, beta int) int {
	if s.tick() {
		return 0
	}
	mv := moves(p, o)
	if mv == 0 {
		if moves(o, p) == 0 {
			return bits.OnesCount64(p) - bits.OnesCount64(o)
		}
		return -s.solve(o, p, -beta, -alpha)
	}
	var list [64]int
	// 残りが少ないと並べ替えの手間の方が大きい。
	n := orderMoves(p, o, mv, bits.OnesCount64(^(p|o)) > 6, &list)
	best := -65
	for _, pos := range list[:n] {
		np, no := play(p, o, pos)
		v := -s.solve(no, np, -beta, -alpha)
		if s.stopped {
			return 0
		}
		if v > best {
			best = v
		}
		if v > alpha {
			alpha = v
		}
		if alpha >= beta {
			break
		}
	}
	return best
}

const allCorners = uint64(1) | 1<<7 | 1<<56 | 1<<63

// moveKey is the ordering key of a move: lower is searched first. 角を
// 最初に、次に相手の打てる場所が少なくなる手 (相手の選択肢を狭める手は良い
// ことが多く、αβで早く枝が刈れる)。
func moveKey(p, o uint64, pos int) int {
	if bit(pos)&allCorners != 0 {
		return -1
	}
	np, no := play(p, o, pos)
	return bits.OnesCount64(moves(no, np))
}

// orderMoves writes the moves in mv into list, in search order, and returns
// how many there are. byMobility が false なら角を先にするだけにする。
func orderMoves(p, o, mv uint64, byMobility bool, list *[64]int) int {
	var keys [64]int
	n := 0
	for x := mv; x != 0; x &= x - 1 {
		pos := bits.TrailingZeros64(x)
		k := 0
		switch {
		case byMobility:
			k = moveKey(p, o, pos)
		case bit(pos)&allCorners != 0:
			k = -1
		}
		// 挿入ソート。手の数は多くて30程度。
		i := n
		for i > 0 && keys[i-1] > k {
			keys[i], list[i] = keys[i-1], list[i-1]
			i--
		}
		keys[i], list[i] = k, pos
		n++
	}
	return n
}

// pick chooses the move from the root scores, best first, the way the level
// says.
func pick(root []scored, depth int, lv Level, rng *rand.Rand) Move {
	choice := 0
	switch {
	case lv.TopK > 1:
		k := min(lv.TopK, len(root))
		// 上位ほど選ばれやすくする (k, k-1, ..., 1 の重み)。
		total := k * (k + 1) / 2
		r := rng.IntN(total)
		for i := range k {
			w := k - i
			if r < w {
				choice = i
				break
			}
			r -= w
		}
	case lv.TieMargin > 0 && len(root) >= 2 && root[0].score-root[1].score <= lv.TieMargin:
		choice = rng.IntN(2)
	}
	return Move{Pos: root[choice].pos, Score: root[choice].score, Depth: depth}
}
