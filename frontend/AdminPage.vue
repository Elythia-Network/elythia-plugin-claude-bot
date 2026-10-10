<!--
SPDX-FileCopyrightText: Elythia project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_gaps_m" :class="$style.root">
	<div v-if="loadError" :class="$style.error">{{ loadError }}</div>

	<template v-else-if="state && draft">
		<div v-if="!state.canEdit" :class="$style.note">設定を変えられるのは管理者だけです。ここでは見るだけです。</div>
		<div v-if="state.usage.budget?.exhausted" :class="$style.error">
			今月の予算を使い切りました (概算)。<template v-if="draft.budget.stopWhenExhausted">返事と定時の投稿を止めています。</template>
		</div>
		<div v-else-if="state.usage.budget?.warning" :class="$style.warn">
			今月の予算の残りが少なくなっています (概算の残り {{ usd(state.usage.budget.remainingUsd) }})。
		</div>

		<!-- アカウント -->
		<MkFolder :defaultOpen="true">
			<template #label>botのアカウント</template>
			<template #suffix>{{ state.account ? '@' + state.account.username : '未作成' }}</template>

			<div v-if="!state.account" class="_gaps_s">
				<MkInput v-model="newUsername" type="text" placeholder="claude">
					<template #label>ユーザー名 (ID)</template>
					<template #caption>半角英数字と_で20文字以内。後から変えられず、削除した後も同じ名前は使えません。</template>
				</MkInput>
				<MkInput v-model="newName" type="text">
					<template #label>名前</template>
				</MkInput>
				<div :class="$style.note">
					botの印 (isBot) は必ず付き、外せません。このアカウントにはログインできません。
					自己紹介には、メンションした投稿がAnthropicへ送られることを既定で書きます。
				</div>
				<div class="_buttons">
					<MkButton primary :disabled="busy || !state.canEdit || newUsername === ''" @click="createAccount">作成</MkButton>
				</div>
			</div>

			<div v-else class="_gaps_s">
				<div :class="$style.account">
					<img v-if="state.account.avatarUrl" :src="state.account.avatarUrl" :class="$style.avatar" alt=""/>
					<div :class="$style.accountBody">
						<div><b>{{ state.account.name || state.account.username }}</b> @{{ state.account.username }}</div>
						<div :class="$style.note">bot: {{ state.account.isBot ? 'はい' : 'いいえ' }}</div>
					</div>
				</div>
				<MkInput v-model="profileName" type="text">
					<template #label>名前</template>
				</MkInput>
				<div>
					<div :class="$style.label">自己紹介</div>
					<textarea v-model="profileDescription" :class="$style.textarea" rows="4"></textarea>
					<div :class="$style.note">メンションした投稿がAnthropicへ送られることを、利用者に分かるように書いてください。</div>
					<div v-if="draft.vision.enabled" :class="$style.warn">「画像を見る」が有効なので、添付の画像も送られることを書いてください。</div>
				</div>
				<div>
					<div :class="$style.label">アイコン</div>
					<input type="file" accept="image/*" @change="onAvatar"/>
				</div>
				<div class="_buttons">
					<MkButton primary :disabled="busy || !state.canEdit" @click="updateProfile">プロフィールを保存</MkButton>
				</div>

				<MkFolder :defaultOpen="false">
					<template #label>アカウントを削除する</template>
					<div class="_gaps_s">
						<div :class="$style.warn">
							投稿・フォロー・ドライブも消え、連合先へ削除が届きます。同じユーザー名は二度と使えません。
							プラグインを無効にするだけなら、アカウントと投稿は残ります。
						</div>
						<MkInput v-model="deleteConfirm" type="text">
							<template #label>確認のため、ユーザー名 ({{ state.account.username }}) を入れてください</template>
						</MkInput>
						<div class="_buttons">
							<MkButton danger :disabled="busy || !state.canEdit || deleteConfirm !== state.account.username" @click="deleteAccount">削除</MkButton>
						</div>
					</div>
				</MkFolder>
			</div>
		</MkFolder>

		<!-- APIキー -->
		<MkFolder :defaultOpen="true">
			<template #label>Claude APIのキー</template>
			<MkPluginSecrets plugin="claude-bot"/>
		</MkFolder>

		<!-- モデル -->
		<MkFolder :defaultOpen="true">
			<template #label>モデル</template>
			<template #suffix>{{ draft.model }}</template>
			<div class="_gaps_s">
				<MkInput v-model="draft.model" type="text" :datalist="modelIds">
					<template #label>モデルのID</template>
					<template #caption>例: claude-opus-5-5。新しいモデルが出たら、IDを入れればプラグインを更新せずに使えます。</template>
				</MkInput>
				<MkSelect v-model="draft.effort" :items="effortItems">
					<template #label>effort (考える深さ)</template>
					<template #caption>Haiku 4.5 と Sonnet 4.5 は effort に対応していないので「送らない」を選んでください (Opus 4.5 は high まで、4.6 は xhigh を除く)。対応しない組み合わせは保存できません。effort を上げると、自動の max_tokens も思考の分だけ大きくなります。「送らない」ではモデルの既定 (Opus 5.5 は medium、多くは high) で考えるので、high と同じだけ空けます。</template>
				</MkSelect>
				<MkInput v-model="draft.timezone" type="text">
					<template #label>タイムゾーン</template>
					<template #caption>定時の投稿の時刻と、「今日」「今月」の区切りに使います (例: Asia/Tokyo)。</template>
				</MkInput>
			</div>
		</MkFolder>

		<!-- 返事 -->
		<MkFolder :defaultOpen="true">
			<template #label>メンションへの返事</template>
			<div class="_gaps_s">
				<MkSelect v-model="draft.reply.mode" :items="modeItems">
					<template #label>反応のしかた</template>
				</MkSelect>
				<MkSelect v-model="draft.reply.audience" :items="audienceItems">
					<template #label>話しかけられる人</template>
				</MkSelect>
				<MkSelect v-model="draft.reply.visibility" :items="visibilityItems">
					<template #label>返事の公開範囲</template>
					<template #caption>元の投稿より広い範囲にはしません。指名 (DM) の投稿とチャットには反応しません。</template>
				</MkSelect>
				<div>
					<div :class="$style.label">システムプロンプト</div>
					<textarea v-model="draft.reply.systemPrompt" :class="$style.textarea" rows="6"></textarea>
				</div>
				<MkInput v-model="draft.reply.maxChars" type="number" :min="1" :max="state.maxNoteTextLength">
					<template #label>長さの上限 (文字数)</template>
					<template #caption>システムプロンプトの末尾に「{{ draft.reply.maxChars }}文字以内で」と足します。投稿の文字数の上限 ({{ state.maxNoteTextLength }}) まで。</template>
				</MkInput>
				<MkInput v-model="draft.reply.maxTokens" type="number" :min="0">
					<template #label>max_tokens (0で自動)</template>
					<template #caption>出力を切るだけの安全装置です。自動なら {{ autoMaxTokens(draft.reply.maxChars, draft.effort) }}。打ち切られたら1回だけ「もっと短く」と呼び直し、それでも切れたら投稿しません。</template>
				</MkInput>
				<MkInput v-model="draft.reply.contextNotes" type="number" :min="1" :max="50">
					<template #label>文脈として送るスレッドの投稿の数</template>
				</MkInput>
				<MkInput v-model="draft.reply.maxRoundTrips" type="number" :min="0" :max="100">
					<template #label>同じスレッドで返事をする回数の上限 (0で無制限)</template>
					<template #caption>botの印が付いた相手には、この設定によらず返事をしません。</template>
				</MkInput>
			</div>
		</MkFolder>

		<!-- 画像を見る -->
		<MkFolder :defaultOpen="false">
			<template #label>画像を見る</template>
			<template #suffix>{{ draft.vision.enabled ? '有効' : '無効' }}</template>
			<div class="_gaps_s">
				<MkSwitch v-model="draft.vision.enabled">添付の画像をClaudeへ送る</MkSwitch>
				<div v-if="draft.vision.enabled" :class="$style.warn">
					画像もAnthropic社のClaude APIへ送られます。botの自己紹介の文も書き換えてください。
				</div>
				<div :class="$style.note">
					送るのは、話しかけた人も読める投稿の画像だけです。JPEG・PNG・GIF・WebPで大きすぎない画像は元の画像を、それ以外(動画や大きな画像など)はサムネイルを送ります。取れなかった画像は飛ばして返事を続け、記録に残します。画像の分だけ使うtokenが増えます。
				</div>
				<MkInput v-model="draft.vision.maxImages" type="number" :min="1" :max="20">
					<template #label>1回の返事で送る画像の数の上限</template>
					<template #caption>1〜20。取れなかった画像も数えます。</template>
				</MkInput>
				<MkSwitch v-model="draft.vision.includeThread">スレッドの投稿の画像も送る</MkSwitch>
				<div :class="$style.note">オフなら、メンションされた投稿の画像だけを送ります。オンでも、文脈として送る投稿の画像に限ります。</div>
				<MkSwitch v-model="draft.vision.includeSensitive">センシティブ指定の画像も送る</MkSwitch>
			</div>
		</MkFolder>

		<!-- 定時の投稿 -->
		<MkFolder :defaultOpen="false">
			<template #label>定時の投稿</template>
			<template #suffix>{{ draft.scheduled.enabled ? '有効' : '無効' }}</template>
			<div class="_gaps_s">
				<MkSwitch v-model="draft.scheduled.enabled">定時の投稿をする</MkSwitch>
				<MkInput v-model="scheduledTimes" type="text" placeholder="07:00, 12:00, 21:00">
					<template #label>時刻 (HH:MM、カンマ区切り)</template>
				</MkInput>
				<MkInput v-model="draft.scheduled.intervalMinutes" type="number" :min="0" :max="1440">
					<template #label>間隔 (分。0時から数える。0で使わない)</template>
				</MkInput>
				<MkSelect v-model="draft.scheduled.visibility" :items="visibilityItems">
					<template #label>公開範囲</template>
				</MkSelect>
				<div>
					<div :class="$style.label">システムプロンプト</div>
					<textarea v-model="draft.scheduled.systemPrompt" :class="$style.textarea" rows="6"></textarea>
				</div>
				<MkInput v-model="draft.scheduled.maxChars" type="number" :min="1" :max="state.maxNoteTextLength">
					<template #label>長さの上限 (文字数)</template>
				</MkInput>
				<MkInput v-model="draft.scheduled.maxTokens" type="number" :min="0">
					<template #label>max_tokens (0で自動)</template>
					<template #caption>自動なら {{ autoMaxTokens(draft.scheduled.maxChars, draft.effort) }}。</template>
				</MkInput>
			</div>
		</MkFolder>

		<!-- 費用の上限 -->
		<MkFolder :defaultOpen="false">
			<template #label>費用の上限</template>
			<div class="_gaps_s">
				<div :class="$style.note">回数はClaude APIを呼んだ回数で数えます。打ち切られて呼び直した分も数えます。0で上限なし。</div>
				<MkInput v-model="draft.limits.perUserPerHour" type="number" :min="0">
					<template #label>1人あたりの回数 (1時間)</template>
				</MkInput>
				<MkInput v-model="draft.limits.globalPerDay" type="number" :min="0">
					<template #label>全体の回数 (1日)</template>
				</MkInput>
				<MkInput v-model="draft.limits.perHostPerDay" type="number" :min="0">
					<template #label>リモートのサーバーごとの回数 (1日)</template>
					<template #caption>誰でも話しかけられる設定では、1つのサーバーにアカウントを大量に作られると全体の回数を使い切られます。フォロワーだけかローカルの人だけにするか、ここで抑えてください。</template>
				</MkInput>
				<MkInput v-model="draft.limits.monthlyTokens" type="number" :min="0">
					<template #label>1か月のtokenの量</template>
				</MkInput>
				<MkSelect v-model="draft.limits.overLimit" :items="overLimitItems">
					<template #label>上限を超えたとき</template>
				</MkSelect>
				<MkInput v-if="draft.limits.overLimit === 'message'" v-model="draft.limits.overLimitMessage" type="text">
					<template #label>断るときの文</template>
					<template #caption>同じ相手には1時間に1回まで送ります。</template>
				</MkInput>
				<MkInput v-model="draft.budget.monthlyUsd" type="number" :min="0" step="0.01">
					<template #label>今月の予算 (USD、0で使わない)</template>
					<template #caption>前払いのクレジットなら、入金額を入れると残高の目安になります。</template>
				</MkInput>
				<MkInput v-model="draft.budget.warnPercent" type="number" :min="0" :max="100">
					<template #label>残りがこの割合 (%) を下回ったら警告する</template>
				</MkInput>
				<MkSwitch v-model="draft.budget.stopWhenExhausted">予算を使い切ったら返事と定時の投稿を止める</MkSwitch>
			</div>
		</MkFolder>

		<div v-if="saveError" :class="$style.error">{{ saveError }}</div>
		<div v-if="saved" :class="$style.ok">保存しました。</div>
		<div v-for="(w, i) in state.warnings" :key="i" :class="$style.warn">{{ w }}</div>
		<div v-if="state.canEdit" class="_buttons">
			<MkButton primary :disabled="busy" @click="saveSettings">設定を保存</MkButton>
		</div>

		<!-- 使った量 -->
		<MkFolder :defaultOpen="true">
			<template #label>使った量と概算の額</template>
			<div class="_gaps_s">
				<div :class="$style.note">額はtoken数 × 単価で計算した概算です。実際の請求とは異なることがあります。</div>
				<!-- 狭い画面でもはみ出さないよう、期間を列にして項目を縦に並べる。 -->
				<div :class="$style.scroll">
					<table :class="$style.table">
						<thead>
							<tr><th></th><th v-for="p in periods" :key="p.label">{{ p.label }}</th></tr>
						</thead>
						<tbody>
							<tr v-for="r in usageRows" :key="r.label">
								<th scope="row">{{ r.label }}</th>
								<td v-for="p in periods" :key="p.label">{{ r.value(p.usage) }}</td>
							</tr>
						</tbody>
					</table>
				</div>
				<div v-if="state.usage.month.unpriced.length > 0" :class="$style.warn">
					単価が未設定のモデルがあります ({{ state.usage.month.unpriced.join(', ') }})。これらはtoken数だけを数え、額に含めていません。
				</div>
				<div v-if="state.usage.month.byModel.length > 0" :class="$style.scroll">
					<table :class="$style.table">
						<thead><tr><th>今月のモデル別</th><th>回数</th><th>token</th><th>概算の額</th></tr></thead>
						<tbody>
							<tr v-for="m in state.usage.month.byModel" :key="m.model">
								<td :class="$style.message"><code>{{ m.model }}</code></td>
								<td>{{ m.calls }}</td>
								<td>{{ totalTokens(m.tokens) }}</td>
								<td>{{ m.costUsd == null ? '単価が未設定' : usd(m.costUsd) }}</td>
							</tr>
						</tbody>
					</table>
				</div>
				<div v-if="state.usage.budget">
					予算 {{ usd(state.usage.budget.monthlyUsd) }} − 概算 {{ usd(state.usage.budget.spentUsd) }} =
					<b :class="state.usage.budget.warning ? $style.bad : undefined">残り {{ usd(state.usage.budget.remainingUsd) }}</b>
					<span v-if="state.usage.budget.underestimated" :class="$style.note"> (単価が未設定の分を含まない)</span>
				</div>
			</div>
		</MkFolder>

		<!-- 単価 -->
		<MkFolder :defaultOpen="false">
			<template #label>単価 (USD / 100万token)</template>
			<div class="_gaps_s">
				<div :class="$style.scroll">
					<table :class="$style.table">
						<thead><tr><th>モデル</th><th>入力</th><th>キャッシュ書込 (5分)</th><th>キャッシュ読込</th><th>出力</th></tr></thead>
						<tbody>
							<template v-for="p in state.prices" :key="p.model">
								<tr>
									<td :class="$style.message"><code>{{ p.model }}</code><span v-if="p.dated" :class="$style.note"> (日付付きのIDも)</span><span v-if="p.large" :class="$style.note"> (入力が{{ p.largeThreshold }}token以下)</span></td>
									<td>{{ p.input }}</td><td>{{ p.cacheWrite }}</td><td>{{ p.cacheRead }}</td><td>{{ p.output }}</td>
								</tr>
								<tr v-if="p.large">
									<td :class="$style.message"><code>{{ p.model }}</code><span :class="$style.note"> (入力が{{ p.largeThreshold }}tokenを超える)</span></td>
									<td>{{ p.large.input }}</td><td>{{ p.large.cacheWrite }}</td><td>{{ p.large.cacheRead }}</td><td>{{ p.large.output }}</td>
								</tr>
							</template>
						</tbody>
					</table>
				</div>

				<div :class="$style.label">単価の上書き (表より優先します。「設定を保存」で保存)</div>
				<div v-for="(o, i) in draft.priceOverrides" :key="i" :class="$style.override">
					<MkInput v-model="o.model" type="text" small><template #label>モデル</template></MkInput>
					<MkInput v-model="o.input" type="number" small step="0.001"><template #label>入力</template></MkInput>
					<MkInput v-model="o.cacheWrite" type="number" small step="0.001"><template #label>キャッシュ書込</template></MkInput>
					<MkInput v-model="o.cacheRead" type="number" small step="0.001"><template #label>キャッシュ読込</template></MkInput>
					<MkInput v-model="o.output" type="number" small step="0.001"><template #label>出力</template></MkInput>
					<MkButton inline @click="draft.priceOverrides.splice(i, 1)">削除</MkButton>
				</div>
				<div class="_buttons">
					<MkButton inline @click="addOverride">上書きを足す</MkButton>
				</div>
			</div>
		</MkFolder>

		<!-- 記録 -->
		<MkFolder :defaultOpen="false">
			<template #label>記録</template>
			<template #suffix>{{ state.events.length }} 件</template>
			<div :class="$style.scroll">
				<div v-if="state.events.length === 0" :class="$style.note">記録はありません。</div>
				<table v-else :class="$style.table">
					<thead><tr><th>日時</th><th>種類</th><th>内容</th></tr></thead>
					<tbody>
						<tr v-for="(ev, i) in state.events" :key="i">
							<td>{{ new Date(ev.at).toLocaleString() }}</td>
							<td :class="ev.level === 'error' ? $style.bad : undefined">{{ kindLabel(ev.kind) }}</td>
							<td :class="$style.message">{{ ev.message }}<span v-if="ev.noteId" :class="$style.note"> (投稿 {{ ev.noteId }})</span></td>
						</tr>
					</tbody>
				</table>
			</div>
		</MkFolder>
	</template>

	<MkLoading v-else/>
</div>
</template>

<script lang="ts" setup>
import { ref, computed, onMounted } from 'vue';
import { MkInput, MkButton, MkFolder, MkLoading, MkSelect, MkSwitch, MkPluginSecrets } from '@/plugin-api.js';
import { api, autoMaxTokens, errorMessage, readAsBase64, totalTokens, usd } from './api.js';
import type { PeriodUsage, Settings, State } from './api.js';

const state = ref<State | null>(null);
const draft = ref<Settings | null>(null);
const loadError = ref('');
const saveError = ref('');
const saved = ref(false);
const busy = ref(false);

const newUsername = ref('');
const newName = ref('');
const profileName = ref('');
const profileDescription = ref('');
const avatarFile = ref<File | null>(null);
const deleteConfirm = ref('');
const scheduledTimes = ref('');

type Item = { label: string; value: string };
const effortItems: Item[] = [
	{ label: '送らない (モデルの既定)', value: '' },
	{ label: 'low', value: 'low' },
	{ label: 'medium', value: 'medium' },
	{ label: 'high', value: 'high' },
	{ label: 'xhigh', value: 'xhigh' },
	{ label: 'max', value: 'max' },
];
const modeItems: Item[] = [
	{ label: '返事だけ', value: 'reply' },
	{ label: 'リアクションだけ', value: 'reaction' },
	{ label: '返事とリアクション', value: 'both' },
	{ label: '反応しない', value: 'off' },
];
const audienceItems: Item[] = [
	{ label: '誰でも', value: 'everyone' },
	{ label: 'botのフォロワーだけ', value: 'followers' },
	{ label: 'このサーバーの人だけ', value: 'local' },
];
const visibilityItems: Item[] = [
	{ label: 'パブリック', value: 'public' },
	{ label: 'ホーム', value: 'home' },
	{ label: 'フォロワー', value: 'followers' },
];
const overLimitItems: Item[] = [
	{ label: '何もしない (沈黙)', value: 'silent' },
	{ label: '決まった文で断る', value: 'message' },
];

const kindLabels: Record<string, string> = {
	api_error: 'APIのエラー',
	silenced: '沈黙',
	refused: '断った',
	skipped: '反応しない',
	post_error: '投稿の失敗',
	config: '設定',
	reaction_error: 'リアクションの失敗',
	image: '画像',
};
function kindLabel(kind: string): string {
	return kindLabels[kind] ?? kind;
}

const modelIds = computed(() => state.value?.prices.map(p => p.model) ?? []);
const periods = computed(() => state.value ? [
	{ label: '今日', usage: state.value.usage.today },
	{ label: '今月', usage: state.value.usage.month },
] : []);

const usageRows = [
	{ label: '回数', value: (u: PeriodUsage) => String(u.calls) },
	{ label: '入力', value: (u: PeriodUsage) => String(u.tokens.inputTokens) },
	{ label: '出力', value: (u: PeriodUsage) => String(u.tokens.outputTokens) },
	{ label: 'キャッシュ書込', value: (u: PeriodUsage) => String(u.tokens.cacheCreationTokens) },
	{ label: 'キャッシュ読込', value: (u: PeriodUsage) => String(u.tokens.cacheReadTokens) },
	{ label: '概算の額', value: (u: PeriodUsage) => usd(u.costUsd) },
];

// keepDraft が true なら、編集中の設定を残す。プロフィールやアカウントの
// 操作で、保存していない設定の変更を消さないため。
function apply(s: State, keepDraft = false) {
	state.value = s;
	if (!keepDraft || draft.value == null) {
		// 編集中の値は、保存するまでサーバーの値と分けておく。
		draft.value = JSON.parse(JSON.stringify(s.settings)) as Settings;
		scheduledTimes.value = s.settings.scheduled.times.join(', ');
	}
	profileName.value = s.account?.name ?? '';
	profileDescription.value = s.account?.description ?? '';
}

async function load() {
	try {
		apply(await api<State>('admin/state'));
	} catch (err) {
		loadError.value = errorMessage(err);
	}
}

async function run(fn: () => Promise<State>, keepDraft = false) {
	busy.value = true;
	saveError.value = '';
	saved.value = false;
	try {
		apply(await fn(), keepDraft);
		saved.value = true;
	} catch (err) {
		saveError.value = errorMessage(err);
	} finally {
		busy.value = false;
	}
}

function finite(v: unknown): number {
	const n = Number(v);
	return Number.isFinite(n) ? n : 0;
}

function saveSettings() {
	const d = draft.value;
	if (!d) return;
	d.scheduled.times = scheduledTimes.value.split(/[,\s]+/).filter(t => t !== '');
	// MkInputのnumberは空欄でnullやNaNになることがある。NaNはJSONでnullになり、
	// サーバーは今の設定の上に重ねて保存するので、nullだと前の値が黙って残る。
	// 有限でない値はすべて0に寄せる(範囲外ならサーバーの検証で弾かれる)。
	const r = d.reply;
	const sc = d.scheduled;
	const l = d.limits;
	const b = d.budget;
	const v = d.vision;
	r.maxChars = finite(r.maxChars);
	r.maxTokens = finite(r.maxTokens);
	r.contextNotes = finite(r.contextNotes);
	r.maxRoundTrips = finite(r.maxRoundTrips);
	v.maxImages = finite(v.maxImages);
	sc.intervalMinutes = finite(sc.intervalMinutes);
	sc.maxChars = finite(sc.maxChars);
	sc.maxTokens = finite(sc.maxTokens);
	l.perUserPerHour = finite(l.perUserPerHour);
	l.globalPerDay = finite(l.globalPerDay);
	l.perHostPerDay = finite(l.perHostPerDay);
	l.monthlyTokens = finite(l.monthlyTokens);
	b.monthlyUsd = finite(b.monthlyUsd);
	b.warnPercent = finite(b.warnPercent);
	for (const o of d.priceOverrides) {
		o.input = finite(o.input);
		o.cacheWrite = finite(o.cacheWrite);
		o.cacheRead = finite(o.cacheRead);
		o.output = finite(o.output);
	}
	return run(() => api<State>('admin/settings', { settings: d }));
}

function addOverride() {
	draft.value?.priceOverrides.push({ model: '', input: 0, cacheWrite: 0, cacheRead: 0, output: 0 });
}

function createAccount() {
	return run(() => api<State>('admin/account/create', { username: newUsername.value, name: newName.value }), true);
}

function onAvatar(ev: Event) {
	avatarFile.value = (ev.target as HTMLInputElement).files?.[0] ?? null;
}

async function updateProfile() {
	const params: Record<string, unknown> = {
		name: profileName.value,
		description: profileDescription.value,
	};
	if (avatarFile.value) {
		params.avatar = { data: await readAsBase64(avatarFile.value), filename: avatarFile.value.name };
	}
	return run(() => api<State>('admin/account/update', params), true);
}

function deleteAccount() {
	return run(() => api<State>('admin/account/delete', { username: deleteConfirm.value }), true);
}

onMounted(load);
</script>

<style lang="scss" module>
.root {
	padding: 16px;
}

.label {
	font-size: 0.85em;
	padding: 0 0 8px 0;
}

.textarea {
	width: 100%;
	box-sizing: border-box;
	padding: 8px;
	font: inherit;
	color: inherit;
	background: var(--MI_THEME-panel);
	border: solid 1px var(--MI_THEME-divider);
	border-radius: 6px;
	resize: vertical;
}

.note {
	font-size: 0.85em;
	opacity: 0.8;
}

.error {
	color: var(--MI_THEME-error);
	overflow-wrap: anywhere;
}

.warn {
	color: var(--MI_THEME-warn);
	overflow-wrap: anywhere;
}

.ok {
	color: var(--MI_THEME-success);
}

.bad {
	color: var(--MI_THEME-error);
}

.account {
	display: flex;
	gap: 12px;
	align-items: center;
}

// flex の子は既定で中身より縮まないので、空白の無い長い名前で横にはみ出さないようにする。
.accountBody {
	min-width: 0;
	overflow-wrap: anywhere;
}

.avatar {
	width: 48px;
	height: 48px;
	border-radius: 50%;
	object-fit: cover;
}

.override {
	display: flex;
	gap: 8px;
	flex-wrap: wrap;
	align-items: flex-end;
}

.table {
	width: 100%;
	border-collapse: collapse;
	font-size: 0.9em;

	th, td {
		text-align: left;
		padding: 6px 10px;
		border-bottom: 1px solid var(--MI_THEME-divider);
		white-space: nowrap;
	}
}

.message {
	white-space: normal !important;
}

// 表が画面の幅を超えるときは、ページ全体ではなく表の中だけを横に動かす。
.scroll {
	overflow-x: auto;
}
</style>
