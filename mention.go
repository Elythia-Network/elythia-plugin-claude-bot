package claudebot

import (
	"regexp"
	"strings"
	"unicode"
)

// leadingMention matches one mention at the start of text, followed by
// whitespace, a separator (`:` `、` `,` and their full-width forms) or the end
// of text. Group 1 is the username, group 2 the host.
//
// 名前の文字は本体の MFM のパーサーと同じ (英数字と `_`、途中に `.` `-`)。
// 末尾の `.` `-` はパーサーがメンションに含めない (「@alice. こんにちは」は
// @alice と「.」) ので、続く空白の手前までまとめて外す。直後に名前以外の文字が
// 続く「@aliceさん」のような形は、外すと文が崩れるので対象にしない (それは
// defangMentions が無効にする)。「@alice: hi」「@alice、こんにちは」の区切りは
// 宛名の書き方なので、メンションと一緒に外す。半角の `:` `,` は後ろに空白か文末が
// 続くときだけ区切りとして扱う (「@alice:smile:」の絵文字コードや「@alice:30」を
// 壊さない)。ポート付きの書き方 (@alice@host:3000) は外す対象にしない (「:30 分後」の
// 数字と区別できない)。外れなかったものは defangMentions が無効にする。
var leadingMention = regexp.MustCompile(`^@([A-Za-z0-9_](?:[A-Za-z0-9_.-]*[A-Za-z0-9_])?)(?:@([A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?))?[.-]*(?:[:,](?:[\s\p{Zs}]+|$)|[：、，][\s\p{Zs}]*|[\s\p{Zs}]|$)`)

// stripLeadingMentions removes the run of mentions that starts text, as long
// as match accepts each of them (username and host, host empty when the
// mention has none).
//
// Claude は返事を「@相手 」から書き始めることがある (スレッドの中の自分の
// 過去の返事が、こちらで付けた宛先から始まっているのをまねる)。宛先は
// 投稿するときに付けるので、本文の頭の分は外す。外さずに defangMentions に
// 渡すと、リンクにならない「@​相手」が本物の宛先の後に並ぶ (#4)。
//
// 外すのは match が認めたものだけにする。「@bob さんにも聞いてみて」の @bob は
// 宛先ではなく文の一部なので、外すと意味が壊れる。残したものは
// defangMentions が無効にする。
func stripLeadingMentions(text string, match func(user, host string) bool) string {
	for {
		t := strings.TrimLeftFunc(text, unicode.IsSpace)
		m := leadingMention.FindStringSubmatchIndex(t)
		if m == nil {
			return t
		}
		user := t[m[2]:m[3]]
		host := ""
		if m[4] >= 0 {
			host = t[m[4]:m[5]]
		}
		if !match(user, host) {
			return t
		}
		text = t[m[1]:]
	}
}

// anyMention accepts every mention.
func anyMention(string, string) bool { return true }

// mentionOf accepts mentions of u: the same username (case-insensitive) with
// no host, or with u's host when u is remote.
func mentionOf(u userLite) func(user, host string) bool {
	return func(user, host string) bool {
		if !strings.EqualFold(user, u.Username) {
			return false
		}
		if host == "" {
			return true
		}
		return !u.isLocal() && strings.EqualFold(stripPort(host), stripPort(*u.Host))
	}
}

// stripPort removes ":port" from a host. 宛先の host がポート付きでも、Claude が
// ポートを省いて書いた宛名を宛先として扱う。
func stripPort(host string) string {
	// IPv6 のアドレス (`[::1]` や `::1`) は壊さない。ポートを外すのは `:` が 1 つの
	// ときと、`]` の後ろに `:` があるときだけ。
	if i := strings.LastIndexByte(host, ':'); i >= 0 && (strings.Count(host, ":") == 1 || (i > 0 && host[i-1] == ']')) {
		return host[:i]
	}
	return host
}
