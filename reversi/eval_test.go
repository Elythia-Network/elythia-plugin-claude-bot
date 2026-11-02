package reversi

import "testing"

func TestEdgeStable(t *testing.T) {
	b := parseBoard(t,
		"bbbw....",
		"b.......",
		"b.......",
		"........",
		"........",
		"........",
		".......b",
		"...bbbb.",
	)
	// a1 から右へ3つ、下へ2つ (a1 は1回だけ数える)。h8 は持っていないので数えない。
	want := bit(0) | bit(1) | bit(2) | bit(8) | bit(16)
	if got := edgeStable(b.Black); got != want {
		t.Fatalf("edgeStable = %v, want %v", positions(got), positions(want))
	}
	// 途切れた先の石は数えない。
	gap := parseBoard(t,
		"bb.b....",
		"........",
		"........",
		"........",
		"........",
		"........",
		"........",
		"........",
	)
	if got := edgeStable(gap.Black); got != bit(0)|bit(1) {
		t.Fatalf("edgeStable with a gap = %v", positions(got))
	}
	// 辺を埋め尽くしても、盤の外へ出ない。
	full := parseBoard(t,
		"bbbbbbbb",
		"b......b",
		"b......b",
		"b......b",
		"b......b",
		"b......b",
		"b......b",
		"bbbbbbbb",
	)
	if got := edgeStable(full.Black); got != full.Black {
		t.Fatalf("edgeStable of the full edges = %v", positions(got))
	}
}

func TestPositionalNeutralizesNeighborsOfTakenCorners(t *testing.T) {
	// b2 (X) は角が空いていれば -50、角が埋まれば数えない。
	x := bit(9)
	if got := positional(x, 0); got != -50 {
		t.Fatalf("X square with the corner empty = %d", got)
	}
	if got := positional(x, bit(0)); got != 0 {
		t.Fatalf("X square with the corner taken = %d", got)
	}
	for _, c := range cornerNeighbors {
		if got := positional(c.neighbors, bit(c.corner)); got != 0 {
			t.Errorf("corner %d: neighbors still count %d", c.corner, got)
		}
		if got := positional(bit(c.corner), 0); got != 100 {
			t.Errorf("corner %d = %d", c.corner, got)
		}
	}
}

func TestEvaluate(t *testing.T) {
	b := Initial()
	if got := evaluate(b.Black, b.White); got != 0 {
		t.Fatalf("the start is symmetric, got %d", got)
	}
	// 角を持つ側が良い。
	withCorner := parseBoard(t,
		"b.......",
		"........",
		"........",
		"...wb...",
		"...bw...",
		"........",
		"........",
		"........",
	)
	if v := evaluate(withCorner.Black, withCorner.White); v <= 0 {
		t.Fatalf("holding a corner = %d", v)
	}
	if v := evaluate(withCorner.White, withCorner.Black); v >= 0 {
		t.Fatalf("for the other side = %d", v)
	}
	// 打てる場所の差も数える。f5 d6 c3 d3 c4 の後は、黒と白で打てる場所の数が違う。
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	p, o := g.Board.White, g.Board.Black
	mob := bitsCount(moves(p, o)) - bitsCount(moves(o, p))
	if mob == 0 {
		t.Fatal("want a position where the mobility differs")
	}
	rest := positional(p, o) - positional(o, p) + stableWeight*(bitsCount(edgeStable(p))-bitsCount(edgeStable(o)))
	got := evaluate(p, o) - rest
	if got != mobilityWeight*mob || got == 0 || (got > 0) != (mob > 0) {
		t.Fatalf("mobility part = %d, want %d with the sign of %d", got, mobilityWeight*mob, mob)
	}
}

func TestEvaluateCountsStableStones(t *testing.T) {
	// 黒だけが角から辺に3つ並べている。
	b := parseBoard(t,
		"bbb.....",
		"........",
		"........",
		"...wb...",
		"...bw...",
		"........",
		"........",
		"........",
	)
	p, o := b.Black, b.White
	stable := bitsCount(edgeStable(p)) - bitsCount(edgeStable(o))
	if stable != 3 {
		t.Fatalf("stable difference = %d", stable)
	}
	rest := positional(p, o) - positional(o, p) + mobilityWeight*(bitsCount(moves(p, o))-bitsCount(moves(o, p)))
	// 確定石が多い側に有利な値でなければならない (重みが0なら効いていない)。
	if got := evaluate(p, o) - rest; got != stableWeight*stable || got <= 0 {
		t.Fatalf("stable part = %d, want %d (> 0)", got, stableWeight*stable)
	}
}

func bitsCount(x uint64) int { return len(positions(x)) }
