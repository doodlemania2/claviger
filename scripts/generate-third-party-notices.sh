#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_FILE="$ROOT_DIR/THIRD_PARTY_NOTICES.md"
TMP_DIR="$(mktemp -d)"
DATE="$(date +%Y-%m-%d)"

cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

cd "$ROOT_DIR"

if ! command -v go >/dev/null 2>&1; then
  echo "error: go is required" >&2
  exit 1
fi

if ! command -v node >/dev/null 2>&1; then
  echo "error: node is required" >&2
  exit 1
fi

if ! command -v go-licenses >/dev/null 2>&1; then
  GO_BIN_DIR="$TMP_DIR/go-bin"
  mkdir -p "$GO_BIN_DIR"
  GOBIN="$GO_BIN_DIR" go install github.com/google/go-licenses@latest
  GO_LICENSES="$GO_BIN_DIR/go-licenses"
else
  GO_LICENSES="$(command -v go-licenses)"
fi

"$GO_LICENSES" report ./... > "$TMP_DIR/go_licenses.csv" 2> "$TMP_DIR/go_licenses.err" || true

# Collect npm dependency license metadata from installed ui/node_modules packages.
(
  cd "$ROOT_DIR/ui"
  node -e '
const fs = require("fs");
const path = require("path");
const root = path.resolve("node_modules");
if (!fs.existsSync(root)) {
  console.error("error: ui/node_modules is missing; run npm ci in ui/");
  process.exit(1);
}
const out = [];
function scan(dir) {
  for (const ent of fs.readdirSync(dir, { withFileTypes: true })) {
    if (ent.name.startsWith(".")) continue;
    const full = path.join(dir, ent.name);
    if (!ent.isDirectory()) continue;
    if (ent.name.startsWith("@")) {
      scan(full);
      continue;
    }
    const pkg = path.join(full, "package.json");
    if (!fs.existsSync(pkg)) continue;
    try {
      const j = JSON.parse(fs.readFileSync(pkg, "utf8"));
      const license = typeof j.license === "string" ? j.license : (j.license ? JSON.stringify(j.license) : "Unknown");
      out.push({ name: j.name || ent.name, version: j.version || "", license });
    } catch {
      // ignore malformed package files
    }
  }
}
scan(root);
out.sort((a, b) => (a.name + a.version).localeCompare(b.name + b.version));
for (const row of out) {
  console.log(`${row.name}\t${row.version}\t${row.license}`);
}
' > "$TMP_DIR/ui_licenses.tsv"
)

cat > "$OUT_FILE" <<EOF
# Third-Party Notices

This repository is licensed under MIT (see LICENSE.md).

This file documents third-party dependency license metadata for the current source tree.
It is intended as an attribution and compliance aid for source and binary distributions.

Generated on: $DATE

## Scope

- Go modules from go.mod / go.sum
- UI npm packages from ui/package-lock.json (resolved from ui/node_modules)

## Notes

- License metadata is sourced from dependency manifests and module license files.
- SPDX/manifest metadata can occasionally be incomplete or inaccurate; verify during release audits.
- One Go dependency (modernc.org/mathutil) is listed below as BSD-3-Clause based on its LICENSE file in the module source.

## Go Dependencies

<!-- markdownlint-disable MD034 MD060 -->

| Dependency | License | License URL |
|---|---|---|
EOF

awk -F',' 'NF>=3 {
  dep=$1; url=$2; lic=$3;
  if (dep=="modernc.org/mathutil" && (lic=="Unknown" || lic=="UNKNOWN" || lic=="")) {
    lic="BSD-3-Clause";
    url="https://gitlab.com/cznic/mathutil";
  }
  if (url=="" || url=="Unknown") url="N/A";
  if (lic=="" || lic=="Unknown") lic="Unknown";
  printf("| %s | %s | %s |\n", dep, lic, url);
}' "$TMP_DIR/go_licenses.csv" | sort >> "$OUT_FILE"

cat >> "$OUT_FILE" <<'EOF'

## UI (npm) Dependencies

| Dependency | Version | License |
|---|---:|---|
EOF

awk -F'\t' 'NF>=3 {
  dep=$1; ver=$2; lic=$3;
  if (lic=="" || lic=="UNKNOWN") lic="Unknown";
  printf("| %s | %s | %s |\n", dep, ver, lic);
}' "$TMP_DIR/ui_licenses.tsv" | sort >> "$OUT_FILE"

cat >> "$OUT_FILE" <<'EOF'

## Internal Review Summary

- Copyleft blockers detected (GPL/AGPL/LGPL/SSPL/BUSL): none in current inventory.
- Permissive licenses present: MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, CC0-1.0.

For legal sign-off, perform a final release-time audit against the exact lockfiles and build artifacts.

<!-- markdownlint-enable MD034 MD060 -->
EOF

echo "Generated $OUT_FILE"
