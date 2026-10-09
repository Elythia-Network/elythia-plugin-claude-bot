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
	events: BotEvent[];
	defaultDescription: string;
	/** 管理者なら true。モデレーターは見るだけ。 */
	canEdit: boolean;
	/** 保存はできたが気を付けること (単価の表に無いモデルなど)。 */
	warnings: string[];
};

export function totalTokens(t: Tokens): number {
	return t.inputTokens + t.outputTokens + t.cacheCreationTokens + t.cacheReadTokens;
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
export function readAsBase64(file: File): Promise<string> {
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
