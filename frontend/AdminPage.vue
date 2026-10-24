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
					<div v-if="draft.web.search || draft.web.fetch" :class="$style.warn">「webを使う」が有効なので、話しかけた内容をもとにwebを検索・取得することを書いてください。</div>
					<div v-if="draft.memory.enabled" :class="$style.warn">「会話を覚える」が有効なので、やり取りを保存して以後の返事に使うことを書いてください。</div>
				</div>
				<div>
					<div :class="$style.label">アイコン</div>
					<input ref="avatarInput" type="file" accept="image/*" :disabled="busy" @change="onAvatar"/>
					<MkButton v-if="avatarFile" inline small :disabled="busy" @click="clearAvatar">選択を外す</MkButton>
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

		<!-- webを使う -->
		<MkFolder :defaultOpen="false">
			<template #label>webを使う</template>
			<template #suffix>{{ draft.web.search || draft.web.fetch ? '有効' : '無効' }}</template>
			<div class="_gaps_s">
				<MkSwitch v-model="draft.web.search">webを検索する (web search)</MkSwitch>
				<MkSwitch v-model="draft.web.fetch">webページを取得する (web fetch)</MkSwitch>
				<div v-if="draft.web.search || draft.web.fetch" :class="$style.warn">
					話しかけた内容をもとに、Anthropic社のClaude APIがwebを検索・取得します。botの自己紹介の文も書き換えてください。
				</div>
				<div :class="$style.note">
					検索は1回0.01ドル掛かり、取得したページの本文は入力のtokenとして数えます。取得できるのは、その返事の会話に既に出てきたURL(話しかけた投稿とスレッドの本文、検索の結果、前に取得したページの本文にあるもの)だけです。1回の返事でwebを使う時間は3分までです。使えない人や上限を超えたときは、webを使わずに返事をします。定時の投稿では使いません。
				</div>
				<MkSelect v-model="draft.web.audience" :items="webAudienceItems">
					<template #label>使える人</template>
				</MkSelect>
				<MkInput v-model="webUsers" type="text" placeholder="@alice, @bob@remote.example">
					<template #label>ほかに使える人 (カンマ区切り)</template>
					<template #caption>上の範囲に加えて使える人です。このサーバーの人は@名前(このサーバーのホストを付けた@名前@ホストは一致しません)、リモートの人は@名前@ホストで書きます。国際化ドメインのホストは xn-- で始まる形で書きます。</template>
				</MkInput>
				<MkInput v-model="draft.web.perUserPerHour" type="number" :min="0">
					<template #label>1人あたりの回数 (1時間、0で上限なし)</template>
				</MkInput>
				<MkInput v-model="draft.web.globalPerHour" type="number" :min="0">
					<template #label>全体の回数 (1時間、0で上限なし)</template>
					<template #caption>回数は、Claude APIが実際に行った検索と取得の回数で数えます(課金の単位と同じ)。</template>
				</MkInput>
				<MkInput v-model="draft.web.maxUsesPerReply" type="number" :min="1" :max="10">
					<template #label>1回の返事で使える回数</template>
					<template #caption>1〜10。検索と取得の合計です。両方を使うときは、この回数を検索と取得に分けて渡します。</template>
				</MkInput>
				<MkInput v-model="webBlockedDomains" type="text" placeholder="example.com">
					<template #label>除くドメイン (カンマ区切り)</template>
					<template #caption>検索の結果と取得から外します。スキームやパスは付けずに書きます。</template>
				</MkInput>
				<MkInput v-model="draft.web.maxContentTokens" type="number" :min="1000" :max="100000">
					<template #label>取得したページの本文のtoken数の上限 (1ページ)</template>
					<template #caption>1000〜100000。長いページは、ここで切ってから読みます。</template>
				</MkInput>
				<div v-if="draft.web.fetch" :class="$style.note">
					取得の費用の目安(1回の返事の最大): 取得 {{ webFetches }} 回で、入力 約{{ webFetchTokens.toLocaleString() }} token<template v-if="webFetchUsd != null">(約{{ usd(webFetchUsd) }}、{{ draft.model }}の入力の単価で計算)</template>。取得したページの本文は、その後のサーバー側の繰り返しのたびに入力として読み直されるので、取得の回数の2乗に近い形で増えます。pause_turnの続きやwebを外した呼び直しでは、さらに読み直します。
				</div>
				<MkSwitch v-model="draft.web.citations">返事に出典のURLを付ける</MkSwitch>
				<div :class="$style.note">付けるのは最大3件で、返事の長さとは別に数えます。投稿の文字数の上限に収まらない分は付けません。</div>
			</div>
		</MkFolder>

		<!-- 会話を覚える -->
		<MkFolder :defaultOpen="false">
			<template #label>会話を覚える</template>
			<template #suffix>{{ draft.memory.enabled ? '有効' : '無効' }}</template>
			<div class="_gaps_s">
				<MkSwitch v-model="draft.memory.enabled">やり取りを覚えて、同じ相手への次の返事に使う</MkSwitch>
				<div v-if="draft.memory.enabled" :class="$style.warn">
					話しかけた人の投稿とbotの返事を、このサーバーに保存します。botの自己紹介の文も書き換えてください。
				</div>
				<div :class="$style.note">
					覚えるのは、返事の公開範囲がパブリックかホームで、連合しない設定でないやり取りだけです。覚えた内容は、その人自身への返事にだけ使います。投稿が消されても、覚えた内容は消えません(下の「覚えている会話」から消せます)。
				</div>
				<MkSelect v-model="draft.memory.audience" :items="memoryAudienceItems">
					<template #label>覚える相手</template>
				</MkSelect>
				<MkInput v-model="draft.memory.keepRecent" type="number" :min="1" :max="20">
					<template #label>覚えておくやり取りの数</template>
					<template #caption>1〜20。新しい方から、この数だけを残して返事に渡します。1件は相手の投稿とbotの返事をそれぞれ1000文字までにして残すので、数を増やすほど返事の入力のtokenが増えます。</template>
				</MkInput>
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
				<MkSwitch v-model="draft.budget.stopWhenExhausted">
					<template #label>予算を使い切ったら返事と定時の投稿を止める</template>
					<template #caption>単価が未設定のモデルは額に入らないので、予算では止まりません。そのモデルで予算を使うときは、単価を上書きするか、1か月のtokenの量に上限を入れてください (どちらも無いと保存できません)。</template>
				</MkSwitch>
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
				<div :class="$style.note">額はtoken数 × 単価に、webの検索の料金(1回0.01ドル)を足した概算です。実際の請求とは異なることがあります。</div>
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

		<!-- 覚えている会話。話しかけた人の投稿の本文が入るので、管理者にだけ出す。 -->
		<MkFolder v-if="state.canEdit" :defaultOpen="false">
			<template #label>覚えている会話</template>
			<template #suffix>{{ memoriesTotal }} 人</template>
			<div class="_gaps_s">
				<div v-if="memoriesError" :class="$style.error">{{ memoriesError }}</div>
				<div v-if="memories.length === 0" :class="$style.note">覚えている会話はありません。</div>
				<template v-else>
					<div v-if="memoriesTotal > memories.length" :class="$style.note">新しい方から{{ memories.length }}人を出しています。</div>
					<MkFolder v-for="m in memories" :key="m.userId" :defaultOpen="false" @opened="openMemory(m.userId)" @closed="openMemories.delete(m.userId)">
						<template #label>{{ m.acct }}</template>
						<template #suffix>{{ m.count }} 件</template>
						<div class="_gaps_s">
							<div v-if="memoryErrors[m.userId]" :class="$style.error">{{ memoryErrors[m.userId] }}</div>
							<MkLoading v-else-if="!memoryDetails[m.userId]"/>
							<div v-for="(ex, i) in memoryDetails[m.userId] ?? []" :key="i" :class="$style.message">
								<div :class="$style.note">{{ new Date(ex.at).toLocaleString() }} (投稿 {{ ex.noteId }}<template v-if="ex.replyId">、返事 {{ ex.replyId }}</template>)</div>
								<div :class="$style.pre">相手: {{ ex.user }}</div>
								<div :class="$style.pre">bot: {{ ex.bot }}</div>
							</div>
							<div class="_buttons">
								<MkButton inline danger :disabled="busy" @click="forgetMemory(m.userId)">この人の記憶を消す</MkButton>
							</div>
						</div>
					</MkFolder>
					<div v-if="!confirmForgetAll" class="_buttons">
						<MkButton danger :disabled="busy" @click="confirmForgetAll = true">全員の記憶を消す</MkButton>
					</div>
					<template v-else>
						<div :class="$style.warn">{{ memoriesTotal }}人分の記憶をすべて消します。元に戻せません。</div>
						<div class="_buttons">
							<MkButton danger :disabled="busy" @click="forgetAllMemories">消す</MkButton>
							<MkButton :disabled="busy" @click="confirmForgetAll = false">やめる</MkButton>
						</div>
					</template>
				</template>
			</div>
		</MkFolder>

		<!-- 記録。話しかけた人のIDやAPIのエラーの本文が入るので、管理者にだけ出す。 -->
		<MkFolder v-if="state.canEdit" :defaultOpen="false">
			<template #label>記録</template>
			<template #suffix>{{ events.length }} 件</template>
			<div :class="$style.scroll">
				<div v-if="eventsError" :class="$style.error">{{ eventsError }}</div>
				<div v-else-if="events.length === 0" :class="$style.note">記録はありません。</div>
				<table v-else :class="$style.table">
					<thead><tr><th>日時</th><th>種類</th><th>内容</th></tr></thead>
					<tbody>
						<tr v-for="(ev, i) in events" :key="i">
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
import { api, autoMaxTokens, errorMessage, fitAvatar, maxFetchesPerReply, readAsBase64, splitList, totalTokens, usd, webFetchTokensEstimate } from './api.js';
import type { BotEvent, EventsResponse, MemoriesResponse, MemoryExchange, MemoryResponse, MemorySummary, PeriodUsage, Settings, State } from './api.js';

const state = ref<State | null>(null);
const events = ref<BotEvent[]>([]);
const eventsError = ref('');
const memories = ref<MemorySummary[]>([]);
const memoriesTotal = ref(0);
const memoriesError = ref('');
const confirmForgetAll = ref(false);
// 開いた人のやり取りの本文。一覧には件数しか来ないので、開いたときに読む。
const memoryDetails = ref<Record<string, MemoryExchange[]>>({});
const memoryErrors = ref<Record<string, string>>({});
// 開いている人。一覧を読み直したときに、開いている人の本文だけを読み直す。
const openMemories = new Set<string>();
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
const avatarInput = ref<HTMLInputElement | null>(null);
const deleteConfirm = ref('');
const scheduledTimes = ref('');
const webUsers = ref('');
const webBlockedDomains = ref('');

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
const webAudienceItems: Item[] = [
	{ label: '一覧の人だけ', value: 'none' },
	{ label: 'このサーバーの人', value: 'local' },
	{ label: 'botのフォロワー', value: 'followers' },
	{ label: '誰でも', value: 'everyone' },
];
const memoryAudienceItems: Item[] = [
	{ label: 'このサーバーの人だけ', value: 'local' },
	{ label: '話しかけられる人すべて', value: 'everyone' },
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
	prompt_leak: 'プロンプトの写し',
	web: 'web',
	memory: '記憶',
};

function kindLabel(kind: string): string {
	return kindLabels[kind] ?? kind;
}

const webFetches = computed(() => {
	const w = draft.value?.web;
	return w ? maxFetchesPerReply(w.search, w.fetch, Number(w.maxUsesPerReply) || 0) : 0;
});
const webFetchTokens = computed(() => webFetchTokensEstimate(webFetches.value, Number(draft.value?.web.maxContentTokens) || 0));
// 入力の単価は、上書きがあればそれを、無ければ表の値を使う (段階のあるモデルは
// 小さい方の単価)。単価が分からなければ額は出さない。
const webFetchUsd = computed(() => {
	const d = draft.value;
	if (!d || !state.value) return null;
	const price = d.priceOverrides.find(o => o.model === d.model) ?? state.value.prices.find(p => p.model === d.model);
	return price ? webFetchTokens.value * Number(price.input) / 1_000_000 : null;
});
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
	{ label: 'webの検索', value: (u: PeriodUsage) => String(u.webSearchRequests) },
	{ label: 'webページの取得', value: (u: PeriodUsage) => String(u.webFetchRequests) },
	{ label: '概算の額', value: (u: PeriodUsage) => usd(u.costUsd) },
];

// 記録は管理者だけが読める別のルートにある。モデレーターには取りに行かない。
async function loadEvents() {
	try {
		events.value = (await api<EventsResponse>('admin/events')).events;
		eventsError.value = '';
	} catch (err) {
		eventsError.value = errorMessage(err);
	}
}

// 覚えている会話も、記録と同じく管理者だけが読める別のルートにある。
function applyMemories(res: MemoriesResponse) {
	memories.value = res.memories;
	memoriesTotal.value = res.total;
	memoriesError.value = '';
	// 一覧を読み直したら、開いている人の本文も読み直す (件数と食い違わないように)。
	// 折り畳みは開いたままなので、opened は再び来ない。消えた人の分は捨てる。
	memoryDetails.value = {};
	memoryErrors.value = {};
	for (const userId of [...openMemories]) {
		if (listed(userId)) void loadMemory(userId);
		else openMemories.delete(userId);
	}
}

function listed(userId: string): boolean {
	return memories.value.some(m => m.userId === userId);
}

function openMemory(userId: string) {
	openMemories.add(userId);
	void loadMemory(userId);
}

async function loadMemory(userId: string) {
	try {
		const res = await api<MemoryResponse>('admin/memories/show', { userId });
		// 読んでいる間に消した人の本文は、遅れて届いても書き戻さない。
		if (!listed(userId)) return;
		memoryDetails.value = { ...memoryDetails.value, [userId]: res.exchanges };
		const errors = { ...memoryErrors.value };
		delete errors[userId];
		memoryErrors.value = errors;
	} catch (err) {
		if (!listed(userId)) return;
		memoryErrors.value = { ...memoryErrors.value, [userId]: errorMessage(err) };
	}
}

async function loadMemories() {
	try {
		applyMemories(await api<MemoriesResponse>('admin/memories'));
	} catch (err) {
		memoriesError.value = errorMessage(err);
	}
}

// 失敗は「覚えている会話」の中に出す。設定の保存のエラーの欄は、この折り畳みより
// 上にあって見えないため。
async function deleteMemories(params: Record<string, unknown>) {
	busy.value = true;
	memoriesError.value = '';
	try {
		applyMemories(await api<MemoriesResponse>('admin/memories/delete', params));
	} catch (err) {
		memoriesError.value = errorMessage(err);
	} finally {
		busy.value = false;
	}
}

function forgetMemory(userId: string) {
	return deleteMemories({ userId });
}

async function forgetAllMemories() {
	await deleteMemories({ all: true });
	confirmForgetAll.value = false;
}

// keepDraft が true なら、編集中の設定を残す。プロフィールやアカウントの
// 操作で、保存していない設定の変更を消さないため。
function apply(s: State, keepDraft = false) {
	// アカウントを削除して作り直すと、file inputは空で作り直されるが、選んだ
	// ファイルのrefは残る。見えない画像を送らないよう、ここで外す。
	if (s.account?.id !== state.value?.account?.id) clearAvatar();
	state.value = s;
	if (!keepDraft || draft.value == null) {
		// 編集中の値は、保存するまでサーバーの値と分けておく。
		draft.value = JSON.parse(JSON.stringify(s.settings)) as Settings;
		scheduledTimes.value = s.settings.scheduled.times.join(', ');
		webUsers.value = s.settings.web.users.join(', ');
		webBlockedDomains.value = s.settings.web.blockedDomains.join(', ');
	}
	profileName.value = s.account?.name ?? '';
	profileDescription.value = s.account?.description ?? '';
	if (s.canEdit) {
		void loadEvents();
		void loadMemories();
	}
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
	d.scheduled.times = splitList(scheduledTimes.value);
	d.web.users = splitList(webUsers.value);
	d.web.blockedDomains = splitList(webBlockedDomains.value);
	// MkInputのnumberは空欄でnullやNaNになることがある。NaNはJSONでnullになり、
	// サーバーは今の設定の上に重ねて保存するので、nullだと前の値が黙って残る。
	// 有限でない値はすべて0に寄せる(範囲外ならサーバーの検証で弾かれる)。
	const r = d.reply;
	const sc = d.scheduled;
	const l = d.limits;
	const b = d.budget;
	const v = d.vision;
	const w = d.web;
	r.maxChars = finite(r.maxChars);
	r.maxTokens = finite(r.maxTokens);
	r.contextNotes = finite(r.contextNotes);
	r.maxRoundTrips = finite(r.maxRoundTrips);
	v.maxImages = finite(v.maxImages);
	d.memory.keepRecent = finite(d.memory.keepRecent);
	w.perUserPerHour = finite(w.perUserPerHour);
	w.globalPerHour = finite(w.globalPerHour);
	w.maxUsesPerReply = finite(w.maxUsesPerReply);
	w.maxContentTokens = finite(w.maxContentTokens);
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

function clearAvatar() {
	avatarFile.value = null;
	if (avatarInput.value) avatarInput.value.value = '';
}

async function updateProfile() {
	const file = avatarFile.value;
	await run(async () => {
		const params: Record<string, unknown> = {
			name: profileName.value,
			description: profileDescription.value,
		};
		if (file) {
			try {
				const { blob, filename } = await fitAvatar(file);
				params.avatar = { data: await readAsBase64(blob), filename };
			} catch (err) {
				// 選択を残すと、名前だけ直して保存し直しても同じ画像で失敗し続ける。
				// 名前と自己紹介もまだ送っていないことを、文で伝える。
				clearAvatar();
				throw new Error(`${errorMessage(err)}。画像の選択を外したので、もう一度保存してください`);
			}
		}
		return api<State>('admin/account/update', params);
	}, true);
	// 選んだファイルを残すと、名前だけ直して保存し直したときにも同じ画像を
	// 送ってしまう (#9)。
	if (saved.value && avatarFile.value === file) clearAvatar();
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

// 投稿の本文の改行を残す。
.pre {
	white-space: pre-wrap;
	overflow-wrap: anywhere;
}

// 表が画面の幅を超えるときは、ページ全体ではなく表の中だけを横に動かす。
.scroll {
	overflow-x: auto;
}
</style>
