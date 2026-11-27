import { expect, type Browser } from "@playwright/test";
import { SceneRun } from "../lib/scene";
import { api, stack } from "../lib/stack";
import { beat, ensureAdmin, open, press, skip, typeIn } from "../lib/ui";
import type { World } from "../lib/world";

const tomasSpec = [{ name: "tomas", device: "desktop" as const, label: "Tomas Alder (developer)" }];

/** Scene 8: the audit log. */
export async function auditScene(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "audit", "The audit log", "Who changed what, with before and after values", tomasSpec);
    const { page, say } = scene.actor("tomas");
    try {
        await ensureAdmin(page, world.admin);
        await open(page, `${world.base}/_/#/audit`);
        await expect(page.getByRole("table")).toContainText("create");
        await say("Every write is an audit entry: action, collection, record, actor and address. Passwords and tokens are redacted", 4500);
        await typeIn(page.getByLabel("Collection"), "users");
        await page.getByLabel("Collection").press("Enter");
        await beat(page, 1500);
        await say("Filtered to the users collection");
        const row = page.locator("tbody tr", { hasText: "users" }).first();
        await press(row, page, 1000);
        await expect(page.getByRole("heading", { name: "Audit entry" })).toBeVisible();
        await say("An entry shows exactly what changed. The same entries feed the webhooks and the data export", 4500);
        await beat(page, 1500);
        await press(page.getByRole("button", { name: "Close" }), page, 600);
    } finally {
        await scene.end();
    }
}

/** Scene 9: encrypted fields, the ciphertext at rest, and the encryption keys page. */
export async function fieldEncryptionScene(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "field-encryption", "Field encryption keys", "An encrypted field, its ciphertext at rest, and key rotation", tomasSpec);
    const { page, say, mark } = scene.actor("tomas");
    try {
        await api("DELETE", "/api/collections/customers").catch(() => undefined);
        await api("POST", "/api/collections", { name: "customers", type: "base", fields: [{ name: "company", type: "text" }] });
        await ensureAdmin(page, world.admin);
        await open(page, `${world.base}/_/#/collections?collection=customers`);
        await say("A customers collection holds a phone number that must be unreadable in the database file");
        await page.getByRole("button", { name: "Collection settings" }).click();
        const editor = page.locator(".modal", { hasText: "Edit customers collection" }).last();
        await beat(page, 800);
        await editor.getByRole("button", { name: "New field" }).click();
        await say("Besides the usual field types, Claviger has Encrypted text and Encrypted JSON");
        await beat(page, 1200);
        await page.locator(".field-types-dropdown").getByRole("button", { name: "Encrypted text" }).click();
        await beat(page, 800);
        await press(editor.getByRole("button", { name: "Save changes" }), page, 1200);
        await expect(editor).toBeHidden();

        await page.locator(".new-record-btn").click();
        await expect(page.getByRole("heading", { name: "Create customers record" })).toBeVisible();
        await typeIn(page.getByRole("textbox", { name: "company", exact: true }), "Larkspur Tools");
        await typeIn(page.getByRole("textbox", { name: "encryptedText", exact: true }), "contact: +1 555 0142");
        await press(page.getByRole("button", { name: "Create", exact: true }), page, 1200);
        await expect(page.getByRole("heading", { name: "Create customers record" })).toBeHidden();
        // the short field-encryption clip (clip.mjs ... field-encryption field-encryption-start field-encryption-done)
        mark("field-encryption-start");
        await say("The API and this screen read the value back in clear, for people who may");

        await open(page, `${world.base}/_/#/settings/sql`);
        await say("But the database holds ciphertext. The SQL console shows the stored column", 3000);
        await page.locator(".sql-console-field [contenteditable], .sql-console-field textarea").first().click();
        await page.keyboard.type("select company, encryptedText from customers", { delay: 10 });
        await press(page.getByRole("button", { name: "Execute" }), page, 1500);
        await expect(page.locator("body")).toContainText("enc:v1:");
        await say("enc:v1: followed by an AES-256-GCM sealed value. A stolen file or backup shows nothing", 4500);
        mark("field-encryption-done");

        await open(page, `${world.base}/_/#/settings/encryption-keys`);
        const row = page.getByRole("row", { name: /customers\.encryptedText/ });
        await expect(row).toBeVisible();
        await say("The keys page lists a data key per field, wrapped by the key-encryption key from the environment", 4500);
        const before = await row.locator("code").first().innerText();
        await press(row.getByRole("button", { name: /rotate/i }), page, 800);
        const yes = page.locator(".modal.popup.manual").getByRole("button", { name: "Yes" });
        if (await yes.isVisible().catch(() => false)) await yes.click();
        await expect(row.locator("code").first()).not.toHaveText(before, { timeout: 20_000 });
        await say("Rotating a field's key re-encrypts its rows in the background. One audit entry records the run", 4500);
    } finally {
        await scene.end();
    }
}

/** Scene 10: an encrypted backup and its restore verification. */
export async function backupScene(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "backup", "A backup with restore verification", "Encrypted at rest, and proven restorable", tomasSpec);
    const { page, say } = scene.actor("tomas");
    try {
        await ensureAdmin(page, world.admin);
        await open(page, `${world.base}/_/#/settings/backups`);
        await say("Backups: Tomas turns on encryption, so a copied backup is useless without the key");
        await page.getByRole("button", { name: "Backup options" }).click();
        await beat(page, 1000);
        await expect(async () => {
            await page.getByLabel("Encrypt new backups").setChecked(true, { force: true, timeout: 2000 });
            await expect(page.getByLabel("Encrypt new backups")).toBeChecked({ timeout: 1000 });
        }).toPass({ timeout: 15_000 });
        await press(page.getByRole("button", { name: "Save changes" }), page, 1500);
        await expect.poll(async () => (await api("GET", "/api/settings")).backups.encrypt).toBe(true);
        await page.getByRole("button", { name: "Initialize new backup" }).click();
        const dialog = page.locator(".modal", { hasText: "Initialize new backup" }).last();
        await typeIn(dialog.getByLabel(/Backup name/i), "walkthrough.zip");
        await press(dialog.getByRole("button", { name: /Start backup|Create/i }), page, 800);
        const item = page.locator(".list-item", { hasText: "walkthrough.zip.enc" });
        await expect(item).toBeVisible({ timeout: 30_000 });
        await say("The backup is stored as .zip.enc: it is not a zip until it is decrypted with the key");
        await item.hover();
        await beat(page, 1000);
        await say("Verify restore opens the archive in a scratch directory and checks the databases");
        await item.getByRole("button", { name: "Verify restore" }).click();
        await expect(item.locator(".backup-verify.txt-success")).toBeVisible({ timeout: 90_000 });
        await say("Verified: the backup decrypts and restores. Do this before the day you need it", 4000);
        await beat(page, 1500);
    } finally {
        await scene.end();
    }
}

/** Scene 11: telemetry has no settings screen: say so, and show the metrics endpoint that is configured by environment. */
export async function telemetryScene(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "telemetry", "Telemetry", "Configured by environment, not by a screen", tomasSpec);
    const a = scene.actor("tomas");
    try {
        const { rig, metricsToken } = stack();
        await open(a.page, `${rig.url}/blank`);
        await skip(world, "telemetry", "telemetry settings screen", "OTLP logs, traces and metrics are configured by environment variables (OTEL_EXPORTER_OTLP_ENDPOINT and friends); the admin UI has no telemetry page", a);
        const res = await fetch(`${world.base}/api/metrics`, { headers: { Authorization: `Bearer ${metricsToken}` } });
        const text = await res.text();
        const lines = text.split("\n").filter((l) => l && !l.startsWith("#")).slice(0, 7);
        await a.card("With CLAVIGER_METRICS_TOKEN set, /api/metrics serves Prometheus text", [`-> GET /api/metrics   Authorization: Bearer <token>`, `<- ${res.status}`, ...lines]);
        await a.say("The one telemetry piece that is on here: a Prometheus endpoint behind a static bearer token", 5000);
        await beat(a.page, 3000);
    } finally {
        await scene.end();
    }
}
