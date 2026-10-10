package claudebot

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// テストの画像。プラグインは形式と寸法をヘッダーから読むので、本物の画像を使う。
var (
	pngBytes  = encodePNG(4, 3)
	jpegBytes = func() []byte {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 3)), nil); err != nil {
			panic(err)
		}
		return buf.Bytes()
	}()
	gifBytes = func() []byte {
		var buf bytes.Buffer
		img := image.NewPaletted(image.Rect(0, 0, 4, 3), color.Palette{color.Black, color.White})
		if err := gif.Encode(&buf, img, nil); err != nil {
			panic(err)
		}
		return buf.Bytes()
	}()
	// 1x1 の lossless WebP。
	webpBytes, _ = base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
)

func encodePNG(w, h int) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// padded returns a valid PNG header followed by zeros, n bytes in total.
// DecodeConfig はヘッダーだけを読むので、大きさの境界を試すのに使える。
func padded(n int) []byte {
	b := encodePNG(4, 3)
	return append(b, make([]byte, n-len(b))...)
}

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

// setCollectTimeout shortens collectTimeout for the test.
func setCollectTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := collectTimeout
	collectTimeout = d
	t.Cleanup(func() { collectTimeout = prev })
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

func TestVision_TestImagesDecode(t *testing.T) {
	for name, b := range map[string][]byte{"png": pngBytes, "jpeg": jpegBytes, "gif": gifBytes, "webp": webpBytes} {
		_, format, err := image.DecodeConfig(bytes.NewReader(b))
		require.NoError(t, err, name)
		assert.Equal(t, name, format)
	}
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
	alt := "猫が <b>寝ている</b>\n写真 ] [画像9"
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
	assert.Contains(t, p, "これ何?\n[画像1]\n[画像2: 猫が &lt;b&gt;寝ている&lt;/b&gt; 写真 ］ ［画像9]\n</post>",
		"代替テキストはescapeを通し、改行を詰め、角括弧で参照を偽れないよう全角にする")
	assert.NotContains(t, p, "添付ファイル")
	assert.NotContains(t, p, "送っていない添付")
	assert.ElementsMatch(t, []string{driveHost + "f1", driveHost + "f2"}, e.claude.fetchedURLs())
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
		assert.ElementsMatch(t, []string{driveHost + "f1", driveHost + "f2"}, e.claude.fetchedURLs())
		assert.Contains(t, promptOf(e.claude.calls()[0]), "[画像1]\n[画像2]\n(送っていない添付: 上限を超えた分 1件)")
	})
	t.Run("total size of one request", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(func(v *VisionSettings) { v.MaxImages = 6 }))
		var files []map[string]any
		for i := 1; i <= 6; i++ {
			id := "f" + string(rune('0'+i))
			files = append(files, attachment(id, "image/png"))
			// base64 にすると1枚ちょうど 5,000,000 バイト。4枚で上限の 20MB に達する。
			e.claude.serveFile(driveHost+id, padded(maxImageBytes))
		}
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), files...)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		imgs := imagesOf(t, e.claude.calls()[0])
		assert.Len(t, imgs, 4, "base64にした合計がリクエストの上限に収まる分だけ送る")
		assert.Contains(t, promptOf(e.claude.calls()[0]), "[画像4]\n(送っていない添付: 上限を超えた分 2件)")
	})
}

func TestVision_ThumbnailFallback(t *testing.T) {
	cases := []struct {
		name     string
		file     map[string]any
		original []byte // nil なら元の画像を取りに行かない
		variant  string
	}{
		{"video", attachment("f1", "video/mp4"), nil, "動画のサムネイル"},
		{"avif", attachment("f1", "image/avif"), nil, "縮小版"},
		{"original over the size limit", attachment("f1", "image/png"), padded(maxImageBytes + 1), "縮小版"},
		{"original too wide", attachment("f1", "image/png"), encodePNG(maxImageDimension+1, 1), "縮小版"},
		{"original too tall", attachment("f1", "image/png"), encodePNG(1, maxImageDimension+1), "縮小版"},
		{"broken file with a gif header", attachment("f1", "image/gif"), []byte("GIF89a<html>broken</html>"), "縮小版"},
		{"html declared as png", attachment("f1", "image/png"), []byte("<!doctype html><html></html>"), "縮小版"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings(visionOn(nil))
			e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), tc.file)
			if tc.original != nil {
				e.claude.serveFile(driveHost+"f1", tc.original)
			}
			e.claude.serveFile(driveHost+"thumb-f1", webpBytes)
			e.claude.push(message("はい", "end_turn", 1, 1))
			require.NoError(t, e.mention("notif-1", "n1"))
			want := []string{driveHost + "thumb-f1"}
			if tc.original != nil {
				want = []string{driveHost + "f1", driveHost + "thumb-f1"}
			}
			assert.Equal(t, want, e.claude.fetchedURLs(), "サムネイルは1回だけ試す")
			imgs := imagesOf(t, e.claude.calls()[0])
			require.Len(t, imgs, 1)
			assert.Equal(t, "image/webp", imgs[0].mediaType, "型は取った中身から決める")
			assert.Contains(t, promptOf(e.claude.calls()[0]), "見て\n[画像1("+tc.variant+")]\n</post>")
			assert.Empty(t, e.events(eventImage), "サムネイルを送れたら記録しない")
		})
	}

	t.Run("declared size and dimensions are not trusted", func(t *testing.T) {
		// 他の人向けの url はwebpublicを指し、size・propertiesは原本の値になる。
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/jpeg", func(f map[string]any) {
				f["size"] = 9_000_000
				f["properties"] = map[string]any{"width": 12000, "height": 9000}
			}))
		e.claude.serveFile(driveHost+"f1", jpegBytes)
		e.claude.serveFile(driveHost+"thumb-f1", webpBytes)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Equal(t, []string{driveHost + "f1"}, e.claude.fetchedURLs())
		imgs := imagesOf(t, e.claude.calls()[0])
		require.Len(t, imgs, 1)
		assert.Equal(t, jpegBytes, imgs[0].data)
		assert.Contains(t, promptOf(e.claude.calls()[0]), "見て\n[画像1]\n</post>")
	})

	t.Run("exact limits use the original", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/png"), attachment("f2", "image/png"), attachment("f3", "image/gif"))
		e.claude.serveFile(driveHost+"f1", padded(maxImageBytes))
		e.claude.serveFile(driveHost+"f2", encodePNG(maxImageDimension, 1))
		e.claude.serveFile(driveHost+"f3", gifBytes)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.ElementsMatch(t, []string{driveHost + "f1", driveHost + "f2", driveHost + "f3"}, e.claude.fetchedURLs())
		imgs := imagesOf(t, e.claude.calls()[0])
		require.Len(t, imgs, 3)
		assert.Len(t, imgs[0].data, maxImageBytes)
		assert.Equal(t, "image/gif", imgs[2].mediaType)
	})

	t.Run("exact height limit", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png"))
		e.claude.serveFile(driveHost+"f1", encodePNG(1, maxImageDimension))
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Equal(t, []string{driveHost + "f1"}, e.claude.fetchedURLs())
		assert.Len(t, imagesOf(t, e.claude.calls()[0]), 1)
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

	t.Run("unsuitable original without thumbnail", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/png", func(f map[string]any) { f["thumbnailUrl"] = nil }))
		e.claude.serveFile(driveHost+"f1", encodePNG(maxImageDimension+1, 1))
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Equal(t, []string{driveHost + "f1"}, e.claude.fetchedURLs())
		assert.Contains(t, promptOf(e.claude.calls()[0]), "見て\n(送っていない添付: 取得できなかったもの 1件)\n</post>")
		evs := e.events(eventImage)
		require.Len(t, evs, 1)
		assert.Contains(t, evs[0].Message, "寸法")
	})

	t.Run("thumbnail is checked too", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png"))
		e.claude.serveFile(driveHost+"f1", padded(maxImageBytes+1))
		e.claude.serveFile(driveHost+"thumb-f1", []byte("GIF89a"))
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Equal(t, []string{driveHost + "f1", driveHost + "thumb-f1"}, e.claude.fetchedURLs())
		assert.Contains(t, promptOf(e.claude.calls()[0]), "見て\n(送っていない添付: 取得できなかったもの 1件)\n</post>")
		evs := e.events(eventImage)
		require.Len(t, evs, 1)
		assert.Contains(t, evs[0].Message, "元の画像: 大きすぎます")
		assert.Contains(t, evs[0].Message, "サムネイル: 画像として読めません")
	})

	t.Run("fetch errors do not fall back", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"), attachment("f1", "image/png"))
		e.claude.serveFile(driveHost+"thumb-f1", webpBytes)
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Equal(t, []string{driveHost + "f1"}, e.claude.fetchedURLs(), "404 ではサムネイルを試さない")
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
		{"body over the limit", func(f *fakeClaude) { f.serveFile(driveHost+"f1", padded(maxImageBytes+1)) }, "大きすぎます"},
		{"broken header", func(f *fakeClaude) { f.serveFile(driveHost+"f1", []byte("\x89PNG\r\n\x1a\nbroken")) }, "画像として読めません"},
		{"connection error", func(f *fakeClaude) { f.serveFileStatus(driveHost+"f1", 0, nil) }, "通信に失敗しました: connection refused"},
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
			assert.Contains(t, evs[0].Message, "ファイルf1")
			assert.Contains(t, evs[0].Message, tc.log)
			assert.NotContains(t, evs[0].Message, "drive.example", "記録にURLを残さない")
			assert.Equal(t, "n1", evs[0].NoteID)
		})
	}

	t.Run("bad url", func(t *testing.T) {
		e := newEnv(t)
		e.settings(visionOn(nil))
		e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
			attachment("f1", "image/png", func(f map[string]any) { f["url"] = "::not a url" }))
		e.claude.push(message("はい", "end_turn", 1, 1))
		require.NoError(t, e.mention("notif-1", "n1"))
		assert.Contains(t, promptOf(e.claude.calls()[0]), "見て\n(送っていない添付: 取得できなかったもの 1件)\n</post>")
		evs := e.events(eventImage)
		require.Len(t, evs, 1)
		assert.Contains(t, evs[0].Message, "URLが不正です")
		assert.NotContains(t, evs[0].Message, "not a url")
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

func TestVision_Deadline(t *testing.T) {
	setCollectTimeout(t, 300*time.Millisecond)
	e := newEnv(t)
	e.settings(visionOn(nil))
	e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
		attachment("f1", "image/png"), attachment("f2", "image/png"))
	e.claude.serveFile(driveHost+"f1", pngBytes)
	e.claude.delayFile(driveHost+"f1", 5*time.Second)
	e.claude.serveFile(driveHost+"f2", pngBytes)
	e.claude.push(message("はい", "end_turn", 1, 1))

	start := time.Now()
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Less(t, time.Since(start), 3*time.Second, "全体の期限で打ち切る")

	imgs := imagesOf(t, e.claude.calls()[0])
	require.Len(t, imgs, 1)
	assert.Contains(t, promptOf(e.claude.calls()[0]), "見て\n[画像1]\n(送っていない添付: 取得できなかったもの 1件)\n</post>")
	evs := e.events(eventImage)
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0].Message, "時間内に取れませんでした")
}

func TestVision_ParallelFetchKeepsOrder(t *testing.T) {
	e := newEnv(t)
	e.settings(visionOn(nil))
	e.api.notes["n1"] = withFiles(note("n1", "alice", "alice", "", "見て", "public"),
		attachment("f1", "image/png"), attachment("f2", "image/jpeg"), attachment("f3", "image/gif"), attachment("f4", "image/webp"))
	for id, b := range map[string][]byte{"f1": pngBytes, "f2": jpegBytes, "f3": gifBytes, "f4": webpBytes} {
		e.claude.serveFile(driveHost+id, b)
		e.claude.delayFile(driveHost+id, 400*time.Millisecond)
	}
	// 最初の画像を一番遅く返しても、番号は投稿の順に振る。
	e.claude.delayFile(driveHost+"f1", 700*time.Millisecond)
	e.claude.push(message("はい", "end_turn", 1, 1))

	start := time.Now()
	require.NoError(t, e.mention("notif-1", "n1"))
	assert.Less(t, time.Since(start), 1500*time.Millisecond, "並行して取る(直列なら1.9秒かかる)")

	imgs := imagesOf(t, e.claude.calls()[0])
	require.Len(t, imgs, 4)
	assert.Equal(t, []sentBlock{
		{label: "画像1", mediaType: "image/png", data: pngBytes},
		{label: "画像2", mediaType: "image/jpeg", data: jpegBytes},
		{label: "画像3", mediaType: "image/gif", data: gifBytes},
		{label: "画像4", mediaType: "image/webp", data: webpBytes},
	}, imgs)
}

func TestVision_WithoutImages(t *testing.T) {
	assert.Nil(t, (*visionResult)(nil).withoutImages())
	vr := &visionResult{
		images: []visionImage{{Label: "画像1"}},
		notes: map[string]*noteAttachments{
			"n1": {sent: []sentImage{{n: 1}}, skipped: [numSkipReasons]int{skipSensitive: 1}},
		},
	}
	got := vr.withoutImages()
	assert.Empty(t, got.imageList())
	assert.Empty(t, got.notes["n1"].sent)
	assert.Equal(t, 1, got.notes["n1"].skipped[skipSensitive])
	assert.Equal(t, 1, got.notes["n1"].skipped[skipRejected])
	assert.Len(t, vr.notes["n1"].sent, 1, "元の結果は変えない")
	assert.Equal(t, "\n(送っていない添付: センシティブ 1件、Claudeが受け付けなかった画像 1件)",
		attachmentText(noteView{ID: "n1"}, got))
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
