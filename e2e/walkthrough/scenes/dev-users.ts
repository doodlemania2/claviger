import { expect, type Browser } from "@playwright/test";
import { SceneRun } from "../lib/scene";
import { api, configureInstance, ensureUser, stack } from "../lib/stack";
import { beat, ensureAdmin, hostedSignIn, open, press, typeIn } from "../lib/ui";
import { saveWorld, type World } from "../lib/world";

/** Scene 2: an auth collection with passkeys turned on, and a first user made in the admin UI. */
export async function usersPasskeys(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "users-passkeys", "A users collection with passkeys", "Turn on WebAuthn for the users collection and add the first user", [
        { name: "tomas", device: "desktop", label: "Tomas Alder (developer)" },
    ]);
    const { page, say } = scene.actor("tomas");
    try {
        await ensureAdmin(page, world.admin);
        await open(page, `${world.base}/_/#/collections?collection=users`);
        await say("Every Claviger instance starts with a users auth collection. Passkeys are opt in per collection");
        await page.getByRole("button", { name: "Collection settings" }).click();
        const editor = page.locator(".modal", { hasText: "Edit users collection" }).last();
        await beat(page, 1000);
        await editor.getByText("Options", { exact: true }).first().click();
        await beat(page, 800);
        await editor.locator("summary", { hasText: "WebAuthn / Passkeys" }).click();
        await say("In Options, the WebAuthn / Passkeys section: enable it, and allow usernameless sign-in");
        await editor.locator(".webauthn-accordion label", { hasText: /^Enable$/ }).click();
        await beat(page, 600);
        await editor.locator(".webauthn-accordion label", { hasText: "Usernameless sign-in" }).click();
        await beat(page, 1200);
        await press(editor.getByRole("button", { name: "Save changes" }), page, 1500);
        await expect(editor).toBeHidden();
        const col = await api("GET", "/api/collections/users");
        expect(col.webauthn.enabled).toBe(true);
        await say("Saved: the collection now accepts passkeys next to the password");

        // the first user, made in the record editor
        const priya = world.people.priya;
        await page.locator(".new-record-btn").click();
        await expect(page.getByRole("heading", { name: "Create users record" })).toBeVisible();
        await say("A first user, made in the record editor");
        await typeIn(page.locator('input[type="email"]'), priya.email);
        await typeIn(page.locator('input[type="password"]').nth(0), priya.password);
        await typeIn(page.locator('input[type="password"]').nth(1), priya.password);
        await typeIn(page.getByRole("textbox", { name: /^name/ }), priya.name);
        await page.getByText("verified", { exact: true }).first().click({ timeout: 2000 }).catch(() => undefined);
        await press(page.getByRole("button", { name: "Create", exact: true }), page, 1500);
        await expect(page.getByRole("heading", { name: "Create users record" })).toBeHidden();
        const found = await api("GET", `/api/collections/users/records?filter=${encodeURIComponent(`email="${priya.email}"`)}`);
        priya.id = found.items[0].id;
        await api("PATCH", `/api/collections/users/records/${priya.id}`, { verified: true });
        await ensureUser(world, "jonas");
        saveWorld(world);
        await say(`${priya.name} exists. Next she adds a passkey herself`);
    } finally {
        await scene.end();
    }
}

/** Scene 3: a user registers a passkey on the hosted /passkeys page and signs in with it. */
export async function passkeySignin(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "passkey-signin", "A user adds a passkey and signs in", "The hosted /passkeys page: register, sign out, sign in with the passkey alone", [
        { name: "priya", device: "desktop", label: "Priya Raman (user)", passkey: true },
    ]);
    const { page, say, mark } = scene.actor("priya");
    try {
        const priya = world.people.priya;
        await open(page, `${world.base}/passkeys`);
        await say("Claviger ships a hosted page for passkeys. Priya first signs in with her password");
        await page.locator("#linkPasswordLogin").click();
        await typeIn(page.locator("#pwEmail"), priya.email);
        await typeIn(page.locator("#pwPass"), priya.password);
        await press(page.locator("#btnPwLogin"), page, 1200);
        await expect(page.locator("#me")).toContainText(priya.email);
        await say("Signed in. Now she registers a passkey on this device");
        await press(page.locator("#btnRegister"), page, 800);
        await expect(page.getByText("Passkey added.")).toBeVisible();
        await beat(page, 1500);
        await say("The private key stays in the device; Claviger keeps only the public key");
        await press(page.locator("#linkLogout"), page, 1000);
        // the short passkey sign-in clip (clip.mjs ... passkey-signin signin-start signin-done) starts here
        mark("signin-start");
        await say("Signed out. Now she types her email and uses the passkey: no password");
        await typeIn(page.locator("#identity"), priya.email);
        await press(page.locator("#btnLogin"), page, 800);
        await expect(page.locator("#me")).toContainText(priya.email);
        await beat(page, 1500);
        await press(page.locator("#linkLogout"), page, 800);
        await say("With a discoverable passkey she does not even type the email");
        await press(page.locator("#btnLogin"), page, 800);
        await expect(page.locator("#me")).toContainText(priya.email);
        await beat(page, 1500);
        mark("signin-done");
    } finally {
        await scene.end();
    }
}
