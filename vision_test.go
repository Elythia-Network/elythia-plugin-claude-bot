package claudebot

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// テストの画像。中身は http.DetectContentType が型を決められる先頭だけでよい
// (プラグインはデコードしない)。
var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-png-test")
	jpegBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00-jpeg-test")
	webpBytes = []byte("RIFF\x10\x00\x00\x00WEBPVP8 -webp-test")
)

const driveHost = "https://drive.example/files/"

// attachment builds a packed DriveFile.
func attachment(id, typ string, opts ...func(map[string]any)) map[string]any {
	f := map[string]any{
		"id":           id,
		"name":         id,
		"type":         typ,
		"url":          driveHost + id,
		"thumbnailUrl": driveHost + "thumb-" + id,
		"size":         1000,
		"isSensitive":  false,
		"comment":      nil,
		"properties":   map[string]any{"width": 640, "height": 480},
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func withFiles(n map[string]any, files ...map[string]any) map[string]any {
	ids := make([]string, 0, len(files))
	for _, f := range files {
		ids = append(ids, f["id"].(string))
	}
	n["files"] = files
	n["fileIds"] = ids
	return n
}

func visionOn(mutate func(*VisionSettings)) func(*Settings) {
	return func(s *Settings) {
		s.Vision.Enabled = true
		if mutate != nil {
			mutate(&s.Vision)
		}
	}
}

// contentOf returns the content blocks of the single user message.
func contentOf(t *testing.T, r capturedRequest) []map[string]any {
	t.Helper()
	msgs, _ := r.Body["messages"].([]any)
	require.Len(t, msgs, 1)
	m, _ := msgs[0].(map[string]any)
	assert.Equal(t, "user", m["role"])
	raw, _ := m["content"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		cm, _ := c.(map[string]any)
		out = append(out, cm)
	}
	return out
}

// sentBlock is one image block with the label text block before it.
type sentBlock struct {
	label     string
	mediaType string
	data      []byte
}

// imagesOf returns the image blocks of the request in order.
func imagesOf(t *testing.T, r capturedRequest) []sentBlock {
	t.Helper()
	blocks := contentOf(t, r)
	var out []sentBlock
	for i, b := range blocks {
		if b["type"] != "image" {
			continue
		}
		src, _ := b["source"].(map[string]any)
		assert.Equal(t, "base64", src["type"])
		data, err := base64.StdEncoding.DecodeString(src["data"].(string))
		require.NoError(t, err)
		label := ""
		if i > 0 && blocks[i-1]["type"] == "text" {
			label, _ = blocks[i-1]["text"].(string)
		}
		mt, _ := src["media_type"].(string)
		out = append(out, sentBlock{label: label, mediaType: mt, data: data})
	}
	return out
}

func TestVision_OffSendsTextOnly(t *testing.T) {
	e := newEnv(t)
	e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png"))
	e.claude.serveFile(driveHost+"f1", pngBytes)
	e.claude.push(message("いいね", "end_turn", 1, 1))

	require.NoError(t, e.mention("notif-1", "n1"))

	calls := e.claude.calls()
	require.Len(t, calls, 1)
	blocks := contentOf(t, calls[0])
	require.Len(t, blocks, 1, "OFFでは従来どおりtextのブロック1つだけ")
	assert.Equal(t, "text", blocks[0]["type"])
	p := promptOf(calls[0])
	assert.Contains(t, p, "見て\n(添付ファイル1件)\n</post>")
	assert.NotContains(t, p, "画像")
	assert.Empty(t, e.claude.fetchedURLs(), "OFFでは画像を取りに行かない")
	assert.Len(t, e.api.callsTo("notes/create"), 1)
}

func TestVision_SendsMentionedNoteImages(t *testing.T) {
	e := newEnv(t)
	e.settings(visionOn(nil))
	alt := "猫が <b>寝ている</b>\n写真"
	e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "これ何?", "public"),
		attachment("f1", "image/png"),
		attachment("f2", "image/jpeg", func(f map[string]any) { f["comment"] = alt }),
	)
	e.claude.serveFile(driveHost+"f1", pngBytes)
	e.claude.serveFile(driveHost+"f2", jpegBytes)
	e.claude.push(message("猫ですね", "end_turn", 1, 1))

	require.NoError(t, e.mention("notif-1", "n1"))

	calls := e.claude.calls()
	require.Len(t, calls, 1)
	blocks := contentOf(t, calls[0])
	require.Len(t, blocks, 5, "ラベル+画像を2組、その後にスレッドの文")
	assert.Equal(t, map[string]any{"type": "text", "text": "画像1"}, blocks[0])
	assert.Equal(t, "image", blocks[1]["type"])
	assert.Equal(t, map[string]any{"type": "text", "text": "画像2"}, blocks[2])
	assert.Equal(t, "image", blocks[3]["type"])
	assert.Equal(t, "text", blocks[4]["type"])
	imgs := imagesOf(t, calls[0])
	require.Len(t, imgs, 2)
	assert.Equal(t, sentBlock{label: "画像1", mediaType: "image/png", data: pngBytes}, imgs[0])
	assert.Equal(t, sentBlock{label: "画像2", mediaType: "image/jpeg", data: jpegBytes}, imgs[1])

	p, _ := blocks[4]["text"].(string)
	assert.Contains(t, p, "「画像N」のラベル")
	assert.Contains(t, p, "これ何?\n[画像1]\n[画像2: 猫が &lt;b&gt;寝ている&lt;/b&gt; 写真]\n</post>",
		"代替テキストはプロンプトのescapeを通し、改行を詰める")
	assert.NotContains(t, p, "添付ファイル")
	assert.NotContains(t, p, "送っていない添付")
	assert.Equal(t, []string{driveHost + "f1", driveHost + "f2"}, e.claude.fetchedURLs())
}

// threadEnv builds a reply (n3, alice) under a public note of bob (n2, with
// an image) and a followers-only note of carol (n1, with an image).
func threadEnv(t *testing.T, mutate func(*VisionSettings)) *env {
	t.Helper()
	e := newEnv(t)
	e.settings(visionOn(mutate))
	n := withFiles(note("n3", "alice", "alice", "", "どう?", "public"), attachment("f3", "image/png"))
	n["replyId"] = "n2"
	e.api.notes["n3"] = n
	e.api.conversation["n3"] = []map[string]any{
		withFiles(note("n2", "bob", "bob", "", "ボブの写真", "public"), attachment("f2", "image/jpeg")),
		// aliceには読めない投稿。botは読めるが、文脈にも画像にも入れない。
		withFiles(note("n1", "carol", "carol", "", "鍵の写真", "followers"), attachment("f1", "image/png")),
	}
	e.claude.serveFile(driveHost+"f1", pngBytes)
	e.claude.serveFile(driveHost+"f2", jpegBytes)
	e.claude.serveFile(driveHost+"f3", pngBytes)
	e.claude.push(message("見ました", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-1", "n3"))
	return e
}

func TestVision_ThreadImages(t *testing.T) {
	t.Run("include thread", func(t *testing.T) {
		e := threadEnv(t, func(v *VisionSettings) { v.IncludeThread = true })
		calls := e.claude.calls()
		require.Len(t, calls, 1)
		imgs := imagesOf(t, calls[0])
		require.Len(t, imgs, 2)
		assert.Equal(t, sentBlock{label: "画像1", mediaType: "image/png", data: pngBytes}, imgs[0], "メンションされた投稿が先")
		assert.Equal(t, sentBlock{label: "画像2", mediaType: "image/jpeg", data: jpegBytes}, imgs[1])
		assert.NotContains(t, e.claude.fetchedURLs(), driveHost+"f1", "話しかけた人が読めない投稿の画像は取りに行かない")
		p := promptOf(calls[0])
		assert.Contains(t, p, "ボブの写真\n[画像2]\n</post>")
		assert.Contains(t, p, "どう?\n[画像1]\n</post>")
		assert.NotContains(t, p, "鍵の写真")
	})
	t.Run("mentioned note only", func(t *testing.T) {
		e := threadEnv(t, nil)
		calls := e.claude.calls()
		require.Len(t, calls, 1)
		imgs := imagesOf(t, calls[0])
		require.Len(t, imgs, 1)
		assert.Equal(t, "画像1", imgs[0].label)
		assert.Equal(t, []string{driveHost + "f3"}, e.claude.fetchedURLs())
		p := promptOf(calls[0])
		assert.Contains(t, p, "ボブの写真\n(添付ファイル1件)\n</post>", "見ていない祖先の添付は従来どおり件数だけ")
		assert.Contains(t, p, "どう?\n[画像1]\n</post>")
	})
	t.Run("newer ancestors first", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(func(v *VisionSettings) { v.IncludeThread = true; v.MaxImages = 1 }))
		n := note("n3", "alice", "alice", "", "どう?", "public")
		n["replyId"] = "n2"
		e.api.notes["n3"] = n
		e.api.conversation["n3"] = []map[string]any{
			withFiles(note("n2", "bob", "bob", "", "近い", "public"), attachment("f2", "image/png")),
			withFiles(note("n1", "bob", "bob", "", "遠い", "public"), attachment("f1", "image/png")),
		}
		e.claude.serveFile(driveHost+"f1", pngBytes)
		e.claude.serveFile(driveHost+"f2", pngBytes)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n3"))
		assert.Equal(t, []string{driveHost + "f2"}, e.claude.fetchedURLs(), "上限に掛かるときは新しい投稿の画像を残す")
		p := promptOf(e.claude.calls()[0])
		assert.Contains(t, p, "遠い\n(送っていない添付: 上限を超えた分 1件)\n</post>")
		assert.Contains(t, p, "近い\n[画像1]\n</post>")
	})
	t.Run("only notes within contextNotes", func(t *testing.T) {
		e := newEnv(t)
		e.settings(func(s *Settings) {
			visionOn(func(v *VisionSettings) { v.IncludeThread = true })(s)
			s.Reply.ContextNotes = 2 // 返事をする投稿と、祖先1件
		})
		n := note("n3", "alice", "alice", "", "どう?", "public")
		n["replyId"] = "n2"
		e.api.notes["n3"] = n
		e.api.conversation["n3"] = []map[string]any{
			withFiles(note("n2", "bob", "bob", "", "近い", "public"), attachment("f2", "image/png")),
			withFiles(note("n1", "bob", "bob", "", "遠い", "public"), attachment("f1", "image/png")),
		}
		e.claude.serveFile(driveHost+"f1", pngBytes)
		e.claude.serveFile(driveHost+"f2", pngBytes)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n3"))
		assert.Equal(t, []string{driveHost + "f2"}, e.claude.fetchedURLs(), "プロンプトに入らない投稿の画像は送らない")
	})
}

func TestVision_SensitiveAndLimit(t *testing.T) {
	sensitive := func(f map[string]any) { f["isSensitive"] = true }
	t.Run("sensitive is not sent by default", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/png", sensitive), attachment("f2", "image/png"))
		e.claude.serveFile(driveHost+"f1", pngBytes)
		e.claude.serveFile(driveHost+"f2", pngBytes)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Equal(t, []string{driveHost + "f2"}, e.claude.fetchedURLs())
		assert.Contains(t, promptOf(e.claude.calls()[0]), "見て\n[画像1]\n(送っていない添付: センシティブ 1件)\n</post>")
	})
	t.Run("sensitive is sent when allowed", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(func(v *VisionSettings) { v.IncludeSensitive = true }))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png", sensitive))
		e.claude.serveFile(driveHost+"f1", pngBytes)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Len(t, imagesOf(t, e.claude.calls()[0]), 1)
	})
	t.Run("max images", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(func(v *VisionSettings) { v.MaxImages = 2 }))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/png"), attachment("f2", "image/png"), attachment("f3", "image/png"))
		for _, id := range []string{"f1", "f2", "f3"} {
			e.claude.serveFile(driveHost+id, pngBytes)
		}
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Len(t, imagesOf(t, e.claude.calls()[0]), 2)
		assert.Equal(t, []string{driveHost + "f1", driveHost + "f2"}, e.claude.fetchedURLs())
		assert.Contains(t, promptOf(e.claude.calls()[0]), "[画像1]\n[画像2]\n(送っていない添付: 上限を超えた分 1件)")
	})
}

func TestVision_ThumbnailFallback(t *testing.T) {
	cases := []struct {
		name string
		file map[string]any
	}{
		{"video", attachment("f1", "video/mp4")},
		{"avif", attachment("f1", "image/avif")},
		{"too large", attachment("f1", "image/png", func(f map[string]any) { f["size"] = maxImageBytes + 1 })},
		{"too wide", attachment("f1", "image/png", func(f map[string]any) {
			f["properties"] = map[string]any{"width": maxImageDimension + 1, "height": 100}
		})},
		{"too tall", attachment("f1", "image/png", func(f map[string]any) {
			f["properties"] = map[string]any{"width": 100, "height": maxImageDimension + 1}
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings(visionOn(nil))
			e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), tc.file)
			e.claude.serveFile(driveHost+"f1", pngBytes)
			e.claude.serveFile(driveHost+"thumb-f1", webpBytes)
			e.claude.push(message("はい", "end_turn", 1, 1))
			require.NoError(t, e.mention("notif-1", "n1"))
			assert.Equal(t, []string{driveHost + "thumb-f1"}, e.claude.fetchedURLs())
			imgs := imagesOf(t, e.claude.calls()[0])
			require.Len(t, imgs, 1)
			assert.Equal(t, "image/webp", imgs[0].mediaType, "型は取った中身から決める")
		})
	}

	t.Run("exact limits use the original", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/png", func(f map[string]any) {
				f["size"] = maxImageBytes
				f["properties"] = map[string]any{"width": maxImageDimension, "height": maxImageDimension}
			}),
			// 寸法の無い画像はそのまま送る。
			attachment("f2", "image/gif", func(f map[string]any) { f["properties"] = map[string]any{} }))
		e.claude.serveFile(driveHost+"f1", pngBytes)
		e.claude.serveFile(driveHost+"f2", []byte("GIF89a-gif-test"))
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Equal(t, []string{driveHost + "f1", driveHost + "f2"}, e.claude.fetchedURLs())
		imgs := imagesOf(t, e.claude.calls()[0])
		require.Len(t, imgs, 2)
		assert.Equal(t, "image/gif", imgs[1].mediaType)
	})

	t.Run("no thumbnail", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		noThumb := func(f map[string]any) { f["thumbnailUrl"] = nil }
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "audio/mpeg", noThumb),
			attachment("f2", "application/zip", noThumb),
			attachment("f3", "image/avif", noThumb))
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Empty(t, e.claude.fetchedURLs())
		calls := e.claude.calls()
		require.Len(t, calls, 1)
		assert.Len(t, contentOf(t, calls[0]), 1, "送る画像が無ければtextだけ")
		p := promptOf(calls[0])
		assert.Contains(t, p, "見て\n(送っていない添付: 画像でないもの 2件、送れる形の無い画像 1件)\n</post>")
		assert.NotContains(t, p, "「画像N」のラベル", "画像が無ければ説明も付けない")
	})
}

func TestVision_FetchFailuresAreSkipped(t *testing.T) {
	cases := []struct {
		name  string
		serve func(*fakeClaude)
		log   string
	}{
		{"not found", func(*fakeClaude) {}, "HTTP 404"},
		{"server error", func(f *fakeClaude) { f.serveFileStatus(driveHost+"f1", 500, pngBytes) }, "HTTP 500"},
		{"html declared as png", func(f *fakeClaude) {
			f.serveFile(driveHost+"f1", []byte("<!doctype html><html><body>hi</body></html>"))
		}, "text/html"},
		{"svg declared as png", func(f *fakeClaude) {
			f.serveFile(driveHost+"f1", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
		}, "どれでもない"},
		{"empty", func(f *fakeClaude) { f.serveFile(driveHost+"f1", nil) }, "どれでもない"},
		{"body over the limit", func(f *fakeClaude) {
			f.serveFile(driveHost+"f1", append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0}, maxImageBytes)...))
		}, "大きすぎます"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings(visionOn(nil))
			e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
				attachment("f1", "image/png"), attachment("f2", "image/png"))
			tc.serve(e.claude)
			e.claude.serveFile(driveHost+"f2", pngBytes)
			e.claude.push(message("はい", "end_turn", 1, 1))

			require.NoError(t, e.mention("notif-1", "n1"))

			calls := e.claude.calls()
			require.Len(t, calls, 1, "取れない画像があっても返事は続ける")
			imgs := imagesOf(t, calls[0])
			require.Len(t, imgs, 1)
			assert.Equal(t, sentBlock{label: "画像1", mediaType: "image/png", data: pngBytes}, imgs[0])
			assert.Contains(t, promptOf(calls[0]), "見て\n[画像1]\n(送っていない添付: 取得できなかったもの 1件)\n</post>")
			assert.Len(t, e.api.callsTo("notes/create"), 1)
			evs := e.events(eventImage)
			require.Len(t, evs, 1)
			assert.Contains(t, evs[0].Message, "f1")
			assert.Contains(t, evs[0].Message, tc.log)
			assert.Equal(t, "n1", evs[0].NoteID)
		})
	}

	t.Run("request errors", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/png", func(f map[string]any) { f["url"] = "::not a url" }),
			attachment("f2", "image/png"))
		e.claude.serveFileStatus(driveHost+"f2", 0, nil)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Contains(t, promptOf(e.claude.calls()[0]), "見て\n(送っていない添付: 取得できなかったもの 2件)\n</post>")
		assert.Len(t, e.events(eventImage), 2)
	})
	t.Run("exactly at the limit is kept", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png"))
		body := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0}, maxImageBytes-len(pngBytes))...)
		e.claude.serveFile(driveHost+"f1", body)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		imgs := imagesOf(t, e.claude.calls()[0])
		require.Len(t, imgs, 1)
		assert.Len(t, imgs[0].data, maxImageBytes)
	})

	t.Run("failed fetches use up the limit", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(func(v *VisionSettings) { v.MaxImages = 1 }))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/png"), attachment("f2", "image/png"))
		e.claude.serveFile(driveHost+"f2", pngBytes)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Equal(t, []string{driveHost + "f1"}, e.claude.fetchedURLs())
		assert.Contains(t, promptOf(e.claude.calls()[0]), "(送っていない添付: 取得できなかったもの 1件、上限を超えた分 1件)")
	})
}

func TestVision_SameImagesOnMaxTokensRetry(t *testing.T) {
	e := newEnv(t)
	e.settings(visionOn(nil))
	e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png"))
	e.claude.serveFile(driveHost+"f1", pngBytes)
	e.claude.push(message("途中", "max_tokens", 1, 1), message("短く", "end_turn", 1, 1))

	require.NoError(t, e.mention("notif-1", "n1"))

	calls := e.claude.calls()
	require.Len(t, calls, 2)
	assert.Equal(t, imagesOf(t, calls[0]), imagesOf(t, calls[1]))
	assert.Len(t, imagesOf(t, calls[1]), 1)
	assert.Len(t, e.claude.fetchedURLs(), 1, "呼び直しでは取り直さない")
}

func TestVision_DraftRetryDoesNotFetch(t *testing.T) {
	e := newEnv(t)
	e.settings(visionOn(nil))
	e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png"))
	e.claude.serveFile(driveHost+"f1", pngBytes)
	e.api.createFails = []int{502}
	e.claude.push(message("見ました", "end_turn", 1, 1))

	require.Error(t, e.mention("n-id", "n1"))
	require.Len(t, e.claude.fetchedURLs(), 1)
	require.NoError(t, e.mention("n-id", "n1"))

	assert.Len(t, e.claude.calls(), 1)
	assert.Len(t, e.claude.fetchedURLs(), 1, "下書きで投稿し直すときは画像を取りに行かない")
	assert.Len(t, e.api.callsTo("notes/create"), 2)
}

func TestVision_NotFetchedOverLimit(t *testing.T) {
	e := newEnv(t)
	e.settings(func(s *Settings) {
		visionOn(nil)(s)
		s.Limits.PerUserPerHour = 1
	})
	e.addUsage("alice", now(), "claude-opus-5-5", tokens{Input: 1})
	e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png"))
	e.claude.serveFile(driveHost+"f1", pngBytes)
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Empty(t, e.claude.calls())
	assert.Empty(t, e.claude.fetchedURLs(), "費用の上限で返事をしないときは画像も取らない")
}

func TestVision_ImageSizeLimitFitsAPI(t *testing.T) {
	// base64にしても、APIの上限の5MBに収まる。
	assert.LessOrEqual(t, base64.StdEncoding.EncodedLen(maxImageBytes), 5_000_000)
	assert.Greater(t, base64.StdEncoding.EncodedLen(maxImageBytes+3), 5_000_000, "上限を必要以上に小さくしない")
}

func TestVisionSettings(t *testing.T) {
	d := defaultSettings().Vision
	assert.Equal(t, VisionSettings{MaxImages: 4}, d, "既定はOFF・4枚・スレッドとセンシティブは送らない")

	for _, n := range []int{1, maxVisionImages} {
		s := defaultSettings()
		s.Vision.MaxImages = n
		assert.NoError(t, s.check(3000), n)
	}
	for _, n := range []int{0, -1, maxVisionImages + 1} {
		s := defaultSettings()
		s.Vision.MaxImages = n
		assert.Error(t, s.check(3000), n)
	}
	assert.Equal(t, 20, maxVisionImages)

	t.Run("stored settings without vision", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.db.Exec(`INSERT INTO settings (id, data) VALUES (1, '{"model":"claude-opus-5-5","reply":{"mode":"reply"}}')`)
		require.NoError(t, err)
		s, err := loadSettings(t.Context(), e.db)
		require.NoError(t, err)
		assert.Equal(t, VisionSettings{MaxImages: 4}, s.Vision)
	})
	t.Run("stored vision is read", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.db.Exec(`INSERT INTO settings (id, data) VALUES (1, '{"vision":{"enabled":true,"maxImages":7,"includeThread":true,"includeSensitive":true}}')`)
		require.NoError(t, err)
		s, err := loadSettings(t.Context(), e.db)
		require.NoError(t, err)
		assert.Equal(t, VisionSettings{Enabled: true, MaxImages: 7, IncludeThread: true, IncludeSensitive: true}, s.Vision)
	})
}

func TestVision_DuplicateFileIsSentOnce(t *testing.T) {
	e := newEnv(t)
	e.settings(visionOn(func(v *VisionSettings) { v.IncludeThread = true }))
	n := withFiles(note("n2", "alice", "alice", "", "同じ画像", "public"), attachment("f1", "image/png"))
	n["replyId"] = "n1"
	e.api.notes["n2"] = n
	e.api.conversation["n2"] = []map[string]any{
		withFiles(note("n1", "alice", "alice", "", "最初", "public"), attachment("f1", "image/png")),
	}
	e.claude.serveFile(driveHost+"f1", pngBytes)
	e.claude.push(message("はい", "end_turn", 1, 1))
	require.NoError(t, e.mention("notif-1", "n2"))
	assert.Len(t, e.claude.fetchedURLs(), 1)
	p := promptOf(e.claude.calls()[0])
	assert.Equal(t, 2, strings.Count(p, "[画像1]"))
}
