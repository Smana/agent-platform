#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# demo-site.sh OUT: the live demo as a flat static site, every path relative, so it serves
# from a domain root or from GitHub Pages' /<repo>/ prefix alike.
set -euo pipefail
out=${1:?usage: demo-site.sh OUT}
web=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out/fixtures"
out=$(cd "$out" && pwd)
cd "$web"
[ -d node_modules ] || npm ci --no-audit --no-fund
npx esbuild preview/demo.ts --bundle --minify --format=esm --target=es2022 --outfile="$out/demo.js"
cp preview/demo.html "$out/index.html"
cp src/app.css "$out/app.css"
cp test/fixtures/demo-session.jsonl test/fixtures/demo-snapshot.json test/fixtures/demo-summary.json "$out/fixtures/"
echo "demo site: $out"
