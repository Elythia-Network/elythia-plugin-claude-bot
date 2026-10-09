/*
 * SPDX-FileCopyrightText: Elythia project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { definePlugin } from '@/plugin-api.js';
import { initApi } from './api.js';
import AdminPage from './AdminPage.vue';

export default definePlugin({
	name: 'claude-bot',

	// ページは setup ではなくここで宣言する。ルーターはモジュール読み込み時に
	// 現在の URL を解決するので、setup で登録すると直接開いたときに 404 になる。
	pages: [
		{
			path: '/',
			component: AdminPage,
			admin: true,
			navTitle: 'Claude bot',
			navIcon: 'ti ti-robot',
		},
	],

	setup(host) {
		initApi(host.api);
	},
});
