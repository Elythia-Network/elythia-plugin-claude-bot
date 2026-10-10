package claudebot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // DecodeConfig で形式と寸法を読むため
	_ "image/jpeg" // 同上
	_ "image/png"  // 同上
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp" // 同上
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
	// maxImagesBase64Total caps the images of one request, after base64.
	//
	// Claude APIのリクエストは全体で32MBまで。1枚の上限のまま上限の枚数を
	// 送ると超えて413になり、返事が沈黙する。システムプロンプトとスレッドの
	// 文の分の余裕を大きく残して20MBにする。
	maxImagesBase64Total = 20_000_000
	// imageFetchTimeout bounds fetching one image.
	imageFetchTimeout = 10 * time.Second
	// imageFetchParallel is how many images are fetched at once.
	imageFetchParallel = 4
)

// collectTimeout bounds fetching all images for one reply. 過ぎた分は送らずに
// 返事を続ける。通知のworkerを長く塞がないため。テストで短くするので変数にする。
var collectTimeout = 20 * time.Second

// decodeConfig reads the format and size of an image. テストでpanicを
// 起こすために差し替えられるようにしておく。
var decodeConfig = image.DecodeConfig

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
	skipRejected
	numSkipReasons
)

// skipLabels are shown to the model, in this order.
var skipLabels = [numSkipReasons]string{
	skipSensitive:  "センシティブ",
	skipNotImage:   "画像でないもの",
	skipUnsendable: "送れる形の無い画像",
	skipFailed:     "取得できなかったもの",
	skipOverLimit:  "上限を超えた分",
	skipRejected:   "Claudeが受け付けなかった画像",
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
	// variant says the image is not the original ("縮小版" など). 空なら元の画像。
	variant string
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

// withoutImages returns v as if Claude had refused every image. 画像を外して
// 呼び直すときのプロンプトに使う。
func (v *visionResult) withoutImages() *visionResult {
	if v == nil {
		return nil
	}
	out := &visionResult{notes: make(map[string]*noteAttachments, len(v.notes))}
	for id, na := range v.notes {
		c := &noteAttachments{skipped: na.skipped}
		c.skipped[skipRejected] += len(na.sent)
		out.notes[id] = c
	}
	return out
}

// fetchJob is one attachment to fetch.
type fetchJob struct {
	fileID string
	noteID string
	video  bool
	// original is the URL of the image as is, empty when the declared type
	// is not one Claude API accepts.
	original  string
	thumbnail string

	img *fetchedImage
	err error
}

type fetchedImage struct {
	mediaType string
	data      []byte
	thumbnail bool
}

// attachRef is one attachment of a note: a job, or a reason it has none.
type attachRef struct {
	job    int // -1 なら reason の理由で送らない
	reason skipReason
	alt    string
}

// collectImages fetches the images to send with a reply.
//
// 対象はメンションされた投稿(先に)と、IncludeThreadなら文脈として実際に
// 送る祖先(新しい方から)。祖先はcontextForを通った投稿だけを受け取るので、
// 話しかけた人が読めない投稿の画像は送らない。visionがOFFならnilを返す。
//
// 取得は並行して行うが、画像の番号と並びは上の順で決める。取れなかった
// 画像は飛ばして記録に残し、返事は続ける。
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

	// 1. 何を取るかを決める。取りに行く数で上限を数えるので、失敗した分も枠を
	// 使う。壊れた画像を大量に付けた投稿で、取得を何十回も待たされないため。
	refs := make([][]attachRef, len(targets))
	var jobs []*fetchJob
	byFile := map[string]int{}
	for i, t := range targets {
		for _, f := range t.Files {
			ref := attachRef{job: -1, alt: cleanAlt(f.Comment)}
			if j, ok := byFile[f.ID]; ok && f.ID != "" {
				// 同じファイルを付けた投稿が複数あっても、送るのは1回にする。
				ref.job = j
			} else if f.IsSensitive && !v.IncludeSensitive {
				ref.reason = skipSensitive
			} else if original, thumbnail, reason := imageSources(f); original == "" && thumbnail == "" {
				ref.reason = reason
			} else if len(jobs) >= v.MaxImages {
				ref.reason = skipOverLimit
			} else {
				ref.job = len(jobs)
				jobs = append(jobs, &fetchJob{
					fileID: f.ID, noteID: t.ID, video: strings.HasPrefix(f.Type, "video/"),
					original: original, thumbnail: thumbnail,
				})
				if f.ID != "" {
					byFile[f.ID] = ref.job
				}
			}
			refs[i] = append(refs[i], ref)
		}
	}

	// 2. 取る。
	b.fetchAll(ctx, jobs)

	// 3. 決めた順に番号を振る。
	res := &visionResult{notes: map[string]*noteAttachments{}}
	labels := make([]int, len(jobs))
	reasons := make([]skipReason, len(jobs))
	total := 0
	for j, job := range jobs {
		if job.err != nil {
			reasons[j] = skipFailed
			b.logEvent(ctx, "warn", eventImage,
				fmt.Sprintf("添付の画像(ファイル%s)を取得できなかったため、送らずに返事を続けます: %v", job.fileID, job.err), note.User.ID, job.noteID)
			continue
		}
		size := base64.StdEncoding.EncodedLen(len(job.img.data))
		if total+size > maxImagesBase64Total {
			reasons[j] = skipOverLimit
			continue
		}
		total += size
		labels[j] = len(res.images) + 1
		res.images = append(res.images, visionImage{
			Label:     fmt.Sprintf("画像%d", labels[j]),
			MediaType: job.img.mediaType,
			Data:      base64.StdEncoding.EncodeToString(job.img.data),
		})
	}
	for i, t := range targets {
		na := &noteAttachments{}
		res.notes[t.ID] = na
		for _, ref := range refs[i] {
			switch {
			case ref.job < 0:
				na.skipped[ref.reason]++
			case labels[ref.job] == 0:
				na.skipped[reasons[ref.job]]++
			default:
				na.sent = append(na.sent, sentImage{n: labels[ref.job], alt: ref.alt, variant: variantOf(jobs[ref.job])})
			}
		}
	}
	return res
}

func variantOf(j *fetchJob) string {
	switch {
	case !j.img.thumbnail:
		return ""
	case j.video:
		return "動画のサムネイル"
	default:
		return "縮小版"
	}
}

// fullwidthBrackets turns square brackets into full-width ones.
var fullwidthBrackets = strings.NewReplacer("[", "［", "]", "］")

// cleanAlt makes the alt text safe to put inside "[画像N: ...]".
//
// 角括弧は全角にする。半角のままだと、代替テキストに「] [画像2」のように
// 書いて別の画像への参照を偽れるため。
func cleanAlt(comment *string) string {
	if comment == nil {
		return ""
	}
	s := strings.Join(strings.Fields(*comment), " ")
	return fullwidthBrackets.Replace(s)
}

// imageSources picks what to fetch for f.
//
// 宣言の型をClaude APIが受け付けるなら、まず元のURLを取る。大きさと寸法は
// 宣言の値(size・properties)では弾かない。他の人向けに返る url はwebpublic
// (2048px以下)を指すことがあるが、size・propertiesは原本の値なので、宣言で
// 弾くと送れる画像までサムネイルに落ちるため。動画やAVIFなど、受け付けない
// 型は最初からサムネイルを取る。
func imageSources(f driveFile) (original, thumbnail string, reason skipReason) {
	if f.ThumbnailURL != nil {
		thumbnail = *f.ThumbnailURL
	}
	if visionMediaTypes[f.Type] && f.URL != "" {
		return f.URL, thumbnail, 0
	}
	if thumbnail != "" {
		return "", thumbnail, 0
	}
	if strings.HasPrefix(f.Type, "image/") {
		return "", "", skipUnsendable
	}
	return "", "", skipNotImage
}

// fetchAll fetches jobs in parallel within collectTimeout.
func (b *bot) fetchAll(ctx context.Context, jobs []*fetchJob) {
	if len(jobs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	sem := make(chan struct{}, imageFetchParallel)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		// pctx.Goではなく素のgoにして、自分でrecoverする。pctx.Goはpanicを
		// 記録するだけで結果を返す手段が無く、plugintestのGoはfnをその場で
		// 呼ぶので並行にならず、並行の上限をテストで確かめられないため。外から
		// 来たバイト列をデコーダーに渡すので、panicは回収しないとプロセスごと落ちる。
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					b.log.Error("claude-bot: 画像の取得でpanicしました", "panic", r, "fileId", j.fileID)
					j.img, j.err = nil, &imageError{reason: "画像の読み取りで異常が起きました"}
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				j.err = &imageError{reason: "時間内に取れませんでした"}
				return
			}
			defer func() { <-sem }()
			j.img, j.err = b.fetchJob(ctx, j)
		}()
	}
	wg.Wait()
}

// fetchJob fetches the original, falling back to the thumbnail once when
// the original turns out to be unsuitable.
func (b *bot) fetchJob(ctx context.Context, j *fetchJob) (*fetchedImage, error) {
	var first error
	if j.original != "" {
		img, err := b.fetchImage(ctx, j.original)
		if err == nil {
			return img, nil
		}
		var ie *imageError
		if !errors.As(err, &ie) || !ie.unsuitable || j.thumbnail == "" {
			return nil, err
		}
		first = err
	}
	img, err := b.fetchImage(ctx, j.thumbnail)
	if err != nil {
		if first != nil {
			return nil, &imageError{reason: fmt.Sprintf("元の画像: %v、サムネイル: %v", first, err)}
		}
		return nil, err
	}
	img.thumbnail = true
	return img, nil
}

// imageError is why an image could not be used. 文面にURLを入れない。
// proxyのURLは署名を含み、記録は管理画面に出るため。
type imageError struct {
	reason string
	// unsuitable means the image was fetched but cannot be sent as is
	// (大きすぎる、形式が違う)。サムネイルなら送れることがある。
	unsuitable bool
}

func (e *imageError) Error() string { return e.reason }

func unsuitable(format string, args ...any) error {
	return &imageError{reason: fmt.Sprintf(format, args...), unsuitable: true}
}

// fetchImage downloads one image through the host's HTTP client and checks
// that Claude API can take it.
//
// 宣言された型と寸法は信用せず、中身から決める。リモートのファイルの型は
// 相手のサーバーが決めるので、偽ることができる。全体のデコードはせず、
// 形式と寸法を読むためにヘッダーだけを読む(DecodeConfig)。先頭だけ画像の
// 形をした壊れたファイルや多言語のファイルを送ると、APIが400を返して返事が
// 丸ごと止まるため。
func (b *bot) fetchImage(ctx context.Context, rawURL string) (*fetchedImage, error) {
	ctx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &imageError{reason: "URLが不正です"}
	}
	resp, err := b.pctx.HTTP().Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer resp.Body.Close() //nolint:errcheck // 読み捨て
	if resp.StatusCode != http.StatusOK {
		return nil, &imageError{reason: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if len(data) > maxImageBytes {
		return nil, unsuitable("大きすぎます(%dバイトを超えます)", maxImageBytes)
	}
	mediaType := http.DetectContentType(data)
	if !visionMediaTypes[mediaType] {
		return nil, unsuitable("JPEG・PNG・GIF・WebPのどれでもない中身です(%s)", mediaType)
	}
	cfg, format, err := decodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, unsuitable("画像として読めません(%s)", mediaType)
	}
	// 今登録しているデコーダーは型の判定と同じ先頭を見るので、ここで食い違う
	// ことは無い。デコーダーを足したときに、宣言と違う形式を送らない守りとして残す。
	if "image/"+format != mediaType {
		return nil, unsuitable("中身の形式が食い違っています(%s、%s)", mediaType, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImageDimension || cfg.Height > maxImageDimension {
		return nil, unsuitable("寸法が送れる範囲を超えています(%dx%d)", cfg.Width, cfg.Height)
	}
	return &fetchedImage{mediaType: mediaType, data: data}, nil
}

// transportError describes a failed request without the URL.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &imageError{reason: "時間内に取れませんでした"}
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	return &imageError{reason: "通信に失敗しました: " + err.Error()}
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
		sb.WriteString(fmt.Sprintf("\n[画像%d", im.n))
		if im.variant != "" {
			sb.WriteString("(" + im.variant + ")")
		}
		if im.alt != "" {
			sb.WriteString(": " + im.alt)
		}
		sb.WriteString("]")
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
