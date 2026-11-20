import { test, expect } from "@playwright/test";
import { execFileSync, spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
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
        const report = JSON.parse(readFileSync(join(dir, "scan-report.json"), "utf8"));
        // every image written was scanned, and nothing failed
        const pngs = readdirSync(dir, { recursive: true }).filter((f) => String(f).endsWith(".png"));
        expect(report).toEqual({ checked: pngs.length, failures: [] });
    } finally {
        rmSync(dir, { recursive: true, force: true });
    }
});

test("the safety scan flags real contact details, internal hostnames and the deny list", async () => {
    const { scanText, loadDenyList } = await import(join(__dirname, "scan.mjs"));
    const safe = "operator@example.org dara@calloway.example.com +1 214 555 0142 (214) 555-0199 2026-03-14 09:30:00 15/11/2026, 17:22:47 4102444800 https://claviger.example.org/oidc v0.41.0";
    expect(scanText(safe)).toEqual([]);
    const rules = (t: string, deny: string[] = []) => scanText(t, deny).map((h: { rule: string }) => h.rule);
    expect(rules("write to someone@gmail.com")).toEqual(["email"]);
    expect(rules("call (972) 867-5309")).toEqual(["phone"]);
    expect(rules("call +1 214 555 0200")).toEqual(["phone"]);
    expect(rules("call +442079460100")).toEqual(["phone"]);
    expect(rules("see docs.claviger.app")).toEqual(["hostname"]);
    expect(rules("auth.stfoafrisco.org")).toEqual(["hostname"]);
    expect(rules("box.tail1234.ts.net")).toEqual(["hostname"]);
    expect(rules("a Secret Thing here", ["secret thing"])).toEqual(["deny"]);
    // a half-typed example address passes only for the walkthrough recorder
    expect(rules("tomas@example.o")).toEqual(["email"]);
    expect(scanText("tomas@example.o", [], { typing: true })).toEqual([]);
    expect(rules("Autotomas@example.orgManage")).toEqual(["email"]);
    expect(loadDenyList()).toEqual([]);
});

test("the safety scan flags error states", async () => {
    const { scanErrorState } = await import(join(__dirname, "scan.mjs"));
    const rules = (prose: string, panels: string[] = []) => scanErrorState({ prose, panels }).map((h: { match: string }) => h.match);
    expect(rules("Retention policies Archive old newsletter drafts", [])).toEqual([]);
    expect(rules("Something went wrong")).toEqual(["Something went wrong"]);
    expect(rules("Please try again later.")).toEqual(["try again"]);
    expect(rules("Failed to load record.")).toEqual(["Failed to load"]);
    expect(rules("No access You are not a member of this organization.")).toEqual(["No access", "not a member of this organization"]);
    expect(rules("6 consecutive failures lookup rota.example.com on 8.8.8.8:53: no such host")).toEqual(["consecutive failures", "no such host"]);
    expect(rules("Orgs", [""])).toEqual(["(empty error panel)"]);
    expect(rules("Orgs", ["Name is required."])).toEqual(["Name is required."]);
});

test("a shot that fails the safety scan is not written and the run fails", () => {
    const dir = mkdtempSync(join(tmpdir(), "claviger-guides-scan-"));
    try {
        // "Webhooks" is on the webhooks page, so the deny list must stop that shot
        const deny = join(dir, "deny.json");
        writeFileSync(deny, JSON.stringify({ substrings: ["webhooks"] }));
        const out = join(dir, "out");
        const r = spawnSync(node[0], [...node.slice(1), "--out", out, "--only", "admin-webhooks", "--no-dark", "--deny", deny], { cwd: join(__dirname, ".."), encoding: "utf8", timeout: 170_000, env: { ...process.env, GUIDES_QUIET: "1" } });
        expect(r.status, r.stdout + r.stderr).toBe(1);
        // the page title says "Webhooks" in every state, so no state is written
        const img = join(out, "integrations", "img");
        expect(existsSync(img) ? readdirSync(img) : []).toEqual([]);
        const report = JSON.parse(readFileSync(join(out, "scan-report.json"), "utf8"));
        expect(report.checked).toBeGreaterThan(0);
        expect(new Set(report.failures.map((f: { variant: string; screen: string }) => f.screen)).size).toBe(report.checked);
        expect(report.failures.length).toBeGreaterThan(0);
        expect(report.failures[0]).toMatchObject({ screen: "admin-webhooks", rule: "deny" });
    } finally {
        rmSync(dir, { recursive: true, force: true });
    }
});
