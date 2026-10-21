/*
 * SPDX-FileCopyrightText: Elythia project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

let call: <T>(endpoint: string, params?: Record<string, unknown>) => Promise<T>;

/** Called once from the plugin's setup. */
export function initApi(fn: typeof call): void {
	call = fn;
}

export function api<T>(path: string, params: Record<string, unknown> = {}): Promise<T> {
	return call<T>(`plugin/claude-bot/${path}`, params);
}

export type Price = {
	input: number;
	cacheWrite: number;
	cacheRead: number;
	output: number;
};

export type PriceOverride = Price & { model: string };

export type Settings = {
	model: string;
	effort: string;
	timezone: string;
	reply: {
		mode: string;
		systemPrompt: string;
		maxChars: number;
		maxTokens: number;
		visibility: string;
		audience: string;
		contextNotes: number;
		maxRoundTrips: number;
	};
	scheduled: {
		enabled: boolean;
		systemPrompt: string;
		maxChars: number;
		maxTokens: number;
		visibility: string;
		times: string[];
		intervalMinutes: number;
	};
	limits: {
		perUserPerHour: number;
		globalPerDay: number;
		perHostPerDay: number;
		monthlyTokens: number;
		overLimit: string;
		overLimitMessage: string;
	};
	budget: {
		monthlyUsd: number;
		warnPercent: number;
		stopWhenExhausted: boolean;
	};
	vision: {
		enabled: boolean;
		maxImages: number;
		includeThread: boolean;
		includeSensitive: boolean;
	};
	web: {
		search: boolean;
		fetch: boolean;
		audience: string;
		users: string[];
		perUserPerHour: number;
		globalPerHour: number;
		maxUsesPerReply: number;
		blockedDomains: string[];
		citations: boolean;
		maxContentTokens: number;
	};
	priceOverrides: PriceOverride[];
};

export type Tokens = {
	inputTokens: number;
	outputTokens: number;
	cacheCreationTokens: number;
	cacheReadTokens: number;
};

export type PeriodUsage = {
	calls: number;
	tokens: Tokens;
	webSearchRequests: number;
	webFetchRequests: number;
	costUsd: number;
	byModel: { model: string; calls: number; tokens: Tokens; costUsd: number | null }[];
	unpriced: string[];
};

export type BudgetStatus = {
	monthlyUsd: number;
	spentUsd: number;
	remainingUsd: number;
	warning: boolean;
	exhausted: boolean;
	underestimated: boolean;
};

export type BotEvent = {
	at: string;
	level: string;
	kind: string;
	message: string;
	userId?: string;
	noteId?: string;
};

/** Response of admin/events (administrators only). */
export type EventsResponse = {
	events: BotEvent[];
};

export type PriceRow = Price & {
	model: string;
	large?: Price;
	largeThreshold?: number;
	dated: boolean;
};

export type State = {
	account: {
		id: string;
		username: string;
		name: string | null;
		description: string | null;
		avatarUrl: string | null;
		isBot: boolean;
	} | null;
	settings: Settings;
	maxNoteTextLength: number;
	usage: { today: PeriodUsage; month: PeriodUsage; budget: BudgetStatus | null };
	prices: PriceRow[];
	defaultDescription: string;
	/** 管理者なら true。モデレーターは見るだけで、記録 (admin/events) は読めない。 */
	canEdit: boolean;
	/** 保存はできたが気を付けること (単価の表に無いモデルなど)。 */
	warnings: string[];
};

export function totalTokens(t: Tokens): number {
	return t.inputTokens + t.outputTokens + t.cacheCreationTokens + t.cacheReadTokens;
}

/** Splits a list typed in a text box (commas, spaces or new lines). */
export function splitList(v: string): string[] {
	return v.split(/[,\s]+/).filter(t => t !== '');
}

/**
 * Estimates the input tokens the fetched pages of one reply can take, at
 * most: each page is read again by every later step of the server-side loop,
 * so n pages of t tokens add up to t * n * (n + 1) / 2.
 */
export function webFetchTokensEstimate(fetches: number, maxContentTokens: number): number {
	return maxContentTokens * fetches * (fetches + 1) / 2;
}

/**
 * The most fetches one reply can make. web.go の splitWebUses と同じ分け方
 * (両方を使うときは、URLがあれば取得に多めに分ける)。
 */
export function maxFetchesPerReply(search: boolean, fetch: boolean, maxUses: number): number {
	if (!fetch) return 0;
	return search ? Math.ceil(maxUses / 2) : maxUses;
}

export function usd(v: number): string {
	return `$${v.toFixed(v < 1 ? 4 : 2)}`;
}

// claude.go の thinkingHeadroom と同じ値。effort を上げるほど思考に使う
// token が増えるので、余裕も広げる。空 (送らない) はモデルの既定の effort
// (多くは high) で考えるので、high と同じにする。
const thinkingHeadroom: Record<string, number> = {
	'': 8192,
	low: 1024,
	medium: 4096,
	high: 8192,
	xhigh: 16384,
	max: 16384,
};

/** Mirrors autoMaxTokens in claude.go. */
export function autoMaxTokens(chars: number, effort: string): number {
	return Math.min(chars * 3 + (thinkingHeadroom[effort] ?? 1024), 32000);
}

export function errorMessage(err: unknown): string {
	return (err as { message?: string } | null)?.message ?? '失敗しました';
}

/** Reads a file as base64 (without the data: prefix). */
export function readAsBase64(file: Blob): Promise<string> {
	return new Promise((resolve, reject) => {
		const reader = new FileReader();
		reader.onload = () => {
			const s = String(reader.result);
			resolve(s.slice(s.indexOf(',') + 1));
		};
		reader.onerror = () => reject(reader.error);
		reader.readAsDataURL(file);
	});
}

/**
 * The largest avatar the server accepts, in bytes. Same as maxAvatarBytes in
 * routes.go.
 */
export const maxAvatarBytes = 700 * 1024;

// 縮めるときの長辺。アイコンとして表示する大きさには十分足りる。
const avatarEdge = 512;

/**
 * Returns the avatar to send: the file itself when it is small enough, or a
 * copy scaled down to fit maxAvatarBytes.
 */
export async function fitAvatar(file: File): Promise<{ blob: Blob; filename: string }> {
	// 本体は /api の body を1MiBで切る。base64は4/3倍になるので、上限を超える
	// 画像はそのまま送ると、名前や自己紹介ごと413で弾かれる (#9)。上限以下の
	// ものはGIFの動きを残すため、そのまま送る。
	if (file.size <= maxAvatarBytes) return { blob: file, filename: file.name };
	let bitmap: ImageBitmap;
	try {
		bitmap = await createImageBitmap(file);
	} catch {
		throw new Error('アイコンの画像を読めません');
	}
	try {
		const scale = Math.min(1, avatarEdge / Math.max(bitmap.width, bitmap.height));
		const canvas = window.document.createElement('canvas');
		canvas.width = Math.max(1, Math.round(bitmap.width * scale));
		canvas.height = Math.max(1, Math.round(bitmap.height * scale));
		const g = canvas.getContext('2d');
		if (!g) throw new Error('アイコンの画像を縮められません');
		g.imageSmoothingQuality = 'high';
		g.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
		// WebPを書けないブラウザは、黙ってPNGを返す。名前は実際の形式から付ける。
		for (const type of ['image/webp', 'image/jpeg']) {
			const blob = await new Promise<Blob | null>(resolve => canvas.toBlob(resolve, type, 0.9));
			if (blob && blob.size <= maxAvatarBytes) {
				const ext = blob.type === 'image/webp' ? 'webp' : blob.type === 'image/jpeg' ? 'jpg' : 'png';
				return { blob, filename: `${file.name.replace(/\.[^.]*$/, '')}.${ext}` };
			}
		}
	} finally {
		bitmap.close();
	}
	throw new Error(`アイコンの画像を${maxAvatarBytes / 1024}KB以下に縮められません`);
}
