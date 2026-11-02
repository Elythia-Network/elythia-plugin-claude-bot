package reversi

import "math/bits"

// squareWeights is the value of holding each square. 角が最も高く、角に
// 隣り合うマス (C と X) は、角を取られやすくするので低い。
var squareWeights = [64]int{
	100, -20, 10, 5, 5, 10, -20, 100,
	-20, -50, -2, -2, -2, -2, -50, -20,
	10, -2, 1, 1, 1, 1, -2, 10,
	5, -2, 1, 0, 0, 1, -2, 5,
	5, -2, 1, 0, 0, 1, -2, 5,
	10, -2, 1, 1, 1, 1, -2, 10,
	-20, -50, -2, -2, -2, -2, -50, -20,
	100, -20, 10, 5, 5, 10, -20, 100,
}

// corners and the squares next to each of them (C and X).
var cornerNeighbors = [4]struct {
	corner    int
	neighbors uint64
}{
	{0, bit(1) | bit(8) | bit(9)},
	{7, bit(6) | bit(15) | bit(14)},
	{56, bit(57) | bit(48) | bit(49)},
	{63, bit(62) | bit(55) | bit(54)},
}

const (
	mobilityWeight = 10
	stableWeight   = 25
)

// evaluate scores the position for the player with stones p. 大きいほど p に
// 良い。終局の局面には使わない (石差で数える)。
func evaluate(p, o uint64) int {
	score := positional(p, o) - positional(o, p)
	score += mobilityWeight * (bits.OnesCount64(moves(p, o)) - bits.OnesCount64(moves(o, p)))
	score += stableWeight * (bits.OnesCount64(edgeStable(p)) - bits.OnesCount64(edgeStable(o)))
	return score
}

// positional sums the square weights of p's stones.
func positional(p, o uint64) int {
	occupied := p | o
	// 角が埋まった後は、その隣のマスを持っていても角を取られる心配が無いので、
	// 低い重みを付けない。
	var neutral uint64
	for _, c := range cornerNeighbors {
		if occupied&bit(c.corner) != 0 {
			neutral |= c.neighbors
		}
	}
	score := 0
	for x := p &^ neutral; x != 0; x &= x - 1 {
		score += squareWeights[bits.TrailingZeros64(x)]
	}
	return score
}

// edgeStable approximates p's stable stones: the runs of p's stones that go
// along an edge from a corner p holds. どの向きからも返せないので、確定石に
// なる。辺の内側の確定石は数えない (近似)。
func edgeStable(p uint64) uint64 {
	var stable uint64
	for _, e := range edgeWalks {
		if p&bit(e.from) == 0 {
			continue
		}
		pos := e.from
		for range 8 {
			if p&bit(pos) == 0 {
				break
			}
			stable |= bit(pos)
			pos += e.step
		}
	}
	return stable
}

// edgeWalks are the eight walks from a corner along an edge.
var edgeWalks = [8]struct{ from, step int }{
	{0, 1}, {0, 8},
	{7, -1}, {7, 8},
	{56, 1}, {56, -8},
	{63, -1}, {63, -8},
}
