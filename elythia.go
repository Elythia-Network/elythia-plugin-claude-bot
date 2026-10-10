package claudebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/elythia-network/elythia/plugin"
)

/*
 * 本体の API を呼ぶ部分。
 *
 * 投稿の取得も投稿も、bot のアカウントとして (AsUser) 呼ぶ。可視性は本体が
 * 判定するので、bot が読めない投稿は文脈に入らない。
 */

// userLite is the part of Misskey's UserLite the bot reads.
type userLite struct {
	ID       string  `json:"id"`
	Username string  `json:"username"`
	Host     *string `json:"host"`
	IsBot    bool    `json:"isBot"`
}

// acct is "@user" for local users and "@user@host" for remote ones.
func (u userLite) acct() string {
	if u.Host != nil && *u.Host != "" {
		return "@" + u.Username + "@" + *u.Host
	}
	return "@" + u.Username
}

func (u userLite) isLocal() bool { return u.Host == nil || *u.Host == "" }

// remoteHost is the user's server, or "" for local users.
func (u userLite) remoteHost() string {
	if u.isLocal() {
		return ""
	}
	return *u.Host
}

// noteView is the part of a packed note the bot reads.
type noteView struct {
	ID         string   `json:"id"`
	Text       *string  `json:"text"`
	CW         *string  `json:"cw"`
	Visibility string   `json:"visibility"`
	LocalOnly  bool     `json:"localOnly"`
	User       userLite `json:"user"`
	ReplyID    *string  `json:"replyId"`
	IsHidden   bool     `json:"isHidden"`
	FileIDs    []string `json:"fileIds"`
	// Files are the attachments (#6). 画像を送るときだけ使う。
	Files []driveFile `json:"files"`
}

// driveFile is the part of Misskey's packed DriveFile the bot reads.
//
// 大きさと寸法はfloat64で受ける。形の違う値(小数など)が来ても投稿ごと
// 読めなくならないようにするため。読めないと返事そのものが止まる。
type driveFile struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	URL          string  `json:"url"`
	ThumbnailURL *string `json:"thumbnailUrl"`
	Size         float64 `json:"size"`
	IsSensitive  bool    `json:"isSensitive"`
	Comment      *string `json:"comment"`
	Properties   struct {
		Width  *float64 `json:"width"`
		Height *float64 `json:"height"`
	} `json:"properties"`
}

func (n noteView) text() string {
	if n.Text == nil {
		return ""
	}
	return *n.Text
}

func call[T any](ctx context.Context, c plugin.Caller, endpoint string, params any) (T, error) {
	var out T
	raw, err := c.Call(ctx, endpoint, params)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// 応答の形が違うのは繰り返しても直らない。
		return out, plugin.NoRetry(fmt.Errorf("decode %s: %w", endpoint, err))
	}
	return out, nil
}

// isClientError reports whether err is a 4xx (other than 429) from the host
// API.
//
// 4xx は呼び直しても結果が変わらない (投稿が消された、見えない、など) ので
// 再試行しない。429 (bot のレート制限)・5xx・通信の失敗は、通知の再試行に
// 任せる。
func isClientError(err error) bool {
	var apierr *plugin.APIError
	return errors.As(err, &apierr) && apierr.Status >= 400 && apierr.Status < 500 && apierr.Status != http.StatusTooManyRequests
}

// apiErrorCode returns Misskey's error code ("NO_SUCH_NOTE" など) from err.
func apiErrorCode(err error) string {
	var apierr *plugin.APIError
	if !errors.As(err, &apierr) {
		return ""
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(apierr.Body, &body)
	return body.Error.Code
}

// defaultMaxNoteLength is upstream's MAX_NOTE_TEXT_LENGTH.
const defaultMaxNoteLength = 3000

// maxNoteLength reads the server's note length limit from meta.
func (b *bot) maxNoteLength(ctx context.Context) int {
	api := b.pctx.API()
	if api == nil {
		return defaultMaxNoteLength
	}
	m, err := call[struct {
		MaxNoteTextLength int `json:"maxNoteTextLength"`
	}](ctx, api.Anonymous(), "meta", map[string]any{"detail": false})
	if err != nil || m.MaxNoteTextLength <= 0 {
		return defaultMaxNoteLength
	}
	return m.MaxNoteTextLength
}

// botAccount returns the account this plugin manages, if any.
//
// 管理するアカウントは 1 つだけにする (作成のルートが 2 つ目を断る)。
func (b *bot) botAccount(ctx context.Context) (*plugin.Account, error) {
	accs, err := b.pctx.Accounts().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	if len(accs) == 0 {
		return nil, nil
	}
	return &accs[0], nil
}

// visibilityRank orders visibilities from the widest.
var visibilityRank = map[string]int{"public": 0, "home": 1, "followers": 2, "specified": 3}

// clampVisibility returns want, narrowed so it is never wider than original.
func clampVisibility(want, original string) string {
	w, ok := visibilityRank[want]
	if !ok {
		w = visibilityRank["home"]
		want = "home"
	}
	o, ok := visibilityRank[original]
	if !ok {
		// 知らない値は最も狭いものとして扱う (広げる方に倒さない)。
		return "followers"
	}
	if o > w {
		return original
	}
	return want
}
