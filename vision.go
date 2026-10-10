package claudebot

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

/*
 * 添付の画像をClaudeへ送る部分(#6)。
 *
 * 画像は本体のctx.HTTP()で取ってbase64で送る。URLをClaudeに渡して取らせる
 * 形にはしない。フォロワー限定の投稿の画像のURLを外部へ渡さないため、また
 * 本体のproxyとSSRFガードを通すため。
 */

const (
	// maxImageBytes is the largest image sent, before base64.
	//
	// Claude APIは1枚5MBを超える画像を断る。base64は3バイトを4文字にするので、
	// base64にしても5MBに収まる大きさ(生で3.75MB)を上限にする。上限が生の
	// 大きさで数えられても、base64の大きさで数えられても通るようにするため。
	maxImageBytes = 5_000_000 / 4 * 3
	// maxImageDimension is the longest side Claude API accepts.
	maxImageDimension = 8000
	// imageFetchTimeout bounds fetching one image. 取得は1枚ずつなので、
	// 上限の枚数だけ重なっても通知の処理が長くなりすぎないよう短くする。
	imageFetchTimeout = 10 * time.Second
)

// visionMediaTypes are the formats Claude API accepts.
var visionMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// skipReason is why an attachment was not sent.
type skipReason int

const (
	skipSensitive skipReason = iota
	skipNotImage
	skipUnsendable
	skipFailed
	skipOverLimit
	numSkipReasons
)

// skipLabels are shown to the model, in this order.
var skipLabels = [numSkipReasons]string{
	skipSensitive:  "センシティブ",
	skipNotImage:   "画像でないもの",
	skipUnsendable: "送れる形の無い画像",
	skipFailed:     "取得できなかったもの",
	skipOverLimit:  "上限を超えた分",
}

// visionImage is one image sent to the model.
type visionImage struct {
	// Label is "画像N", sent as a text block before the image.
	Label     string
	MediaType string
	// Data is the image in base64.
	Data string
}

// sentImage is how a note refers to an image it sent.
type sentImage struct {
	n   int
	alt string
}

// noteAttachments is what the prompt says about one note's attachments.
type noteAttachments struct {
	sent    []sentImage
	skipped [numSkipReasons]int
}

// visionResult is the images collected for one reply.
type visionResult struct {
	images []visionImage
	// notes holds the notes whose attachments were looked at, by ID. ここに
	// 無い投稿(スレッドの画像を送らない設定の祖先)は、従来どおり件数だけを書く。
	notes map[string]*noteAttachments
}

func (v *visionResult) imageList() []visionImage {
	if v == nil {
		return nil
	}
	return v.images
}

// collectImages fetches the images to send with a reply.
//
// 対象はメンションされた投稿(先に)と、IncludeThreadなら文脈として実際に
// 送る祖先(新しい方から)。祖先はcontextForを通った投稿だけを受け取るので、
// 話しかけた人が読めない投稿の画像は送らない。visionがOFFならnilを返す。
//
// 取れなかった画像は飛ばして記録に残し、返事は続ける。
func (b *bot) collectImages(ctx context.Context, v VisionSettings, note noteView, ctxNotes []noteView, contextNotes int) *visionResult {
	if !v.Enabled {
		return nil
	}
	targets := []noteView{note}
	if v.IncludeThread {
		sent := promptThread(ctxNotes, contextNotes)
		for i := len(sent) - 1; i >= 0; i-- {
			targets = append(targets, sent[i])
		}
	}
	res := &visionResult{notes: map[string]*noteAttachments{}}
	byFile := map[string]int{}
	// 取りに行った回数で上限を数える。失敗した分も枠を使うのは、壊れた画像を
	// 大量に付けた投稿で、1枚10秒の取得を何十回も待たされないようにするため。
	attempts := 0
	for _, t := range targets {
		na := &noteAttachments{}
		res.notes[t.ID] = na
		for _, f := range t.Files {
			alt := ""
			if f.Comment != nil {
				alt = strings.Join(strings.Fields(*f.Comment), " ")
			}
			if n, ok := byFile[f.ID]; ok && f.ID != "" {
				// 同じファイルを付けた投稿が複数あっても、送るのは1回にする。
				na.sent = append(na.sent, sentImage{n: n, alt: alt})
				continue
			}
			if f.IsSensitive && !v.IncludeSensitive {
				na.skipped[skipSensitive]++
				continue
			}
			url, reason := imageSource(f)
			if url == "" {
				na.skipped[reason]++
				continue
			}
			if attempts >= v.MaxImages {
				na.skipped[skipOverLimit]++
				continue
			}
			attempts++
			mediaType, data, err := b.fetchImage(ctx, url)
			if err != nil {
				na.skipped[skipFailed]++
				b.logEvent(ctx, "warn", eventImage,
					fmt.Sprintf("添付の画像(ファイル%s)を取得できなかったため、送らずに返事を続けます: %v", f.ID, err), note.User.ID, t.ID)
				continue
			}
			n := len(res.images) + 1
			res.images = append(res.images, visionImage{
				Label:     fmt.Sprintf("画像%d", n),
				MediaType: mediaType,
				Data:      base64.StdEncoding.EncodeToString(data),
			})
			if f.ID != "" {
				byFile[f.ID] = n
			}
			na.sent = append(na.sent, sentImage{n: n, alt: alt})
		}
	}
	return res
}

// imageSource picks the URL to fetch for f: the original when Claude API
// takes it as is, the thumbnail otherwise.
func imageSource(f driveFile) (string, skipReason) {
	dimOK := func(v *float64) bool { return v == nil || *v <= maxImageDimension }
	if visionMediaTypes[f.Type] && f.URL != "" && f.Size <= maxImageBytes &&
		dimOK(f.Properties.Width) && dimOK(f.Properties.Height) {
		return f.URL, 0
	}
	// 動画やAVIF、大きすぎる画像は、本体が作ったサムネイル(webp)を送る。
	if f.ThumbnailURL != nil && *f.ThumbnailURL != "" {
		return *f.ThumbnailURL, 0
	}
	if strings.HasPrefix(f.Type, "image/") {
		return "", skipUnsendable
	}
	return "", skipNotImage
}

// fetchImage downloads one image through the host's HTTP client.
//
// 宣言された型は信用せず、中身から型を決める。リモートのファイルの型は
// 相手のサーバーが決めるので、偽ることができる。デコードはしない(Claude側で
// する)。
func (b *bot) fetchImage(ctx context.Context, url string) (string, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := b.pctx.HTTP().Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // 読み捨て
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", nil, err
	}
	if len(data) > maxImageBytes {
		return "", nil, fmt.Errorf("大きすぎます(%dバイトを超えます)", maxImageBytes)
	}
	mediaType := http.DetectContentType(data)
	if !visionMediaTypes[mediaType] {
		return "", nil, errors.New("JPEG・PNG・GIF・WebPのどれでもない中身です(" + mediaType + ")")
	}
	return mediaType, data, nil
}

// attachmentText renders the attachments of n for the prompt.
func attachmentText(n noteView, vr *visionResult) string {
	var na *noteAttachments
	if vr != nil {
		na = vr.notes[n.ID]
	}
	if na == nil {
		if len(n.FileIDs) > 0 {
			return fmt.Sprintf("\n(添付ファイル%d件)", len(n.FileIDs))
		}
		return ""
	}
	var sb strings.Builder
	for _, im := range na.sent {
		if im.alt != "" {
			fmt.Fprintf(&sb, "\n[画像%d: %s]", im.n, im.alt)
		} else {
			fmt.Fprintf(&sb, "\n[画像%d]", im.n)
		}
	}
	var parts []string
	for r, c := range na.skipped {
		if c > 0 {
			parts = append(parts, fmt.Sprintf("%s %d件", skipLabels[r], c))
		}
	}
	if len(parts) > 0 {
		sb.WriteString("\n(送っていない添付: " + strings.Join(parts, "、") + ")")
	}
	return sb.String()
}
