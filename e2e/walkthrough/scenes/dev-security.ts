import { expect, type Browser } from "@playwright/test";
import { SceneRun } from "../lib/scene";
import { saveWorld, type World } from "../lib/world";
import { totp } from "../lib/totp";
import { adminSignIn, apiCard, expectStatus, beat, ensureAdmin, open, press, typeIn } from "../lib/ui";
import { rawCall } from "../lib/stack";

/** Scene 4: the superuser enrols an authenticator app, signs in with a code, then with a backup code. */
export async function totpScene(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "totp", "An authenticator app and backup codes", "Second factor for the superuser account", [
        { name: "tomas", device: "desktop", label: "Tomas Alder (developer)" },
    ]);
    const { page, say } = scene.actor("tomas");
    try {
        await ensureAdmin(page, world.admin);
        await open(page, `${world.base}/_/#/settings/security`);
        await say("Security settings: Tomas protects the superuser account with an authenticator app");
        await typeIn(page.getByRole("textbox", { name: /^Current password/ }), world.admin.password);
        await press(page.getByRole("button", { name: "Set up authenticator app" }), page, 1200);
        const secret = await page.locator(".field", { hasText: "Setup key" }).locator("input").inputValue();
        world.notes.totpSecret = secret;
        saveWorld(world);
        await say("Claviger shows a setup key and an otpauth link for the app. The film's rig plays the app and computes the code", 4000);
        await typeIn(page.getByLabel("First 6 digit code"), totp(secret));
        await press(page.getByRole("button", { name: "Confirm and enable" }), page, 1200);
        await expect(page.getByText("Save these backup codes now.")).toBeVisible();
        const codes = (await page.locator("#totp-backup-codes").innerText()).trim().split(/\s+/);
        world.notes.backupCode = codes[0];
        saveWorld(world);
        await say("Backup codes are shown once. Each one works a single time", 4000);
        await press(page.getByRole("button", { name: "I saved them" }), page, 1000);
        await expect(page.getByText("enabled", { exact: true })).toBeVisible();

        // sign out (clear the saved session) and sign in again: password, then the code
        await say("Signing out. A password alone no longer signs in", 2500);
        await page.evaluate(() => localStorage.clear());
        await adminSignIn(page, world.admin); // fills the second factor with the app's next code
        await say("Password, then the six digit code: signed in");

        // and with a backup code
        await page.evaluate(() => localStorage.clear());
        await open(page, `${world.base}/_/#/login`);
        await typeIn(page.getByRole("textbox", { name: /^Email/ }), world.admin.email);
        await typeIn(page.getByRole("textbox", { name: /^Password/ }), world.admin.password);
        await press(page.getByRole("button", { name: "Login" }), page, 1200);
        await say("Lost the phone? A backup code takes the place of the app's code");
        await typeIn(page.getByLabel("Authenticator or backup code"), codes[0]);
        await press(page.getByRole("button", { name: /next|login|confirm/i }).last(), page, 1500);
        await expect(page).toHaveURL(/#\/collections/);
        await say("Signed in with the backup code. That code is spent now");
    } finally {
        await scene.end();
    }
}

/** Scene 5: a scoped API key: one collection, read only. The key is shown once, used, then revoked. */
export async function apiKeyScene(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "api-key", "A scoped API key", "Read-only access to one collection, shown once, revoked in one click", [
        { name: "tomas", device: "desktop", label: "Tomas Alder (developer)" },
    ]);
    const a = scene.actor("tomas");
    const { page, say } = a;
    try {
        await ensureAdmin(page, world.admin);
        await open(page, `${world.base}/_/#/settings/api-keys`);
        await say("A billing service needs to read users, nothing else. Tomas gives it a scoped API key");
        await press(page.getByRole("button", { name: "New API key" }), page, 800);
        const dialog = page.locator(".api-key-create-modal");
        await typeIn(dialog.getByLabel("Name"), "billing-sync");
        await say("No all-access switch: the scope is users, read only");
        await dialog.getByRole("checkbox", { name: "users read", exact: true }).check();
        await beat(page, 1000);
        await press(dialog.getByRole("button", { name: "Create key" }), page, 1200);
        await expect(dialog).toContainText("shown only once");
        const key = (await dialog.locator("code").innerText()).trim();
        await say("The key is shown once. Claviger keeps only its hash", 3500);
        await press(dialog.getByRole("button", { name: "Done" }), page, 800);
        await expect(page.getByRole("row", { name: /billing-sync/ })).toBeVisible();
        await say("The list shows a prefix and the scopes, never the key");

        const shown = key.slice(0, 12) + "...";
        const read = await rawCall("GET", "/api/collections/users/records?perPage=1", undefined, `Bearer ${key}`);
        expectStatus("scoped read", read.status);
        await apiCard(a, "The billing service reads users", `GET /api/collections/users/records   Authorization: Bearer ${shown}`, read.status, { page: 1, totalItems: read.json.totalItems });
        const write = await rawCall("POST", "/api/collections/users/records", { email: "x@example.org", password: "x-pass-12345", passwordConfirm: "x-pass-12345" }, `Bearer ${key}`);
        expectStatus("scoped write", write.status, [401, 403]);
        await apiCard(a, "Writing is outside the key's scope", `POST /api/collections/users/records   Authorization: Bearer ${shown}`, write.status, write.json);
        const other = await rawCall("GET", "/api/collections/_superusers/records", undefined, `Bearer ${key}`);
        expectStatus("other collection", other.status, [401, 403]);
        await apiCard(a, "So is every other collection", `GET /api/collections/_superusers/records   Authorization: Bearer ${shown}`, other.status, other.json);

        await say("Done with it: revoke");
        await page.getByRole("row", { name: /billing-sync/ }).getByRole("button").last().click();
        await page.locator(".modal.popup.manual").getByRole("button", { name: "Yes" }).click();
        await expect(page.getByRole("row", { name: /billing-sync/ })).toContainText("Revoked");
        const after = await rawCall("GET", "/api/collections/users/records?perPage=1", undefined, `Bearer ${key}`);
        expectStatus("revoked key", after.status, [401, 403]);
        await apiCard(a, "The revoked key is refused", `GET /api/collections/users/records   Authorization: Bearer ${shown}`, after.status, after.json);
    } finally {
        await scene.end();
    }
}
