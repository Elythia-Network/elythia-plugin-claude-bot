#!/usr/bin/env bash
# Generates the files that build Elythia together with this plugin. Run it in
# the Elythia checkout, with this plugin placed at plugins/$PLUGIN_NAME.
#
# pluginbuild が go.work と、組み込み用の生成物 (cmd/elythia/plugins_generated.go、
# frontend の server-plugins.generated.ts) を作る。
set -euo pipefail

: "${PLUGIN_NAME:?PLUGIN_NAME is required}"
[ -f "plugins/$PLUGIN_NAME/go.mod" ] || { echo "plugins/$PLUGIN_NAME/go.mod not found (run this in the Elythia checkout)" >&2; exit 1; }

GOWORK=off go run ./tools/pluginbuild

# **go.work の go の版を、このプラグインの go.mod に合わせる。** pluginbuild は
# 本体の go.mod の版で go.work を書く。プラグインがそれより新しい版を求めると
# (2.0.0 の本体は go 1.27.1)、`go.work lists go 1.27.1` で何も組めない。
gover=$(awk '/^go [0-9]/ {print $2; exit}' "plugins/$PLUGIN_NAME/go.mod")
go work edit -go="$gover"

# 組み込む対象に、このプラグインが入っていることを確かめる。入っていないと、
# 以降の build が本体だけを組んで緑になる。
grep -q "plugins/$PLUGIN_NAME\"\?\$" go.work || { echo "go.work does not use plugins/$PLUGIN_NAME" >&2; cat go.work >&2; exit 1; }
grep -q "elythia-plugin-$PLUGIN_NAME\"" cmd/elythia/plugins_generated.go || { echo "cmd/elythia/plugins_generated.go does not import this plugin" >&2; exit 1; }
