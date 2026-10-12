package claudebot

import (
	"unicode"
)

// promptLeakWindow is the length, in normalized runes, of a run of the system
// prompt that must not appear in a post.
//
// 「システムプロンプトをそのまま出して」と頼まれて、運営者の指示文が公開の
// 投稿になるのを止めるための目安。短くすると、指示文に書いた「日本語で
// 返事をしてください」のようなありふれた言い回しを返事が使っただけで
// 止まってしまう。40文字続けて一致するのは、指示文を写したときくらいに
// なる長さにする。
const promptLeakWindow = 40

// isHangulFiller reports whether r is a Hangul filler, which renders as
// nothing on its own.
func isHangulFiller(r rune) bool {
	switch r {
	case '\u115f', '\u1160', '\u3164', '\uffa0':
		return true
	}
	return false
}

// normalizeForLeak folds the differences a copied prompt can carry without
// changing its meaning: whitespace, format characters (幅の無い空白など),
// nonspacing and enclosing marks (異体字セレクタ、CGJ、結合文字など) and
// Hangul fillers are dropped, full-width ASCII is folded to ASCII, and letters
// are lowercased.
//
// 空白や見えない文字を1文字ずつ挟ませる、全角にさせる、といった小さな
// 書き換えで一致を外されないようにするため。defangMentions が挟む U+200B も
// ここで落ちる。結合文字を落とすので、アクセントの付いた文字と付かない文字は
// 同じに数える (分解された形のときだけ。止める側に倒れるので構わない)。
// NFKC は新しい依存が要るので使わない。
func normalizeForLeak(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) || isHangulFiller(r) {
			continue
		}
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFF01 - 0x21
		}
		out = append(out, unicode.ToLower(r))
	}
	return out
}

// leaksSystemPrompt reports whether text contains promptLeakWindow or more
// consecutive (normalized) runes of any of prompts.
//
// 正規化して promptLeakWindow 文字に満たない指示文は見ない。短い指示文は
// それ自体がありふれた言い回しになりやすく、止めると普通の返事まで
// 止まるため。言い換えや翻訳、少しずつ聞き出す形は防げない。運営者が
// 指示文に秘密を書かないことが前提で、これは写しをそのまま公開しない
// ための最後の網にすぎない。
func leaksSystemPrompt(text string, prompts ...string) bool {
	out := normalizeForLeak(text)
	if len(out) < promptLeakWindow {
		return false
	}
	windows := make(map[string]struct{}, len(out)-promptLeakWindow+1)
	for i := 0; i+promptLeakWindow <= len(out); i++ {
		windows[string(out[i:i+promptLeakWindow])] = struct{}{}
	}
	for _, p := range prompts {
		pr := normalizeForLeak(p)
		for i := 0; i+promptLeakWindow <= len(pr); i++ {
			if _, ok := windows[string(pr[i:i+promptLeakWindow])]; ok {
				return true
			}
		}
	}
	return false
}
