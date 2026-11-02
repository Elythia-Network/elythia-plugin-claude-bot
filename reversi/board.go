// Package reversi is the reversi (Othello) engine that the bot plays with.
//
// It supports only the standard 8x8 board that the host starts games on.
// Positions are numbered y*8+x from the top left, the same as the host's
// game logs, and bit i of a bitboard is position i.
package reversi

import (
	"errors"
	"fmt"
	"math/bits"
)

// Color is the color of a stone.
type Color int

// The two colors. 黒が先手 (本体のエンジンと同じ)。
const (
	Black Color = iota
	White
)

// Opponent returns the other color.
func (c Color) Opponent() Color { return 1 - c }

func (c Color) String() string {
	if c == Black {
		return "black"
	}
	return "white"
}

// Board holds the stones of both colors.
type Board struct {
	Black uint64
	White uint64
}

// 列 x=0 と x=7。横や斜めにずらしたとき、反対の端へ回り込んだ石を消すのに使う。
const (
	colA uint64 = 0x0101010101010101
	colH uint64 = 0x8080808080808080
)

// Initial returns the standard starting position: d4 and e5 white, d5 and e4
// black (the host's default map "---wb---" / "---bw---").
func Initial() Board {
	return Board{
		Black: bit(4+3*8) | bit(3+4*8),
		White: bit(3+3*8) | bit(4+4*8),
	}
}

func bit(pos int) uint64 { return 1 << uint(pos) }

// Stones returns the stones of c.
func (b Board) Stones(c Color) uint64 {
	if c == Black {
		return b.Black
	}
	return b.White
}

// Count returns how many stones c has.
func (b Board) Count(c Color) int { return bits.OnesCount64(b.Stones(c)) }

// Empty returns the empty squares.
func (b Board) Empty() uint64 { return ^(b.Black | b.White) }

// Legal returns the squares where c can put a stone.
func (b Board) Legal(c Color) uint64 {
	return moves(b.Stones(c), b.Stones(c.Opponent()))
}

// Play puts a stone of c at pos and returns the new board.
func (b Board) Play(c Color, pos int) (Board, error) {
	if pos < 0 || pos >= 64 {
		return b, fmt.Errorf("%w: %d is off the board", ErrIllegalMove, pos)
	}
	if b.Legal(c)&bit(pos) == 0 {
		return b, fmt.Errorf("%w: %s cannot put at %d", ErrIllegalMove, c, pos)
	}
	p, o := play(b.Stones(c), b.Stones(c.Opponent()), pos)
	if c == Black {
		return Board{Black: p, White: o}, nil
	}
	return Board{Black: o, White: p}, nil
}

// ErrIllegalMove means a stone cannot be put there.
var ErrIllegalMove = errors.New("reversi: illegal move")

// shift moves every stone one square in direction d (0..7) and drops the ones
// that leave the board.
func shift(x uint64, d int) uint64 {
	switch d {
	case 0: // x+1
		return (x << 1) &^ colA
	case 1: // x-1
		return (x >> 1) &^ colH
	case 2: // y+1
		return x << 8
	case 3: // y-1
		return x >> 8
	case 4: // x+1, y+1
		return (x << 9) &^ colA
	case 5: // x-1, y+1
		return (x << 7) &^ colH
	case 6: // x+1, y-1
		return (x >> 7) &^ colA
	default: // x-1, y-1
		return (x >> 9) &^ colH
	}
}

// moves returns the squares where the player with stones p can put a stone
// against the opponent's stones o.
func moves(p, o uint64) uint64 {
	empty := ^(p | o)
	var m uint64
	for d := range 8 {
		// 相手の石は1方向に最大6つまで並ぶので、5回伸ばせば足りる。
		x := shift(p, d) & o
		for range 5 {
			x |= shift(x, d) & o
		}
		m |= shift(x, d) & empty
	}
	return m
}

// flips returns the opponent's stones that turn over when the player puts a
// stone at pos.
func flips(p, o uint64, pos int) uint64 {
	var f uint64
	for d := range 8 {
		var line uint64
		x := shift(bit(pos), d)
		for x&o != 0 {
			line |= x
			x = shift(x, d)
		}
		if x&p != 0 {
			f |= line
		}
	}
	return f
}

// play returns the player's and the opponent's stones after the player puts
// a stone at pos. pos は打てる場所であること。
func play(p, o uint64, pos int) (uint64, uint64) {
	f := flips(p, o, pos)
	return p | f | bit(pos), o &^ f
}

// Game is a board and whose turn it is.
type Game struct {
	Board Board
	// Turn is the color to play. Over のときは意味を持たない。
	Turn Color
	// Over is true when neither color can play.
	Over bool
}

// NewGame returns the game at the standard starting position.
func NewGame() Game { return Game{Board: Initial(), Turn: Black} }

// Play puts a stone for the color to play and passes the turn the way the
// host does: to the opponent if it can play, otherwise to the same color if
// it can, otherwise the game is over.
func (g Game) Play(pos int) (Game, error) {
	if g.Over {
		return g, fmt.Errorf("%w: the game is over", ErrIllegalMove)
	}
	b, err := g.Board.Play(g.Turn, pos)
	if err != nil {
		return g, err
	}
	next := Game{Board: b, Turn: g.Turn.Opponent()}
	switch {
	case b.Legal(next.Turn) != 0:
	case b.Legal(g.Turn) != 0:
		next.Turn = g.Turn
	default:
		next.Over = true
	}
	return next, nil
}

// Replay rebuilds a game from the positions in the host's game logs, in
// order. パスは記録に残らないので、手番は Play と同じ規則で決まる。
func Replay(positions []int) (Game, error) {
	g := NewGame()
	for i, pos := range positions {
		next, err := g.Play(pos)
		if err != nil {
			return g, fmt.Errorf("move %d: %w", i+1, err)
		}
		g = next
	}
	return g, nil
}

// Rules are the game settings that change how the game is played.
type Rules struct {
	// Map is the board, one string per row ("-" empty, "b" black, "w"
	// white, anything else not playable). 空なら本体は標準の盤を使う。
	Map              []string
	IsLlotheo        bool
	CanPutEverywhere bool
	LoopedBoard      bool
}

// ErrUnsupported means the engine cannot play under the rules.
var ErrUnsupported = errors.New("reversi: unsupported rules")

var standardMap = []string{
	"--------",
	"--------",
	"--------",
	"---wb---",
	"---bw---",
	"--------",
	"--------",
	"--------",
}

// Supports reports whether the engine can play under r: the standard 8x8
// board with no variant rules.
func Supports(r Rules) error {
	switch {
	case r.IsLlotheo:
		return fmt.Errorf("%w: isLlotheo", ErrUnsupported)
	case r.CanPutEverywhere:
		return fmt.Errorf("%w: canPutEverywhere", ErrUnsupported)
	case r.LoopedBoard:
		return fmt.Errorf("%w: loopedBoard", ErrUnsupported)
	}
	if len(r.Map) == 0 {
		return nil
	}
	if len(r.Map) != len(standardMap) {
		return fmt.Errorf("%w: the map is not the standard board", ErrUnsupported)
	}
	for i, row := range r.Map {
		if row != standardMap[i] {
			return fmt.Errorf("%w: the map is not the standard board", ErrUnsupported)
		}
	}
	return nil
}
