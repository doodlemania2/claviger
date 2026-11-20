// The screenshot capture runner of the guide kit. See e2e/guides/README.md.
//
//   node --disable-warning=ExperimentalWarning --disable-warning=MODULE_TYPELESS_PACKAGE_JSON e2e/guides/capture.mjs [flags]
//
//   --list                 print every id and state, no browser, no server
//   --check                validate the manifest (ids, audiences, routes against the router), no server
//   --check --live         also start the world and prove every route answers 200 or opens in the SPA
//   --only <id[--state]>   capture only that screen or state (repeatable)
//   --out <dir>            where to write (default: $CLAVIGER_GUIDES_DIR/guides, else the claviger-site checkout)
//   --specs <dir>          read the guide specs under <dir> (<area>/*.md) and capture their `shots:` instead
//   --jobs <n>             pages captured at the same time (default 4)
//   --attach <world.json>  reuse a world that `node world.ts --keep` left running (a dev aid)
//   --deny <file>          the safety scan's deny list (default: deny.json next to screens.json)
//   --no-dark / --no-mobile
//
// Every shot passes the safety scan (scan.mjs) before it is written; a shot that fails is not
// written, and the run exits non-zero. Each capture run writes <out>/scan-report.json:
// {"checked": <shots scanned>, "failures": [{"screen", "variant", "rule", "match"}]}.
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "@playwright/test";
import { parse as parseYaml } from "yaml";
import { attachWorld, startWorld, repoRoot } from "./world.ts";
import { DEFAULT_DENY_FILE, ScanError, errorView, loadDenyList, pageText, scanErrorState, scanText } from "./scan.mjs";

const here = dirname(fileURLToPath(import.meta.url));
export const AUDIENCES = ["developers", "superusers", "staff", "members"];
// the hosted pages' refusal card, the one scan hit worth a second look in a fresh context
const TURNED_AWAY = /^(?:no access|not a member of this organi[sz]ation)$/i;
const HOSTED_PREFIXES = ["/passkeys", "/oidc/", "/orgs/", "/erasure/", "/recovery", "/credentials/mine", "/households/mine"];
const SPA_PREFIX = "/_/#";
const DESKTOP = { width: 1440, height: 900 };
const MOBILE = { width: 390, height: 844 };

// ---- arguments ---------------------------------------------------------------------------

function parseArgs(argv) {
    const a = { only: [], jobs: 4, dark: true, mobile: true };
    for (let i = 0; i < argv.length; i++) {
        const k = argv[i];
        if (k === "--list") a.list = true;
        else if (k === "--check") a.check = true;
        else if (k === "--live") a.live = true;
        else if (k === "--only") a.only.push(argv[++i]);
        else if (k === "--out") a.out = argv[++i];
        else if (k === "--specs") a.specs = argv[++i];
        else if (k === "--jobs") a.jobs = Number(argv[++i]);
        else if (k === "--attach") a.attach = argv[++i];
        else if (k === "--deny") a.deny = argv[++i];
        else if (k === "--no-dark") a.dark = false;
        else if (k === "--no-mobile") a.mobile = false;
        else throw new Error(`unknown flag ${k}`);
    }
    return a;
}

/** The primary checkout, even when this file runs from a worktree. */
function primaryRoot() {
    try {
        const common = execFileSync("git", ["rev-parse", "--path-format=absolute", "--git-common-dir"], { cwd: repoRoot, encoding: "utf8" }).trim();
        return dirname(common);
    } catch {
        return repoRoot;
    }
}

function defaultOut() {
    if (process.env.CLAVIGER_GUIDES_DIR) return join(process.env.CLAVIGER_GUIDES_DIR, "guides");
    // ../../claviger-site/docs-site/guides, counted from the e2e directory of the primary checkout
    const dir = resolve(primaryRoot(), "e2e", "..", "..", "claviger-site", "docs-site");
    if (!existsSync(dir)) throw new Error(`no default output: ${dir} does not exist. Pass --out <dir> or set CLAVIGER_GUIDES_DIR.`);
    return join(dir, "guides");
}

// ---- manifest ----------------------------------------------------------------------------

export function loadManifest() {
    return JSON.parse(readFileSync(join(here, "screens.json"), "utf8")).screens;
}

const stateName = (s) => (s.name === "default" || !s.name ? "" : s.name);
const stem = (id, state) => (state ? `${id}--${state}` : id);
const isHosted = (route) => !route.startsWith("/_/");

function spaRoutePatterns() {
    const src = readFileSync(join(repoRoot, "ui", "src", "router.js"), "utf8");
    const out = [];
    for (const m of src.matchAll(/app\.routes\.\w+\(\s*"#(\/[^"]*)"/g)) out.push(m[1]);
    return out.map((p) => new RegExp("^" + p.replace(/\{[^}]+\}/g, "[^/]+") + "$"));
}

/** Static validation. Returns a list of problems. */
export function checkManifest(screens) {
    const problems = [];
    const ids = new Set();
    const spa = spaRoutePatterns();
    for (const s of screens) {
        const where = `screen ${s.id}`;
        if (!/^[a-z0-9]+(-[a-z0-9]+)*$/.test(s.id ?? "")) problems.push(`${where}: id must be kebab-case`);
        if (ids.has(s.id)) problems.push(`${where}: duplicate id`);
        ids.add(s.id);
        if (!AUDIENCES.includes(s.audience)) problems.push(`${where}: unknown audience "${s.audience}" (${AUDIENCES.join(", ")})`);
        if (!s.title) problems.push(`${where}: no title`);
        if (!s.area || !/^[a-z0-9-]+$/.test(s.area)) problems.push(`${where}: area must be a kebab-case folder name`);
        if (!s.route || !(s.route.startsWith("/") || /^\{\w+\}$/.test(s.route))) problems.push(`${where}: route must start with / (or be one placeholder)`);
        else if (s.route.startsWith("{")) {
            if (isHosted(s.route) && !["oidcLogin", "oidcConsent", "oidcChooser", "oidcError", "oidcBadRedirect"].includes(s.route.slice(1, -1))) problems.push(`${where}: unknown route placeholder ${s.route}`);
        }
        else if (isHosted(s.route) && !s.route.startsWith("{")) {
            if (!HOSTED_PREFIXES.some((p) => s.route.startsWith(p))) problems.push(`${where}: hosted route ${s.route} is not a known hosted page`);
        } else {
            const path = s.route.slice(SPA_PREFIX.length).split("?")[0] || "/";
            if (!spa.some((re) => re.test(path))) problems.push(`${where}: ${s.route} is not a route of ui/src/router.js`);
        }
        for (const x of [s, ...(s.states ?? [])]) {
            if ("showsError" in x && !(typeof x.showsError === "string" && x.showsError.trim())) problems.push(`${where}: showsError must say why the shot shows an error`);
        }
        const names = new Set();
        for (const st of s.states ?? [{ name: "default" }]) {
            const n = stateName(st);
            if (names.has(n)) problems.push(`${where}: duplicate state "${n}"`);
            names.add(n);
            if (n && !/^[a-z0-9]+(-[a-z0-9]+)*$/.test(n)) problems.push(`${where}: state "${n}" must be kebab-case`);
        }
    }
    return problems;
}

/** Every <id>[--state] the manifest defines. */
function allStems(screens) {
    return screens.flatMap((s) => (s.states ?? [{ name: "default" }]).map((st) => ({ screen: s, state: st, stem: stem(s.id, stateName(st)) })));
}

// ---- specs mode --------------------------------------------------------------------------

/** Reads <dir>/<area>/*.md and returns every shot with its area. */
export function readSpecShots(dir) {
    const shots = [];
    for (const area of readdirSync(dir)) {
        const adir = join(dir, area);
        if (!statSync(adir).isDirectory()) continue;
        for (const f of readdirSync(adir)) {
            if (!f.endsWith(".md")) continue;
            const text = readFileSync(join(adir, f), "utf8");
            const m = /^---\r?\n([\s\S]*?)\r?\n---/.exec(text);
            if (!m) continue;
            const fm = parseYaml(m[1]) ?? {};
            for (const shot of fm.shots ?? []) shots.push({ ...shot, area, spec: `${area}/${f}` });
        }
    }
    return shots;
}

// ---- placeholders ------------------------------------------------------------------------

async function fill(text, world) {
    let out = "";
    let last = 0;
    for (const m of text.matchAll(/\{(\w+)\}/g)) {
        out += text.slice(last, m.index);
        const name = m[1];
        if (name in world.vars) out += world.vars[name];
        else if (name in world.lazy) out += await world.lazy[name]();
        else throw new Error(`unknown placeholder {${name}}`);
        last = m.index + m[0].length;
    }
    return out + text.slice(last);
}

// ---- the page ----------------------------------------------------------------------------

const FIXED_DATE = "2026-03-14";

/**
 * Makes the volatile text of a page stable: timestamps become one fixed moment and record ids
 * become the same sequence on every run. It changes text and input values only (DOM, never a
 * style tag: the admin UI's CSP forbids inline style), so layout is untouched.
 */
async function normalize(page, origin) {
    await page.evaluate(([fixed, origin]) => {
        const ids = new Map();
        const pseudo = (id) => {
            if (!ids.has(id)) ids.set(id, "demo" + String(ids.size + 1).padStart(3, "0") + "x".repeat(8));
            return ids.get(id);
        };
        const DAY = 86400000;
        const today = Date.parse(new Date().toISOString().slice(0, 10));
        const base = Date.parse(fixed);
        // a date from today to a year ahead was made by this run (a stamp, a grace period's end):
        // move it to the same offset from the fixed day. Seeded calendar dates sit in the past of that range.
        const shiftDate = (d) => {
            const diff = Math.round((Date.parse(d) - today) / DAY);
            return diff >= 0 && diff <= 400 ? new Date(base + diff * DAY).toISOString().slice(0, 10) : null;
        };
        const rewrite = (s) =>
            s
                .replace(/\b(\d{4}-\d{2}-\d{2})(?:([ T])(\d{2}:\d{2}(?::\d{2}(\.\d+)?)?))?/g, (m, d, sep, time, frac) => {
                    const moved = shiftDate(d);
                    if (!moved) return m;
                    if (!time) return moved;
                    return moved + sep + (time.length === 5 ? "09:30" : frac ? "09:30:00.000" : "09:30:00");
                })
                // the browser's own en-GB format: 15/11/2026, 17:22:47
                .replace(/\b(\d{2})\/(\d{2})\/(\d{4})(?:(,? )(\d{2}:\d{2}(?::\d{2})?))?/g, (m, dd, mm, yyyy, sep, time) => {
                    const moved = shiftDate(`${yyyy}-${mm}-${dd}`);
                    if (!moved) return m;
                    const [y, mo, d] = moved.split("-");
                    return `${d}/${mo}/${y}` + (time ? sep + (time.length === 5 ? "09:30" : "09:30:00") : "");
                })
                // a node that holds only a time (the second line of a created column) is a stamp too
                .replace(/^(\s*)\d{2}:\d{2}:\d{2}(\.\d+)?(Z?\s*)$/, (m, a, f, z) => a + (f ? "09:30:00.000" : "09:30:00") + z)
                .replace(/clv_[a-z0-9]{8}/g, "clv_a1b2c3d4")
                .replace(/\b(?=[a-z0-9]*\d)(?=[a-z0-9]*[a-z])[a-z0-9]{15}\b/g, pseudo);
        const shown = "https://claviger.example.org";
        const rewriteAll = (s) => rewrite(s.split(origin).join(shown).replace(/localhost:\d+/g, "claviger.example.org"));
        const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
        const nodes = [];
        while (walker.nextNode()) nodes.push(walker.currentNode);
        for (const n of nodes) {
            const t = n.nodeValue;
            if (t && t.trim()) {
                const r = rewriteAll(t);
                if (r !== t) n.nodeValue = r;
            }
        }
        for (const el of document.querySelectorAll("input, textarea")) {
            if (el.type === "password" || el.type === "checkbox" || el.type === "radio" || !el.value) continue;
            // a device code or a TOTP secret is random on every run: show the well-known example values
            const demoSecret = "JBSWY3DPEHPK3PXP";
            const r = /^[A-Z0-9]{4}-[A-Z0-9]{4}$/.test(el.value)
                ? "WDJB-MJHT"
                : /^[A-Z2-7]{16,}$/.test(el.value)
                  ? demoSecret
                  : /^otpauth:\/\//.test(el.value)
                    ? `otpauth://totp/Claviger%20demo:operator@example.org?algorithm=SHA1&digits=6&issuer=Claviger+demo&period=30&secret=${demoSecret}`
                    : rewriteAll(el.value);
            if (r !== el.value) el.value = r;
        }
    }, [FIXED_DATE, origin]);
}

async function runStep(page, step, world) {
    const [kind] = Object.keys(step);
    const arg = step[kind];
    const sub = (s) => fill(s, world);
    switch (kind) {
        case "click": return page.locator(await sub(arg)).first().click({ timeout: 10_000 });
        case "fill": return page.locator(await sub(arg[0])).first().fill(await sub(arg[1]));
        case "select": return page.locator(await sub(arg[0])).first().selectOption(await sub(arg[1]));
        case "check": return page.locator(await sub(arg)).first().check({ force: true });
        case "press": return page.keyboard.press(arg);
        case "hover": return page.locator(await sub(arg)).first().hover();
        case "wait": return page.locator(await sub(arg)).first().waitFor({ state: "visible", timeout: 10_000 });
        case "scroll": return page.locator(await sub(arg)).first().scrollIntoViewIfNeeded();
        case "top": return page.locator(await sub(arg)).first().evaluate((e) => e.scrollIntoView({ block: "start" }));
        case "pause": return page.waitForTimeout(arg);
        case "goto": return page.goto(world.base + (await sub(arg)));
        default: throw new Error(`unknown step "${kind}"`);
    }
}

// ---- contexts ----------------------------------------------------------------------------

let signSeq = 10_000;
const authCache = new Map();
async function authFor(world, persona) {
    if (authCache.has(persona)) return authCache.get(persona);
    const isSu = persona === "superuser";
    const collection = isSu ? "_superusers" : world.personas[persona]?.collection;
    if (!collection) throw new Error(`unknown persona "${persona}"`);
    const token = isSu ? world.superuser.token : await world.tokenFor(persona);
    const r = await fetch(`${world.base}/api/collections/${collection}/auth-refresh`, { method: "POST", headers: { Authorization: token } });
    const record = (await r.json()).record;
    const v = { token, record, collection };
    authCache.set(persona, v);
    return v;
}

async function newContext(browser, world, { persona, passkey, dark, mobile, hosted }) {
    const ctx = await browser.newContext({
        baseURL: world.base,
        viewport: mobile ? MOBILE : DESKTOP,
        deviceScaleFactor: mobile ? 2 : 1,
        isMobile: !!mobile,
        hasTouch: !!mobile,
        colorScheme: dark ? "dark" : "light",
        reducedMotion: "reduce",
        locale: "en-GB",
        timezoneId: "UTC",
    });
    // nothing leaves the machine: an invented domain (a redirect URI, a webhook) is never visited
    await ctx.route(
        (url) => url.hostname !== "localhost" && url.hostname !== "127.0.0.1",
        (route) => route.abort(),
    );
    // a persona with no account (the invitee) means "not signed in", not a failed sign-in
    const noAccount = persona && persona !== "superuser" && world.personas[persona] && !world.personas[persona].id;
    if (persona && !noAccount) {
        const a = await authFor(world, persona);
        const payload = JSON.stringify({ token: a.token, record: a.record });
        await ctx.addInitScript(
            ([hostedPage, token, payloadJson, collection]) => {
                try {
                    if (hostedPage) {
                        sessionStorage.setItem("claviger_orgs_token", token);
                        localStorage.setItem("pb_passkeys_auth_" + collection, payloadJson);
                    } else {
                        localStorage.setItem("__pb_superusers__/_", payloadJson);
                    }
                } catch {}
            },
            [hosted, a.token, payload, a.collection],
        );
    }
    if (passkey) {
        const page = await ctx.newPage();
        const cdp = await ctx.newCDPSession(page);
        await cdp.send("WebAuthn.enable");
        const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
            options: { protocol: "ctap2", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
        });
        // a server refuses an assertion whose counter did not grow, so every context starts higher than the last
        for (const credential of await world.passkeys[passkey].credentials()) {
            await cdp.send("WebAuthn.addCredential", { authenticatorId, credential: { ...credential, signCount: (signSeq += 100) } });
        }
        // keep this page: the virtual authenticator lives and dies with its CDP session
        ctx.__page = page;
    }
    return ctx;
}

// ---- one shot ----------------------------------------------------------------------------

async function shoot(browser, world, job, variant, deny) {
    const { screen, state } = job;
    const hosted = isHosted(job.route);
    const persona = job.persona ?? (state.anon || (screen.anon && !state.persona) ? undefined : (state.persona ?? screen.persona ?? (hosted ? undefined : "superuser")));
    // a persona that holds a passkey in the world signs steps up with it
    const passkey = state.passkey ?? screen.passkey ?? (persona && world.passkeys[persona] ? persona : undefined);
    const ctx = await newContext(browser, world, { persona, passkey, dark: variant === "dark", mobile: variant === "mobile", hosted });
    try {
        const page = ctx.__page ?? (await ctx.newPage());
        const route = await fill(job.route, world);
        const res = await page.goto(world.base + route, { waitUntil: "load" });
        if (hosted && res && res.status() >= 400 && !(state.allowStatus ?? screen.allowStatus)) throw new Error(`${route} answered ${res.status()}`);
        if (!hosted && persona) await page.locator(".app-main-nav").first().waitFor({ timeout: 15_000 });
        await page.waitForLoadState("networkidle").catch(() => {});
        if (screen.ready) await page.locator(await fill(screen.ready, world)).first().waitFor({ state: "visible", timeout: 15_000 });
        for (const step of [...(screen.steps ?? []), ...(state.steps ?? [])]) {
            await runStep(page, step, world);
            await page.waitForLoadState("networkidle").catch(() => {});
        }
        await page.waitForTimeout(state.settle ?? 450);
        await normalize(page, world.base);
        // the safety scan: a shot that shows anything real is never written
        job.scanned?.();
        const hits = scanText(await pageText(page), deny);
        if (!(state.showsError ?? screen.showsError)) hits.push(...scanErrorState(await errorView(page)));
        if (hits.length) throw new ScanError(hits);
        const masks = [...(screen.mask ?? []), ...(state.mask ?? [])].map((sel) => page.locator(sel));
        const file = job.file(variant);
        mkdirSync(dirname(file), { recursive: true });
        await page.screenshot({ path: file, fullPage: !!(state.fullPage ?? screen.fullPage ?? (hosted && !(state.steps ?? []).some((x) => x.top))), animations: "disabled", caret: "hide", mask: masks, maskColor: "#8b8b8b" });
        return file;
    } finally {
        await ctx.close();
    }
}

// ---- main --------------------------------------------------------------------------------

function selectStems(screens, only) {
    let all = allStems(screens);
    if (only.length) {
        all = all.filter((x) => only.some((o) => x.stem === o || x.screen.id === o));
        if (!all.length) throw new Error(`--only ${only.join(", ")}: no such id`);
    }
    return all;
}

async function liveCheck(screens, args) {
    const world = args.attach ? await attachWorld(args.attach) : await startWorld({ quiet: true });
    const problems = [];
    const browser = await chromium.launch();
    try {
        const missing = [...new Set(screens.flatMap((s) => [s.route, ...(s.states ?? []).flatMap((st) => (st.steps ?? []).filter((x) => x.goto).map((x) => x.goto))]).flatMap((r) => [...r.matchAll(/\{(\w+)\}/g)].map((m) => m[1])))].filter((n) => !(n in world.vars) && !(n in world.lazy));
        for (const n of missing) problems.push(`placeholder {${n}} is not defined by the world`);
        const spa = await newContext(browser, world, { persona: "superuser", hosted: false });
        const guest = await newContext(browser, world, { hosted: false });
        for (const s of screens) {
            try {
                const route = await fill(s.route, world);
                if (isHosted(s.route)) {
                    const r = await fetch(world.base + route, { redirect: "follow" });
                    if (r.status !== 200 && !s.allowStatus) problems.push(`${s.id}: ${route} answered ${r.status}`);
                } else {
                    const page = await (s.anon ? guest : spa).newPage();
                    await page.goto(world.base + route);
                    if (s.anon) await page.waitForLoadState("networkidle");
                    else await page.locator(".app-main-nav").first().waitFor({ timeout: 15_000 });
                    const want = "#" + route.slice(SPA_PREFIX.length).split("?")[0];
                    const got = await page.evaluate(() => location.hash.split("?")[0]);
                    if (got !== want) problems.push(`${s.id}: ${route} opened ${got} in the SPA`);
                    await page.close();
                }
            } catch (e) {
                problems.push(`${s.id}: ${String(e.message).split("\n")[0]}`);
            }
        }
        await spa.close();
        await guest.close();
    } finally {
        await browser.close();
        await world.stop();
    }
    return problems;
}

async function main() {
    const args = parseArgs(process.argv.slice(2));
    const screens = loadManifest();

    if (args.list) {
        for (const x of selectStems(screens, args.only)) console.log(`${x.stem}\t${x.screen.audience}\t${x.screen.area}\t${x.screen.route}`);
        return 0;
    }

    if (args.check) {
        const problems = checkManifest(screens);
        if (args.specs) {
            const known = new Set(allStems(screens).map((x) => x.stem));
            for (const sh of readSpecShots(args.specs)) {
                if (!known.has(sh.screen)) problems.push(`${sh.spec}: shot "${sh.out}" names unknown screen "${sh.screen}"`);
                if (sh.persona && sh.persona !== "superuser" && !PERSONAS.includes(sh.persona)) problems.push(`${sh.spec}: unknown persona "${sh.persona}"`);
                const scr = screens.find((x) => sh.screen === x.id || sh.screen.startsWith(x.id + "--"));
                if (scr?.flow && sh.path) console.warn(`  warning: ${sh.spec}: shot "${sh.out}" gives path ${sh.path}, but ${scr.id} is a flow screen: the path is ignored`);
            }
        }
        if (args.live) problems.push(...(await liveCheck(screens, args)));
        if (problems.length) {
            console.error(problems.map((p) => `  ${p}`).join("\n"));
            console.error(`check: ${problems.length} problem(s)`);
            return 1;
        }
        console.log(`check: ${screens.length} screens, ${allStems(screens).length} states ok${args.live ? " (live)" : ""}`);
        return 0;
    }

    const outRoot = args.out ? resolve(args.out) : args.specs ? resolve(args.specs) : defaultOut();

    // what to capture: a stem per manifest state, or one per spec shot
    let jobs;
    if (args.specs) {
        const byStem = new Map(allStems(screens).map((x) => [x.stem, x]));
        jobs = [];
        for (const sh of readSpecShots(args.specs)) {
            const x = byStem.get(sh.screen);
            if (!x) throw new Error(`${sh.spec}: unknown screen "${sh.screen}"`);
            if (args.only.length && !args.only.some((o) => o === sh.screen || o === sh.out)) continue;
            const name = String(sh.out).replace(/\.png$/, "");
            // a flow screen needs a generated request (an authorize call, a device code, a token): the manifest recipe wins
            jobs.push({ ...x, route: x.screen.flow ? x.screen.route : sh.path || x.screen.route, persona: sh.persona, file: (v) => join(outRoot, sh.area, "img", name + (v === "dark" ? "-dark" : v === "mobile" ? "-mobile" : "") + ".png") });
        }
    } else {
        jobs = selectStems(screens, args.only).map((x) => ({ ...x, route: x.screen.route, file: (v) => join(outRoot, x.screen.area, "img", x.stem + (v === "dark" ? "-dark" : v === "mobile" ? "-mobile" : "") + ".png") }));
    }

    const deny = loadDenyList(args.deny ? resolve(args.deny) : DEFAULT_DENY_FILE);
    const started = Date.now();
    const world = args.attach ? await attachWorld(args.attach) : await startWorld({ quiet: true });
    const browser = await chromium.launch();
    const failures = [];
    const scanFailures = [];
    let written = 0;
    let checked = 0;
    try {
        const variantsOf = (job) => {
            const v = ["light"];
            if (args.dark && job.state.dark !== false) v.push("dark");
            if (args.mobile && isHosted(job.route) && job.state.mobile !== false) v.push("mobile");
            return v;
        };
        const queue = [];
        for (const job of jobs) for (const v of variantsOf(job)) queue.push({ job, v });
        // states that submit something run last, one at a time
        const normal = queue.filter((q) => !q.job.state.mutates);
        const last = queue.filter((q) => q.job.state.mutates);
        const worker = async (list) => {
            for (;;) {
                const q = list.shift();
                if (!q) return;
                try {
                    // A hosted page can turn a signed-in persona away once (a refresh that lost a race on a
                    // busy runner): that card is retried in a fresh context. Any other hit fails at once,
                    // and a persona that really has no access fails after the last attempt.
                    let counted = false;
                    for (let attempt = 1; ; attempt++) {
                        try {
                            await shoot(browser, world, { ...q.job, scanned: () => (counted ? 0 : (counted = true, checked++)) }, q.v, deny);
                            break;
                        } catch (e) {
                            const turnedAway = e instanceof ScanError && e.hits.every((h) => TURNED_AWAY.test(h.match));
                            if (!turnedAway || attempt >= 3) throw e;
                        }
                    }
                    written++;
                } catch (e) {
                    if (e instanceof ScanError) {
                        for (const h of e.hits) scanFailures.push({ screen: q.job.stem, variant: q.v, rule: h.rule, match: h.match });
                    }
                    failures.push(`${q.job.stem} (${q.v}): ${String(e.message).split("\n")[0]}`);
                }
            }
        };
        await Promise.all(Array.from({ length: Math.max(1, args.jobs) }, () => worker(normal)));
        await worker(last);
    } finally {
        await browser.close();
        await world.stop();
    }
    const secs = Math.round((Date.now() - started) / 1000);
    mkdirSync(outRoot, { recursive: true });
    writeFileSync(join(outRoot, "scan-report.json"), JSON.stringify({ checked, failures: scanFailures }, null, 2) + "\n");
    console.log(`capture: ${written} images in ${secs}s -> ${outRoot}`);
    console.log(`scan: ${checked} shots checked, ${scanFailures.length} failure(s)`);
    for (const f of scanFailures) console.error(`  scan: ${f.screen} (${f.variant}): ${f.rule} "${f.match}"`);
    if (failures.length) {
        console.error(`capture: ${failures.length} failure(s)\n` + failures.map((f) => `  ${f}`).join("\n"));
        return 1;
    }
    return 0;
}

const PERSONAS = ["org-owner", "org-admin", "invitee", "coordinator", "registrar", "greeter", "member", "guardian", "clergy", "care-team"];

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
    main().then(
        (code) => process.exit(code),
        (e) => {
            console.error(e.message ?? e);
            process.exit(1);
        },
    );
}
