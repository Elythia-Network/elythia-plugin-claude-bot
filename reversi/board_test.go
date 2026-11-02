package reversi

import (
	"errors"
	"math/bits"
	"math/rand/v2"
	"strings"
	"testing"
)

// parseBoard reads 8 rows of "b", "w" and "." into a board.
func parseBoard(t *testing.T, rows ...string) Board {
	t.Helper()
	if len(rows) != 8 {
		t.Fatalf("want 8 rows, got %d", len(rows))
	}
	var b Board
	for y, row := range rows {
		row = strings.ReplaceAll(row, " ", "")
		if len(row) != 8 {
			t.Fatalf("row %d: want 8 squares, got %q", y, row)
		}
		for x, ch := range row {
			switch ch {
			case 'b':
				b.Black |= bit(x + y*8)
			case 'w':
				b.White |= bit(x + y*8)
			case '.':
			default:
				t.Fatalf("row %d: unknown square %q", y, ch)
			}
		}
	}
	return b
}

func positions(x uint64) []int {
	var out []int
	for ; x != 0; x &= x - 1 {
		out = append(out, bits.TrailingZeros64(x))
	}
	return out
}

// perft counts the positions after depth moves. パスも1手と数える (OEIS
// A124004 と同じ数え方)。
func perft(p, o uint64, depth int) int {
	if depth == 0 {
		return 1
	}
	mv := moves(p, o)
	if mv == 0 {
		if moves(o, p) == 0 {
			return 1
		}
		return perft(o, p, depth-1)
	}
	n := 0
	for x := mv; x != 0; x &= x - 1 {
		np, no := play(p, o, bits.TrailingZeros64(x))
		n += perft(no, np, depth-1)
	}
	return n
}

func TestPerft(t *testing.T) {
	// OEIS A124004 (the number of positions after n plies from the start).
	want := []int{1, 4, 12, 56, 244, 1396, 8200, 55092, 390216}
	b := Initial()
	for depth, w := range want {
		if got := perft(b.Black, b.White, depth); got != w {
			t.Errorf("perft(%d) = %d, want %d", depth, got, w)
		}
	}
}

func TestInitial(t *testing.T) {
	want := parseBoard(t,
		"........",
		"........",
		"........",
		"...wb...",
		"...bw...",
		"........",
		"........",
		"........",
	)
	if got := Initial(); got != want {
		t.Fatalf("Initial() = %+v, want %+v", got, want)
	}
	// 黒の打てる場所は d3, c4, f5, e6 (pos 19, 26, 37, 44)。
	if got := positions(Initial().Legal(Black)); !equalInts(got, []int{19, 26, 37, 44}) {
		t.Fatalf("legal moves for black = %v", got)
	}
	if got := Initial().Count(Black) + Initial().Count(White); got != 4 {
		t.Fatalf("stones = %d", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlayFlipsEveryDirection(t *testing.T) {
	// d4 (pos 27) に黒を置くと、8方向すべての白が返る。
	b := parseBoard(t,
		"b..b..b.",
		".w.w.w..",
		"..www...",
		"bww.wwwb",
		"..www...",
		".w.w.w..",
		"b..b..b.",
		"........",
	)
	got, err := b.Play(Black, 27)
	if err != nil {
		t.Fatal(err)
	}
	if got.White != 0 {
		t.Fatalf("white stones left: %v", positions(got.White))
	}
	if got.Black != b.Black|b.White|bit(27) {
		t.Fatalf("black stones = %v", positions(got.Black))
	}
}

func TestPlayDoesNotWrapAroundTheEdges(t *testing.T) {
	// a2 (pos 8) の白は、h1 (pos 7) の黒と行をまたいで並ぶが、挟んでいない。
	// 回り込みを消し忘れると、b2 (pos 9) に置けることになる。
	cases := []struct {
		name string
		rows []string
		pos  int
	}{
		{"east", []string{
			".......b",
			"w.......",
			"........", "........", "........", "........", "........", "........",
		}, 9},
		{"west", []string{
			"........",
			".......w",
			"b.......",
			"........", "........", "........", "........", "........",
		}, 14},
		{"diagonal", []string{
			"........",
			"w.......",
			"......b.",
			"........", "........", "........", "........", "........",
		}, 15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := parseBoard(t, tc.rows...)
			for _, c := range []Color{Black, White} {
				if legal := b.Legal(c); legal != 0 {
					t.Fatalf("%s has moves %v", c, positions(legal))
				}
			}
			if _, err := b.Play(Black, tc.pos); !errors.Is(err, ErrIllegalMove) {
				t.Fatalf("Play = %v, want ErrIllegalMove", err)
			}
		})
	}
}

func TestPlayFlipsTheLongestLine(t *testing.T) {
	// 相手の石が6つ並ぶ線 (盤の端から端まで) も返せる。
	b := parseBoard(t,
		"bwwwwww.",
		"........",
		"........",
		"........",
		"........",
		"........",
		"........",
		"........",
	)
	got, err := b.Play(Black, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.White != 0 || got.Count(Black) != 8 {
		t.Fatalf("after h1: black %v, white %v", positions(got.Black), positions(got.White))
	}
}

func TestShiftDropsStonesLeavingTheBoard(t *testing.T) {
	all := ^uint64(0)
	want := []int{64 - 8, 64 - 8, 56, 56, 49, 49, 49, 49}
	for d, w := range want {
		if got := bits.OnesCount64(shift(all, d)); got != w {
			t.Errorf("direction %d keeps %d squares, want %d", d, got, w)
		}
	}
}

func TestPlayRejects(t *testing.T) {
	b := Initial()
	for _, pos := range []int{-1, 64, 0, 27} {
		if _, err := b.Play(Black, pos); !errors.Is(err, ErrIllegalMove) {
			t.Errorf("Play(%d) = %v, want ErrIllegalMove", pos, err)
		}
	}
}

func TestGamePassesAndEnds(t *testing.T) {
	// 白が a1 に置くと、黒は打てないのでパスになり、白が続けて打つ。
	b := parseBoard(t,
		".bw.....",
		"........",
		"........",
		"........",
		"........",
		"wb......",
		"........",
		"........",
	)
	g := Game{Board: b, Turn: White}
	next, err := g.Play(0)
	if err != nil {
		t.Fatal(err)
	}
	if next.Over || next.Turn != White {
		t.Fatalf("after a1: turn %s, over %v; want white to play again", next.Turn, next.Over)
	}
	// 白が c6 に置くと黒の石が無くなり、どちらも打てないので終局。
	last, err := next.Play(42)
	if err != nil {
		t.Fatal(err)
	}
	if !last.Over {
		t.Fatalf("want the game over, turn %s", last.Turn)
	}
	if _, err := last.Play(1); !errors.Is(err, ErrIllegalMove) {
		t.Fatalf("Play after the end = %v", err)
	}
}

func TestGameTurnPassesToTheOpponent(t *testing.T) {
	g, err := NewGame().Play(19)
	if err != nil {
		t.Fatal(err)
	}
	if g.Turn != White || g.Over {
		t.Fatalf("turn %s, over %v", g.Turn, g.Over)
	}
}

func TestReplay(t *testing.T) {
	// f5 d6 c3 d3 c4 (虎定石の出だし)。
	g, err := Replay([]int{37, 43, 18, 19, 26})
	if err != nil {
		t.Fatal(err)
	}
	if g.Turn != White {
		t.Fatalf("turn = %s", g.Turn)
	}
	if g.Board.Count(Black)+g.Board.Count(White) != 9 {
		t.Fatalf("stones = %d", g.Board.Count(Black)+g.Board.Count(White))
	}

	if _, err := Replay([]int{37, 0}); !errors.Is(err, ErrIllegalMove) || !strings.Contains(err.Error(), "move 2") {
		t.Fatalf("Replay with a bad move = %v", err)
	}
}

func TestReplayMatchesTheHost(t *testing.T) {
	// 本体のエンジン (internal/core/reversi/game.go) で無作為に打った対局。
	// 白が続けて打つパスが7回あり、47手目で黒の石が無くなって終わる。
	// turns は各手の後に本体が決めた手番 (B / W、E は終局)。
	logs := []int{26, 18, 10, 43, 44, 25, 17, 11, 51, 16, 8, 34, 33, 42, 4, 32, 50, 0, 19, 52, 40, 53, 41, 3, 9, 48, 60, 61, 1, 49, 57, 2, 20, 24, 59, 5, 54, 13, 62, 12, 56, 21, 58, 29, 37, 63, 55}
	turns := "WBWBWBWBWBWBWBWBWBWBWBWBWBWBWBWBWBWBWBWWWWWWWWE"
	for i := range logs {
		g, err := Replay(logs[:i+1])
		if err != nil {
			t.Fatal(err)
		}
		got := "E"
		if !g.Over {
			got = map[Color]string{Black: "B", White: "W"}[g.Turn]
		}
		if want := turns[i : i+1]; got != want {
			t.Fatalf("after move %d: turn %s, the host says %s", i+1, got, want)
		}
	}
	g, err := Replay(logs)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Board{Black: 0, White: 0xffff1f3f3f3f3f3f}); g.Board != want {
		t.Fatalf("final board = %#x / %#x", g.Board.Black, g.Board.White)
	}
}

func TestReplayFollowsPasses(t *testing.T) {
	// 無作為な対局を最後まで打ち、置いた位置だけから、1手ずつ打った結果と同じ
	// 盤と手番を作れることを確かめる (本体との一致は TestReplayMatchesTheHost)。
	rng := rand.New(rand.NewPCG(1, 2))
	for range 50 {
		g := NewGame()
		var logs []int
		for !g.Over {
			legal := positions(g.Board.Legal(g.Turn))
			pos := legal[rng.IntN(len(legal))]
			next, err := g.Play(pos)
			if err != nil {
				t.Fatal(err)
			}
			g = next
			logs = append(logs, pos)
		}
		got, err := Replay(logs)
		if err != nil {
			t.Fatal(err)
		}
		if got != g {
			t.Fatalf("Replay = %+v, want %+v", got, g)
		}
	}
}

func TestSupports(t *testing.T) {
	ok := []Rules{
		{},
		{Map: append([]string(nil), standardMap...)},
	}
	for _, r := range ok {
		if err := Supports(r); err != nil {
			t.Errorf("Supports(%+v) = %v", r, err)
		}
	}
	bad := []Rules{
		{IsLlotheo: true},
		{CanPutEverywhere: true},
		{LoopedBoard: true},
		{Map: standardMap[:7]},
		{Map: []string{"--------", "--------", "--------", "---bw---", "---wb---", "--------", "--------", "--------"}},
	}
	for _, r := range bad {
		if err := Supports(r); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Supports(%+v) = %v, want ErrUnsupported", r, err)
		}
	}
}

func TestColor(t *testing.T) {
	if Black.Opponent() != White || White.Opponent() != Black {
		t.Fatal("Opponent")
	}
	if Black.String() != "black" || White.String() != "white" {
		t.Fatal("String")
	}
}
