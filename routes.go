package claudebot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/elythia-network/elythia/plugin"
)

/*
 * 管理画面 (`/admin/plugin/claude-bot/`) が呼ぶルート。
 *
 * 画面はモデレーター以上にしか出ないが、それは UI の都合でしかないので、
 * ここで必ず確かめる。読むのはモデレーター以上、変えるのは管理者だけ
 * (APIキーの費用と、サーバーの名前で投稿するアカウントを扱うため)。
 */

func routes(pctx plugin.Context, r plugin.Router) error {
	b := newBot(pctx)
	r.POST("/admin/state", b.requireModerator(b.routeState))
	r.POST("/admin/settings", b.requireAdmin(b.routeSaveSettings))
	r.POST("/admin/account/create", b.requireAdmin(b.routeCreateAccount))
	r.POST("/admin/account/update", b.requireAdmin(b.routeUpdateAccount))
	r.POST("/admin/account/delete", b.requireAdmin(b.routeDeleteAccount))
	return nil
}

func (b *bot) requireModerator(h plugin.Handler) plugin.Handler {
	return func(req plugin.Request) (any, error) {
		if !req.IsModerator() && !req.IsAdministrator() {
			return nil, plugin.Errorf(http.StatusForbidden, "権限がありません")
		}
		return h(req)
	}
}

func (b *bot) requireAdmin(h plugin.Handler) plugin.Handler {
	return func(req plugin.Request) (any, error) {
		if !req.IsAdministrator() {
			return nil, plugin.Errorf(http.StatusForbidden, "管理者だけが変更できます")
		}
		return h(req)
	}
}

// accountView is the bot account shown on the admin page.
type accountView struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
	AvatarURL   *string `json:"avatarUrl"`
	IsBot       bool    `json:"isBot"`
}

type stateResponse struct {
	Account           *accountView `json:"account"`
	Settings          Settings     `json:"settings"`
	MaxNoteTextLength int          `json:"maxNoteTextLength"`
	Usage             usageSummary `json:"usage"`
	Prices            []priceRow   `json:"prices"`
	Events            []event      `json:"events"`
	// DefaultDescription is the profile text set on creation.
	DefaultDescription string `json:"defaultDescription"`
	// CanEdit is true for administrators. 画面で保存のボタンを出すかに使う
	// (権限の判定そのものは各ルートが行う)。
	CanEdit bool `json:"canEdit"`
	// Warnings are notes about the saved settings that did not block saving.
	Warnings []string `json:"warnings"`
}

func (b *bot) state(req plugin.Request) (*stateResponse, error) {
	ctx := req.Context()
	db := b.db()
	s, err := loadSettings(ctx, db)
	if err != nil {
		return nil, err
	}
	usage, err := summarize(ctx, db, s)
	if err != nil {
		return nil, err
	}
	events, err := recentEvents(ctx, db, 100)
	if err != nil {
		return nil, err
	}
	acc, err := b.botAccount(ctx)
	if err != nil {
		return nil, err
	}
	res := &stateResponse{
		Settings:           s,
		MaxNoteTextLength:  b.maxNoteLength(ctx),
		Usage:              usage,
		Prices:             priceRows(),
		Events:             events,
		DefaultDescription: DefaultDescription,
		CanEdit:            req.IsAdministrator(),
		Warnings:           []string{},
	}
	if acc != nil {
		view := &accountView{ID: acc.ID, Username: acc.Username}
		// プロフィールは本体が持つ。bot 自身として i を読む。
		if me, err := call[accountView](ctx, b.pctx.API().AsUser(acc.ID), "i", map[string]any{}); err == nil {
			view.Name, view.Description, view.AvatarURL, view.IsBot = me.Name, me.Description, me.AvatarURL, me.IsBot
		} else {
			b.log.Warn("claude-bot: botのプロフィールを読めません", "err", err)
		}
		res.Account = view
	}
	return res, nil
}

func (b *bot) routeState(req plugin.Request) (any, error) {
	return b.state(req)
}

func (b *bot) routeSaveSettings(req plugin.Request) (any, error) {
	ctx := req.Context()
	var body struct {
		Settings json.RawMessage `json:"settings"`
	}
	if err := req.Bind(&body); err != nil || len(body.Settings) == 0 || string(body.Settings) == "null" {
		return nil, badRequest("設定を読めません")
	}
	// 今の設定の上に重ねる。後から足した項目(visionなど)を知らない古い画面から
	// 保存しても、その項目がゼロ値になって検証で弾かれたり、ONにしてあった
	// ものが黙って既定値に戻ったりしないようにするため(#6)。保存したことが
	// 無ければ、今の設定は既定値になる。
	s, err := loadSettings(ctx, b.db())
	if err != nil {
		return nil, err
	}
	// 配列は今の値を外してから読む。encoding/jsonは配列を今の要素の上に
	// 読むので、送られた要素に無い項目(単価の一部など)が前の要素の値のまま
	// 残るため。送られなかった配列だけ、今の値に戻す(nullは送られた扱い)。
	cur := s
	s.Scheduled.Times, s.PriceOverrides = nil, nil
	if err := json.Unmarshal(body.Settings, &s); err != nil {
		return nil, badRequest("設定を読めません")
	}
	var sent struct {
		Scheduled struct {
			Times json.RawMessage `json:"times"`
		} `json:"scheduled"`
		PriceOverrides json.RawMessage `json:"priceOverrides"`
	}
	_ = json.Unmarshal(body.Settings, &sent) // 形は上で確かめてある
	if sent.Scheduled.Times == nil {
		s.Scheduled.Times = cur.Scheduled.Times
	}
	if sent.PriceOverrides == nil {
		s.PriceOverrides = cur.PriceOverrides
	}
	if s.Scheduled.Times == nil {
		s.Scheduled.Times = []string{}
	}
	if s.PriceOverrides == nil {
		s.PriceOverrides = []PriceOverride{}
	}
	s.Model = strings.TrimSpace(s.Model)
	warnings, err := s.validate(b.maxNoteLength(ctx))
	if err != nil {
		return nil, err
	}
	if err := saveSettings(ctx, b.db(), s); err != nil {
		return nil, err
	}
	st, err := b.state(req)
	if err != nil {
		return nil, err
	}
	st.Warnings = warnings
	return st, nil
}

func (b *bot) routeCreateAccount(req plugin.Request) (any, error) {
	ctx := req.Context()
	var body struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := req.Bind(&body); err != nil {
		return nil, badRequest("入力を読めません")
	}
	existing, err := b.botAccount(ctx)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, badRequest("botのアカウントは既にあります (@%s)", existing.Username)
	}
	acc, err := b.pctx.Accounts().Create(ctx, body.Username)
	switch {
	case errors.Is(err, plugin.ErrInvalidUsername):
		return nil, badRequest("ユーザー名は半角英数字と_で20文字以内にしてください")
	case errors.Is(err, plugin.ErrUsernameUnavailable):
		return nil, badRequest("そのユーザー名は使えません (使用中・予約語・削除済みのアカウントの名前)")
	case err != nil:
		return nil, err
	}
	desc := DefaultDescription
	p := plugin.ProfileUpdate{Description: &desc}
	if name := strings.TrimSpace(body.Name); name != "" {
		p.Name = &name
	}
	if err := b.pctx.Accounts().UpdateProfile(ctx, acc.ID, p); err != nil {
		// アカウントはできているので、画面から直してもらう。
		b.logEvent(ctx, "warn", eventConfig, "botのプロフィールを設定できませんでした: "+err.Error(), "", "")
	}
	return b.state(req)
}

// maxAvatarBytes bounds the avatar. 本体の body の上限が 1 MiB で、base64 は
// 4/3 倍になるので、それに収まる大きさにする。
const maxAvatarBytes = 700 * 1024

func (b *bot) routeUpdateAccount(req plugin.Request) (any, error) {
	ctx := req.Context()
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Avatar      *struct {
			Data     string `json:"data"`
			Filename string `json:"filename"`
		} `json:"avatar"`
	}
	if err := req.Bind(&body); err != nil {
		return nil, badRequest("入力を読めません")
	}
	acc, err := b.botAccount(ctx)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, badRequest("botのアカウントがまだありません")
	}
	p := plugin.ProfileUpdate{Name: body.Name, Description: body.Description}
	if p.Name != nil && utf8.RuneCountInString(*p.Name) > 50 {
		return nil, badRequest("名前は50文字以内にしてください")
	}
	if p.Description != nil && utf8.RuneCountInString(*p.Description) > 1500 {
		return nil, badRequest("自己紹介は1500文字以内にしてください")
	}
	if body.Avatar != nil {
		data, err := base64.StdEncoding.DecodeString(body.Avatar.Data)
		if err != nil || len(data) == 0 {
			return nil, badRequest("アイコンの画像を読めません")
		}
		if len(data) > maxAvatarBytes {
			return nil, badRequest("アイコンの画像は%dKB以下にしてください", maxAvatarBytes/1024)
		}
		p.Avatar = &plugin.Image{Data: data, Filename: body.Avatar.Filename}
	}
	if err := b.pctx.Accounts().UpdateProfile(ctx, acc.ID, p); err != nil {
		return nil, profileError(err)
	}
	return b.state(req)
}

// profileError turns a profile update error into a message the admin can act
// on. i/update が返した理由 (画像でない、など) はそのまま見せる。
func profileError(err error) error {
	var apierr *plugin.APIError
	switch {
	case errors.Is(err, plugin.ErrAccountSuspended):
		return badRequest("botのアカウントが凍結されているため、変更できません")
	case errors.As(err, &apierr) && apierr.Status < 500:
		var body struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(apierr.Body, &body)
		return badRequest("プロフィールを変更できませんでした: %s %s", body.Error.Code, body.Error.Message)
	default:
		return fmt.Errorf("update profile: %w", err)
	}
}

func (b *bot) routeDeleteAccount(req plugin.Request) (any, error) {
	ctx := req.Context()
	var body struct {
		// Username must repeat the account's username, as a confirmation.
		Username string `json:"username"`
	}
	if err := req.Bind(&body); err != nil {
		return nil, badRequest("入力を読めません")
	}
	acc, err := b.botAccount(ctx)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, badRequest("botのアカウントがありません")
	}
	// 消すと投稿もフォロワーも消え、同じ名前も使えなくなる。打ち間違いで
	// 消さないよう、名前を打ち直してもらう。
	if body.Username != acc.Username {
		return nil, badRequest("確認のため、botのユーザー名 (%s) を正しく入れてください", acc.Username)
	}
	if err := b.pctx.Accounts().Delete(ctx, acc.ID); err != nil {
		return nil, err
	}
	return b.state(req)
}
