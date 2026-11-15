import { expect, type Locator, type Page } from "@playwright/test";
import type { Actor } from "./scene";
import { PACE } from "./scene";
import { stack } from "./stack";
import { totp } from "./totp";
import { saveWorld, type Person, type World } from "./world";

/** A deliberate pause so a person watching the video can follow. */
export const beat = (page: Page, ms = 1200): Promise<void> => page.waitForTimeout(ms * PACE);

/** Open a page and let it settle. */
export async function open(page: Page, url: string): Promise<void> {
    await page.goto(url, { waitUntil: "domcontentloaded" });
    await page.waitForLoadState("networkidle", { timeout: 10_000 }).catch(() => undefined);
    await beat(page, 800);
}

/** Type like a person: a few keystrokes a second. */
export async function typeIn(field: Locator, text: string, delay = PACE ? 30 : 0): Promise<void> {
    await field.click();
    await field.fill("");
    if (delay) await field.pressSequentially(text, { delay });
    else await field.fill(text);
}

/** Click and give the screen a moment. */
export async function press(target: Locator, page: Page, after = 900): Promise<void> {
    await target.click();
    await beat(page, after);
}

/** Run one named step. An optional step that fails is recorded as skipped instead of stopping the run. */
export async function step(world: World, scene: string, name: string, actor: Actor | null, fn: () => Promise<void>, optional = false): Promise<boolean> {
    try {
        await fn();
        return true;
    } catch (error) {
        if (!optional) throw error;
        const reason = String((error as Error).message).split("\n")[0] ?? "failed";
        return skip(world, scene, name, reason, actor);
    }
}

/** A step the product cannot show: recorded for the summary, and said on screen. */
export async function skip(world: World, scene: string, name: string, reason: string, actor: Actor | null): Promise<false> {
    world.skipped.push({ scene, step: name, reason });
    saveWorld(world);
    console.warn(`[walkthrough] skipped "${name}": ${reason}`);
    if (actor) {
        await actor.say(`Not shown here: ${name}. ${reason}`, 3500);
        await actor.say("");
    }
    return false;
}

const short = (v: unknown, n = 520): string => {
    const s = typeof v === "string" ? v : JSON.stringify(v, null, 1);
    const flat = s.replace(/\n\s*/g, " ");
    return flat.length > n ? flat.slice(0, n) + " ..." : flat;
};

/** Show an API call and its answer on a card, for steps that have no screen. */
export async function apiCard(actor: Actor, title: string, call: string, status: number, body: unknown, hold = 4500): Promise<void> {
    await actor.card(title, [`-> ${call}`, `<- ${status} ${short(body)}`]);
    await actor.page.waitForTimeout(hold * PACE);
    await actor.card(null);
}

export const hostedSignIn = async (page: Page, who: Pick<Person, "email" | "password">): Promise<void> => {
    const form = page.locator("form#pw");
    // a page that already holds this tab's session (after /passkeys, say) opens straight on its content
    const needed = await form
        .getByLabel("Email or username")
        .waitFor({ state: "visible", timeout: 3000 })
        .then(
            () => true,
            () => false,
        );
    if (!needed) return;
    await typeIn(form.getByLabel("Email or username"), who.email);
    await typeIn(form.getByLabel("Password"), who.password);
    await press(form.getByRole("button", { name: "Sign in", exact: true }), page, 1200);
};

export async function adminSignIn(page: Page, who: { email: string; password: string }): Promise<void> {
    await open(page, `${stack().world.base}/_/#/login`);
    await typeIn(page.getByRole("textbox", { name: /^Email/ }), who.email);
    await typeIn(page.getByRole("textbox", { name: /^Password/ }), who.password);
    await press(page.getByRole("button", { name: "Login" }), page, 1200);
    // a superuser who enrolled an authenticator app is asked for a code next
    const secret = stack().world.notes.totpSecret;
    const second = page.getByLabel("Authenticator or backup code");
    if (secret && (await second.waitFor({ timeout: 4000 }).then(() => true, () => false))) {
        await typeIn(second, totp(secret, Date.now() + 30_000));
        await press(page.getByRole("button", { name: /next|login|confirm/i }).last(), page, 1200);
    }
    await expect(page).toHaveURL(/#\/collections/);
}

/** Signs the actor in to the admin UI unless the saved session still works. */
export async function ensureAdmin(page: Page, who: { email: string; password: string }): Promise<void> {
    await open(page, `${stack().world.base}/_/#/collections`);
    const out = await page
        .locator(".app-main-nav")
        .waitFor({ timeout: 4000 })
        .then(
            () => false,
            () => true,
        );
    if (out) await adminSignIn(page, who);
}

/** Open the newest mail to an address in the mailbox viewer, as the person would in their mail app. */
export async function openMail(page: Page, to: string, subject?: RegExp): Promise<void> {
    const s = stack();
    await s.smtp.waitFor((m) => m.to.includes(to) && (!subject || subject.test(m.text.slice(0, 4000)) || subject.test(m.raw)));
    await open(page, `${s.rig.url}/mail?to=${encodeURIComponent(to)}`);
}

/** Runs a request from inside the page with the session token of a hosted page, and a passkey step-up when asked. */
export async function stepUpCall(page: Page, collection: string, method: string, path: string, body?: unknown): Promise<{ status: number; body: any }> {
    // A string, not a function: Playwright's transpiler must not touch the dynamic import.
    const script = `(async () => {
      const KEY = "claviger_orgs_token";
      let token = sessionStorage.getItem(KEY) || "";
      const send = async () => {
        const res = await fetch(${JSON.stringify(path)}, { method: ${JSON.stringify(method)}, headers: { "Content-Type": "application/json", Authorization: token }, body: ${body === undefined ? "undefined" : JSON.stringify(JSON.stringify(body))} });
        let b = {}; try { b = await res.json(); } catch (e) {}
        return { status: res.status, body: b };
      };
      let r = await send();
      if (r.status === 403 && JSON.stringify(r.body).includes("passkey_step_up_required")) {
        const mod = await import("/api/passkeys-client.js");
        const client = new mod.PasskeysClient({ baseUrl: location.origin, collection: ${JSON.stringify(collection)}, authToken: token, onToken: (t) => { token = t; sessionStorage.setItem(KEY, t); } });
        await client.stepUp();
        r = await send();
      }
      return r;
    })()`;
    return (await page.evaluate(script)) as { status: number; body: any };
}
