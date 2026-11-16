#!/usr/bin/env bash
# Record a walkthrough of Claviger on a LOCAL instance and stitch it into videos.
#
#   e2e/walkthrough/run.sh                       # the developer story
#   WALK_STORY=parish e2e/walkthrough/run.sh
#
# It builds the binary (unless E2E_SKIP_BUILD is set and e2e/.bin/claviger exists), starts it on
# localhost with a temp data dir, records the scenes with Playwright, then stitches them. It never
# talks to a deployment and is not part of this repo's CI or of the default `npx playwright test` run.
# It runs on Linux too (a CI job with Node, ffmpeg and either Go or a prebuilt e2e/.bin/claviger).
# Every recording is safety-scanned while it records (lib/scan.ts); stitch.mjs refuses one that failed.
# See docs/walkthrough.md.
#
# Settings (environment): WALK_STORY=developer|parish  WALK_SLOWMO=200 (ms between browser actions)
#   WALK_ONLY=1,3 (scene numbers or names; a partial run is not stitched unless WALK_STITCH=1)
#   WALK_RUN_DIR=<old run to resume>  WALK_FAST=1 (no reading pauses, no slowMo: a quick check)
#   WALK_PORT=18190  E2E_SKIP_BUILD=1  FFMPEG=/path/ffmpeg FFPROBE=/path/ffprobe (default: from PATH,
#   then /opt/homebrew/bin)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$(cd "$HERE/.." && pwd)"
ROOT="$(cd "$E2E/.." && pwd)"
tool_path() { command -v "$1" 2>/dev/null || { [ -x "/opt/homebrew/bin/$1" ] && echo "/opt/homebrew/bin/$1"; } || true; }
FFMPEG="${FFMPEG:-$(tool_path ffmpeg)}"
FFPROBE="${FFPROBE:-$(tool_path ffprobe)}"
export FFMPEG FFPROBE
for tool in node "$FFMPEG" "$FFPROBE"; do
  if [ -z "$tool" ] || ! command -v "$tool" >/dev/null; then echo "${tool:-ffmpeg/ffprobe} is not installed" >&2; exit 2; fi
done

if [ ! -x "$E2E/.bin/claviger" ] || [ -z "${E2E_SKIP_BUILD:-}" ]; then
  command -v go >/dev/null || { echo "go is not installed (or set E2E_SKIP_BUILD=1 with a prebuilt e2e/.bin/claviger)" >&2; exit 2; }
  mkdir -p "$E2E/.bin"
  (cd "$ROOT" && go build -o "$E2E/.bin/claviger" ./examples/base)
fi
export E2E_SKIP_BUILD=1

cd "$E2E"
[ -d node_modules ] || npm ci
BEFORE="$(ls -d "$HERE"/.out/2* 2>/dev/null | tail -1 || true)"
STATUS=0
npx playwright test -c walkthrough/playwright.walkthrough.config.ts || STATUS=$?

RUN_DIR="${WALK_RUN_DIR:-$(ls -d "$HERE"/.out/2* 2>/dev/null | tail -1)}"
if [ -n "${WALK_ONLY:-}" ] && [ -z "${WALK_STITCH:-}" ]; then
  echo "partial run (WALK_ONLY=$WALK_ONLY): not stitched. Raw recordings: $RUN_DIR/scenes"
elif [ -n "$RUN_DIR" ] && [ -d "$RUN_DIR/scenes" ]; then
  node "$HERE/stitch.mjs" "$RUN_DIR" || STATUS=$?
  echo "videos: $RUN_DIR/video"
fi
exit "$STATUS"
