#!/bin/sh
# rewrite-module-path.sh [path...]
#
# Rewrites the upstream Go module path to the Claviger one in tracked text files:
#
#   ganigeorgiev.com/pocketbase/pocketbase  ->  github.com/doodlemania2/claviger
#
# WHEN TO USE: upstream PocketBase keeps its own module path, and this fork
# syncs upstream with `git merge <tag>`. Every merge brings back new or changed
# files that import the old path. Run this right after the merge (before
# resolving conflicts, and before building) and commit the result.
#
# SAFETY:
#   - Only the exact module path is rewritten. The separate dependency modules
#     ganigeorgiev.com/pocketbase/{dbx,ozzo-validation,tygoja} and
#     ganigeorgiev.com/fexpr keep their paths (they do not match).
#   - Web URLs (https://ganigeorgiev.com/pocketbase/pocketbase/...) are left alone:
#     a match preceded by "/" is skipped.
#   - Idempotent: a second run changes nothing.
#   - Skips .git, ui/node_modules, ui/dist and binary files.
#   - Never touches prose: with no arguments only code and build files are
#     processed (*.go, go.mod, go.sum, Dockerfile, .goreleaser.yaml, and *.ts/*.js
#     under plugins/); ui/ and all *.md are skipped.
#   - Works with BSD sed (macOS) and GNU sed (Linux): no -i, no GNU extensions.
#
# With no arguments the code/build files above are processed. With path
# arguments, every tracked text file at or under the given paths is rewritten
# exactly as given (no file-type filter). Prints each file it changed.

set -eu

OLD='ganigeorgiev\.com/pocketbase/pocketbase'
NEW='github.com/doodlemania2/claviger'

cd "$(git rev-parse --show-toplevel)"

# Default scope: code and build files only (see header).
default_scope() {
  grep -E -e '\.go$' -e '(^|/)go\.(mod|sum)$' -e '(^|/)Dockerfile$' \
    -e '(^|/)\.goreleaser\.yaml$' -e '^plugins/.*\.(ts|js)$' |
    grep -v '^ui/'
}
all_files() { cat; }
if [ "$#" -eq 0 ]; then
  scope=default_scope
  set -- .
else
  scope=all_files
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT INT TERM

git ls-files -z -- "$@" |
  tr '\0' '\n' |
  $scope |
  grep -v -e '^ui/node_modules/' -e '^ui/dist/' |
  while IFS= read -r f; do
    [ -f "$f" ] || continue
    [ -L "$f" ] && continue
    grep -I -q . "$f" 2>/dev/null || continue
    grep -E -q "(^|[^/])$OLD" "$f" || continue
    sed -E "s#(^|[^/])$OLD#\\1$NEW#g" "$f" >"$tmp"
    if ! cmp -s "$f" "$tmp"; then
      cat "$tmp" >"$f" # keep mode and inode
      echo "$f"
    fi
  done
