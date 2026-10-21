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

# **go.work の go の版を、本体とこのプラグインの go.mod の新しい方にする。**
# pluginbuild は本体の go.mod の版で go.work を書くので、プラグインがそれより
# 新しい版を求めると (例えば 2.0.0 の本体は go 1.27.1)、`go.work lists go 1.27.1`
# で何も組めない。逆に本体のほうが新しいときに下げてもいけない。
core=$(awk '/^go [0-9]/ {print $2; exit}' go.mod)
plug=$(awk '/^go [0-9]/ {print $2; exit}' "plugins/$PLUGIN_NAME/go.mod")
gover=$(printf '%s\n%s\n' "$core" "$plug" | sort -V | tail -n 1)
go work edit -go="$gover"

# 組み込む対象に、このプラグインが入っていることを確かめる。入っていないと、
# 以降の build が本体だけを組んで緑になる。
grep -q "plugins/$PLUGIN_NAME\"\?\$" go.work || { echo "go.work does not use plugins/$PLUGIN_NAME" >&2; cat go.work >&2; exit 1; }
grep -q "elythia-plugin-$PLUGIN_NAME\"" cmd/elythia/plugins_generated.go || { echo "cmd/elythia/plugins_generated.go does not import this plugin" >&2; exit 1; }
# frontend の typecheck がこのプラグインの .vue / .ts を読む経路はこの import
# だけ。pluginbuild は frontend/index.ts があるときだけ書くので、無いと
# typecheck はこのプラグインを見ないまま通る。
grep -q "@mkplugin/$PLUGIN_NAME'" frontend/packages/frontend/src/server-plugins.generated.ts || { echo "server-plugins.generated.ts does not import @mkplugin/$PLUGIN_NAME" >&2; exit 1; }
