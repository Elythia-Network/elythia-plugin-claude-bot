// Package claudebot is an Elythia server plugin that runs a bot account which
// answers mentions and posts on a schedule using the Claude API.
//
// botのアカウント・APIキー・振る舞いの設定は、全て管理画面
// (`/admin/plugin/claude-bot/`) から行う。設定ファイルに書くものは無い。
package claudebot

import (
	"time"
	// 定時の投稿と「今日」「今月」の区切りに IANA のタイムゾーンを使う。
	// alpine など tzdata を持たない image でも動くよう、埋め込んでおく。
	_ "time/tzdata"

	"github.com/elythia-network/elythia/plugin"
)

// Name is the plugin name. URL のパス・キュー・schema に使われる。
const Name = "claude-bot"

// Plugin is the entry point referenced by the generated registration code.
var Plugin = plugin.Definition{
	Name:       Name,
	Version:    "0.1.0",
	APIVersion: plugin.APIVersion,
	Migrations: migrations,
	Routes:     routes,
	Jobs:       jobs,
	// メンションと返信は通知の handler で受け取る (Elythia #3469)。
	// i/notifications を定期的に読みに行かずに済む。
	Notifications: notifications,
	Secrets: []plugin.SecretSpec{
		{Name: secretAPIKey, Description: "Claude API の API キー (sk-ant-...)"},
	},
}

const secretAPIKey = "apiKey"

// now is the clock. テストで定時の投稿の時刻を決めるために差し替える。
var now = time.Now

var migrations = []plugin.Migration{
	{Version: 1, SQL: `
CREATE TABLE settings (
	id         smallint PRIMARY KEY CHECK (id = 1),
	data       jsonb NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now()
);

-- 通知ごとの処理状態。at-least-once で同じ通知が再び届いても、返事を
-- 二重に投稿しないために使う。投稿前の本文と一緒に、その本文を書いたときの
-- 公開範囲を残す。再試行の間に文脈の投稿が消えると、計算し直した公開範囲が
-- 広がりうるため。
CREATE TABLE handled_notifications (
	id               text PRIMARY KEY,
	status           text NOT NULL,
	claimed_at       timestamptz NOT NULL DEFAULT now(),
	finished_at      timestamptz,
	outcome          text NOT NULL DEFAULT '',
	reply_text       text,
	reply_visibility text,
	reply_local_only boolean NOT NULL DEFAULT false
);
CREATE INDEX handled_notifications_claimed_at ON handled_notifications (claimed_at);

-- Claude API を呼んだ記録。呼び直した分も、失敗した呼び出しも 1 行になる。
-- host は呼び出した相手のサーバーで、リモートのサーバーごとの 1 日の上限に
-- 使う。ローカルの利用者と定時の投稿は NULL。
CREATE TABLE usage_log (
	id                    bigserial PRIMARY KEY,
	at                    timestamptz NOT NULL DEFAULT now(),
	kind                  text NOT NULL,
	user_id               text,
	host                  text,
	model                 text NOT NULL,
	input_tokens          bigint NOT NULL DEFAULT 0,
	output_tokens         bigint NOT NULL DEFAULT 0,
	cache_creation_tokens bigint NOT NULL DEFAULT 0,
	cache_read_tokens     bigint NOT NULL DEFAULT 0,
	stop_reason           text NOT NULL DEFAULT ''
);
CREATE INDEX usage_log_at ON usage_log (at);
CREATE INDEX usage_log_user_at ON usage_log (user_id, at);
CREATE INDEX usage_log_host_at ON usage_log (host, at);

-- 管理画面に出す記録 (エラー・沈黙した理由・断った相手)。
CREATE TABLE events (
	id      bigserial PRIMARY KEY,
	at      timestamptz NOT NULL DEFAULT now(),
	level   text NOT NULL,
	kind    text NOT NULL,
	message text NOT NULL,
	user_id text,
	note_id text
);
CREATE INDEX events_at ON events (at);
CREATE INDEX events_kind_user_at ON events (kind, user_id, at);

-- 定時の投稿の枠。同じ枠を 2 回投稿しないために、投稿する前に取る。
CREATE TABLE scheduled_runs (
	slot       timestamptz PRIMARY KEY,
	claimed_at timestamptz NOT NULL DEFAULT now()
);

-- bot が投稿した返事。スレッドの根ごとに数えて、同じスレッドでの往復の
-- 上限に使う。文脈として読む祖先の窓に頼ると、相手が自分の投稿を挟んで
-- 窓の外へ押し出せるため。
CREATE TABLE bot_replies (
	notification_id text PRIMARY KEY,
	root_id         text NOT NULL,
	user_id         text NOT NULL,
	at              timestamptz NOT NULL
);
CREATE INDEX bot_replies_root ON bot_replies (root_id);
`},
}
