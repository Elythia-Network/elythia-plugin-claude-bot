package claudebot

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripLeadingMentions_Any(t *testing.T) {
	cases := []struct{ in, want string }{
		{"@alice こんにちは", "こんにちは"},
		{"@alice@remote.example こんにちは", "こんにちは"},
		{"@alice@remote.example:8080 こんにちは", "@alice@remote.example:8080 こんにちは"}, // ポート付きは外さない
		{"@alice @bob@remote.example\nこんにちは", "こんにちは"},
		{"@alice　こんにちは", "こんにちは"}, // 全角の空白
		{"  @alice  こんにちは", "こんにちは"},
		{"@alice. こんにちは", "こんにちは"}, // 末尾の . はメンションに含まれない
		{"@alice", ""},
		{"@a.b_c-d こんにちは", "こんにちは"},
		{"@alice: hi", "hi"},
		{"@alice、こんにちは", "こんにちは"},
		{"@alice，こんにちは", "こんにちは"},
		{"@alice：こんにちは", "こんにちは"},
		{"@alice, hi", "hi"},
		{"@alice:", ""},
		// 区切りの後に空白が無い半角の : , は文の一部として残す。
		{"@alice:smile: よろしく", "@alice:smile: よろしく"},
		{"@alice:30 です", "@alice:30 です"},
		{"@alice,bob と話した", "@alice,bob と話した"},
		{"@alice@remote.example:30 分後に", "@alice@remote.example:30 分後に"},
		// 外さないもの。
		{"こんにちは @alice", "こんにちは @alice"},
		{"@aliceさん、こんにちは", "@aliceさん、こんにちは"}, // 名前に文字が続く
		{"@ こんにちは", "@ こんにちは"},
		{"@.alice こんにちは", "@.alice こんにちは"},
		{"user@example.com です", "user@example.com です"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, stripLeadingMentions(c.in, anyMention), c.in)
	}
}

// 返事の本文から外すのは宛先へのメンションだけ (#4)。他の人へのメンションは
// 文の一部なので残し、defangMentions に任せる。
func TestStripPort(t *testing.T) {
	for in, want := range map[string]string{
		"remote.example:3000": "remote.example",
		"remote.example":      "remote.example",
		"[::1]:3000":          "[::1]",
		"[::1]":               "[::1]",
		"::1":                 "::1",
	} {
		assert.Equal(t, want, stripPort(in), in)
	}
}

func TestStripLeadingMentions_OnlyTheRecipient(t *testing.T) {
	host := "remote.example"
	remote := mentionOf(userLite{Username: "Alice", Host: &host})
	local := mentionOf(userLite{Username: "alice"})
	portHost := "remote.example:3000"
	withPort := mentionOf(userLite{Username: "alice", Host: &portHost})
	cases := []struct {
		in    string
		match func(string, string) bool
		want  string
	}{
		{"@alice こんにちは", remote, "こんにちは"},
		{"@ALICE@Remote.Example こんにちは", remote, "こんにちは"},
		{"@alice@other.example こんにちは", remote, "@alice@other.example こんにちは"},
		{"@bob さんにも聞いてみて", remote, "@bob さんにも聞いてみて"},
		{"@alice @bob と話した", remote, "@bob と話した"},
		{"@alice こんにちは", local, "こんにちは"},
		{"@alice@remote.example こんにちは", local, "@alice@remote.example こんにちは"},
		{"@alice@remote.example こんにちは", withPort, "こんにちは"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, stripLeadingMentions(c.in, c.match), c.in)
	}
}

// 返事の頭のメンションは 1 つだけになる (#4)。Claude が本文を「@相手 」から
// 書き始めても、無効にした「@​相手」が本物の宛先の後に並ばない。
func TestReply_LeadingMentionFromClaudeIsNotDoubled(t *testing.T) {
	e := newEnv(t)
	e.claude.push(message("@alice@remote.example こんにちは", "end_turn", 1, 1))
	e.api.notes["n1"] = note("n1", "alice", "alice", "remote.example", "hi", "public")
	require.NoError(t, e.mention("x", "n1"))
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 1)
	text := creates[0].Params["text"].(string)
	assert.Equal(t, "@alice@remote.example こんにちは", text)
	assert.NotContains(t, text, "​")
}

// 宛先でない人へのメンションが本文の頭にあっても、外さずに無効にする (#4)。
func TestReply_LeadingMentionOfSomeoneElseIsDefanged(t *testing.T) {
	e := newEnv(t)
	e.claude.push(message("@victim@remote.example さんにも聞いてみて", "end_turn", 1, 1))
	e.api.notes["n1"] = note("n1", "alice", "alice", "remote.example", "hi", "public")
	require.NoError(t, e.mention("x", "n1"))
	text := e.api.callsTo("notes/create")[0].Params["text"].(string)
	assert.Equal(t, "@alice@remote.example @​victim@remote.example さんにも聞いてみて", text)
}

// 宛先の後に続くコードブロックの中のメンションも無効にし、コードブロックは
// 行頭から始まるよう宛先の後で改行する (#4)。パーサーはコードブロックを行頭で
// しか読まないので、宛先と同じ行に ``` が続くと中の @ が本物のメンションになる。
func TestReply_CodeBlockAfterRecipientIsSafe(t *testing.T) {
	e := newEnv(t)
	e.claude.push(message("@alice\n```\n@victim@evil.example\n```", "end_turn", 1, 1))
	e.api.notes["n1"] = note("n1", "alice", "alice", "remote.example", "hi", "public")
	require.NoError(t, e.mention("x", "n1"))
	text := e.api.callsTo("notes/create")[0].Params["text"].(string)
	assert.Equal(t, "@alice@remote.example\n```\n@​victim@evil.example\n```", text)
}

// プロンプトに渡す自分の過去の返事からは、こちらで付けた宛先を外す (#4)。
// 残すと Claude がその形をまねて、本文にも宛先を書く。
func TestReplyPrompt_StripsTheBotsOwnLeadingMention(t *testing.T) {
	str := func(s string) *string { return &s }
	host := "remote.example"
	alice := userLite{ID: "alice", Username: "alice", Host: &host}
	bot := userLite{ID: "bot", Username: "bot"}
	thread := []noteView{
		{ID: "n1", Text: str("hi"), User: alice},
		{ID: "n2", Text: str("@alice@remote.example こんにちは"), User: bot},
	}
	last := noteView{ID: "n3", Text: str("@bot 元気？"), User: alice}

	p := replyPrompt(thread, last, "bot", 10)

	assert.Contains(t, p, "\nこんにちは\n", "自分の返事の本文は宛先を外して渡す")
	assert.NotContains(t, p, "@alice@remote.example こんにちは")
	assert.Contains(t, p, "@bot 元気？", "相手の投稿は書かれたまま渡す")
	assert.True(t, strings.Contains(p, "宛先のメンション"), "宛先を書かないよう頼む")
}

// 保存した下書きも、投稿する前に無効にし直す (#4)。古い版が保存した下書きは、
// 今の規則 (URL やコードの中の @ も崩す) で崩されていないことがある。
func TestReply_RetriedDraftIsDefangedAgain(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = note("n1", "alice", "alice", "remote.example", "hi", "public")
	e.api.createFails = []int{502}
	e.claude.push(message("ok", "end_turn", 1, 1))
	require.Error(t, e.mention("x", "n1"))

	// 古い版が保存した、崩していない下書きに差し替える。
	res, err := e.db.Exec(`UPDATE handled_notifications SET reply_text = $1 WHERE reply_text IS NOT NULL`, "https://a.example/*@bob")
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "下書きが保存されていない")

	require.NoError(t, e.mention("x", "n1"))
	creates := e.api.callsTo("notes/create")
	require.Len(t, creates, 2)
	assert.Equal(t, "@alice@remote.example https://a.example/*@​bob", creates[1].Params["text"])
	assert.Len(t, e.claude.calls(), 1, "下書きを使い、Claude は呼び直さない")
}
