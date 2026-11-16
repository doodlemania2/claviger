import { test, expect } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The screenshot kit (e2e/guides/README.md): the manifest validates, and the runner starts its own
// demo world and captures three screens, in light, dark and (hosted pages) mobile. A full capture
// takes minutes and stays out of the suite: node guides/capture.mjs --out <dir>.

const [major, minor] = process.versions.node.split(".").map(Number);
const canStripTypes = major > 22 || (major === 22 && minor >= 18);
const node = [process.execPath, "--disable-warning=ExperimentalWarning", "--disable-warning=MODULE_TYPELESS_PACKAGE_JSON", join(__dirname, "capture.mjs")];
const run = (args: string[]) => execFileSync(node[0], [...node.slice(1), ...args], { cwd: join(__dirname, ".."), encoding: "utf8", timeout: 170_000, env: { ...process.env, GUIDES_QUIET: "1" } });

test.skip(!canStripTypes || !!process.env.E2E_SKIP_GUIDES, "needs Node 22.18+ (type stripping)");
test.setTimeout(200_000);

test("the screen manifest validates", () => {
    const out = run(["--check"]);
    expect(out).toMatch(/check: \d+ screens/);
    expect(run(["--list"]).split("\n").filter(Boolean).length).toBeGreaterThan(50);
});

test("a capture of three screens writes light, dark and mobile images", () => {
    const dir = mkdtempSync(join(tmpdir(), "claviger-guides-test-"));
    try {
        run(["--out", dir, "--only", "admin-webhooks", "--only", "hosted-oidc-login", "--only", "hosted-erasure-confirm--invalid"]);
        const expected = [
            "integrations/img/admin-webhooks.png",
            "integrations/img/admin-webhooks-dark.png",
            "hosted/img/hosted-oidc-login.png",
            "hosted/img/hosted-oidc-login-dark.png",
            "hosted/img/hosted-oidc-login-mobile.png",
            "hosted/img/hosted-erasure-confirm--invalid.png",
        ];
        for (const f of expected) {
            const p = join(dir, f);
            expect(existsSync(p), f).toBe(true);
            expect(statSync(p).size, f).toBeGreaterThan(2000);
        }
    } finally {
        rmSync(dir, { recursive: true, force: true });
    }
});
