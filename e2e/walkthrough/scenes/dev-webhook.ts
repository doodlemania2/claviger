import { expect, type Browser } from "@playwright/test";
import { SceneRun } from "../lib/scene";
import { api, stack } from "../lib/stack";
import { beat, ensureAdmin, open, press, typeIn } from "../lib/ui";
import type { World } from "../lib/world";

/** Scene 7: a signed webhook to a receiver, then its delivery log. */
export async function webhookScene(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "webhook",
        "A signed webhook and its delivery log",
        "Claviger tells a CRM when a user is created, and signs every delivery",
        [
            { name: "tomas", device: "desktop", label: "Tomas Alder (developer)" },
            { name: "crm", device: "desktop", label: "The CRM receiver (a local page)" },
        ],
        "row",
    );
    const tomas = scene.actor("tomas");
    const crm = scene.actor("crm");
    try {
        const { rig } = stack();
        await open(crm.page, `${rig.url}/received`);
        await crm.say("On the right, a small CRM endpoint on this machine. It checks the signature of everything it receives");

        await ensureAdmin(tomas.page, world.admin);
        await open(tomas.page, `${world.base}/_/#/webhooks`);
        await tomas.say("Tomas wants the CRM told whenever a user is created");
        await press(tomas.page.getByRole("button", { name: "New webhook" }), tomas.page, 800);
        await typeIn(tomas.page.getByLabel("URL (https)"), `${rig.url}/hook`);
        await typeIn(tomas.page.getByLabel("Description"), "CRM sync");
        await typeIn(tomas.page.getByLabel("Events (one per line)"), "users:create");
        await tomas.say("Production endpoints must be https. This local one is allowed only because the rig sets a development flag", 4200);
        await press(tomas.page.getByRole("button", { name: "Create" }), tomas.page, 1200);
        const secret = await tomas.page.getByLabel("Signing secret").inputValue();
        expect(secret.length).toBeGreaterThan(16);
        rig.hookSecrets = [secret];
        await tomas.say("The signing secret is shown once. The CRM is configured with it");
        await press(tomas.page.getByRole("button", { name: "Done" }), tomas.page, 800);
        const row = tomas.page.getByRole("row", { name: /\/hook/ });
        await expect(row).toBeVisible();

        // the short webhook clip (clip.mjs ... webhook webhook-start webhook-done): a test event, then a real one, both verified
        tomas.mark("webhook-start");
        await tomas.say("A test event first");
        await press(row.getByRole("button", { name: "Send test event" }), tomas.page, 2500);
        await expect.poll(() => rig.deliveries.length, { timeout: 15_000 }).toBeGreaterThan(0);
        await crm.say("The CRM received webhook.test and verified the signature");
        await beat(crm.page, 2500);

        await tomas.say("Now a real event: a new user is created");
        const mira = await api("POST", "/api/collections/users/records", { email: "mira@example.org", password: "walk-mira-pass-2026", passwordConfirm: "walk-mira-pass-2026", name: "Mira Voss", verified: true });
        await expect.poll(() => rig.deliveries.some((d) => d.type === "record.created"), { timeout: 30_000 }).toBe(true);
        await crm.say("record.created arrives with the new record. Passwords and secrets are redacted by Claviger", 4000);
        await beat(crm.page, 2000);
        void mira;
        tomas.mark("webhook-done");

        await tomas.page.reload();
        await tomas.page.waitForLoadState("networkidle").catch(() => undefined);
        await tomas.page.getByText("Recent deliveries").scrollIntoViewIfNeeded();
        await tomas.say("The delivery log keeps every attempt: status, attempts and errors. A failed delivery can be redelivered", 4500);
        await beat(tomas.page, 1500);
    } finally {
        await scene.end();
    }
}
