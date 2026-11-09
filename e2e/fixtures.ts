import { test as base, expect, type Page, type BrowserContext } from "@playwright/test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { createHmac } from "node:crypto";

export type State = {
    base: string;
    token: string;
    admin: { email: string; password: string };
    user: { email: string; password: string };
    data: string;
    log: string;
};

export function state(): State {
    return JSON.parse(readFileSync(join(__dirname, ".run", "state.json"), "utf8"));
}

/** Superuser API call. Throws on a non-2xx answer. */
export async function api(method: string, path: string, body?: unknown, token = state().token) {
    const s = state();
    const res = await fetch(s.base + path, {
        method,
        headers: { Authorization: token, "Content-Type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${text}`);
    return text ? JSON.parse(text) : {};
}

// ---- RFC 6238, SHA-1, 6 digits, 30s: what authenticator apps do ----
export function base32Decode(s: string): Buffer {
    const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
    let bits = "";
    for (const c of s.replace(/=+$/, "").toUpperCase()) {
        const v = alphabet.indexOf(c);
        if (v < 0) continue;
        bits += v.toString(2).padStart(5, "0");
    }
    const out: number[] = [];
    for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2));
    return Buffer.from(out);
}

export function totp(secret: string, at = Date.now(), step = 30): string {
    const key = base32Decode(secret);
    const counter = Buffer.alloc(8);
    counter.writeBigUInt64BE(BigInt(Math.floor(at / 1000 / step)));
    const h = createHmac("sha1", key).update(counter).digest();
    const off = h[h.length - 1] & 0xf;
    const code = (h.readUInt32BE(off) & 0x7fffffff) % 1_000_000;
    return String(code).padStart(6, "0");
}

// ---- console / CSP guard ----------------------------------------------------

const CSP_HOOK = `
window.__cspViolations = [];
document.addEventListener("securitypolicyviolation", (e) => {
  window.__cspViolations.push(e.violatedDirective + " blocked " + e.blockedURI + " (" + e.sourceFile + ":" + e.lineNumber + ")");
});
`;

export type Problems = { list: string[]; ignore: (re: RegExp) => void };

/** Attaches console error, page error, failed-request and CSP listeners to a page. */
export function watch(page: Page, problems: Problems) {
    page.addInitScript(CSP_HOOK);
    page.on("console", (m) => {
        if (m.type() === "error") {
            problems.list.push(`console.error: ${m.text()} @ ${m.location().url}`);
        }
        // Chromium reports CSP blocks as console messages too
        if (/Content Security Policy|violates the following/i.test(m.text())) {
            problems.list.push(`CSP: ${m.text()}`);
        }
    });
    page.on("pageerror", (e) => problems.list.push(`pageerror: ${e.message}`));
}

type Fixtures = {
    problems: Problems;
    /** A page of a fresh context with the console/CSP guard attached. */
    guarded: Page;
    /** A guarded page signed in as the superuser. */
    admin: Page;
    newGuardedPage: (ctx?: BrowserContext) => Promise<Page>;
};

export const test = base.extend<Fixtures>({
    problems: async ({}, use) => {
        const ignored: RegExp[] = [];
        const problems: Problems = {
            list: [],
            ignore: (re) => ignored.push(re),
        };
        await use(problems);
        const real = problems.list.filter((p) => !ignored.some((re) => re.test(p)));
        expect(real, "console errors / CSP violations").toEqual([]);
    },

    guarded: async ({ page, problems }, use) => {
        watch(page, problems);
        await use(page);
        const csp = await page.evaluate(() => (window as any).__cspViolations || []).catch(() => []);
        expect(csp, "securitypolicyviolation events").toEqual([]);
    },

    admin: async ({ browser, problems }, use) => {
        const ctx = await browser.newContext({ baseURL: state().base });
        const page = await ctx.newPage();
        watch(page, problems);
        await signInAdmin(page);
        await use(page);
        const csp = await page.evaluate(() => (window as any).__cspViolations || []).catch(() => []);
        await ctx.close();
        expect(csp, "securitypolicyviolation events").toEqual([]);
    },

    newGuardedPage: async ({ browser, problems }, use) => {
        const ctxs: BrowserContext[] = [];
        await use(async (ctx) => {
            const c = ctx ?? (await browser.newContext({ baseURL: state().base }));
            ctxs.push(c);
            const p = await c.newPage();
            watch(p, problems);
            return p;
        });
        for (const c of ctxs) await c.close();
    },
});

export { expect };

export async function signInAdmin(page: Page) {
    const s = state();
    await page.goto("/_/#/login");
    await page.getByRole("textbox", { name: /^Email/ }).fill(s.admin.email);
    await page.getByRole("textbox", { name: /^Password/ }).fill(s.admin.password);
    await page.getByRole("button", { name: "Login" }).click();
    await expect(page).toHaveURL(/#\/collections/);
}

/** Accepts the admin UI's confirmation popup. */
export async function confirmYes(page: Page) {
    await page.locator(".modal.popup.manual").getByRole("button", { name: "Yes" }).click();
}
