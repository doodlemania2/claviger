import { expect, type Browser } from "@playwright/test";
import { ensureHousehold, ensureParishBase, ensurePasskey, prepParish } from "../lib/parish";
import { SceneRun } from "../lib/scene";
import { api, asUser } from "../lib/stack";
import { PARISH_ORG } from "../lib/story";
import { apiCard, beat, hostedSignIn, open, press, skip, typeIn } from "../lib/ui";
import type { World } from "../lib/world";

/** Scene 5: staff-assisted recovery. A greeter vouches for an older member, who sets up a new passkey with a short code. */
export async function recoveryScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world);
    await ensureParishBase(world);
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "recovery",
        "Recovery with the help of staff",
        "A member who lost her phone gets a short code from the office and sets up a new passkey",
        [
            { name: "maren", device: "desktop", label: "Maren Castellano (greeter)", passkey: true },
            { name: "agnes", device: "phone", label: "Agnes Pembrook (member)", passkey: true },
        ],
        "row",
    );
    const maren = scene.actor("maren");
    const agnes = scene.actor("agnes");
    try {
        const p = world.people;
        await open(agnes.page, `${world.base}/recovery`);
        await agnes.say("Agnes Pembrook has lost her phone and cannot read email. She comes to the parish office");

        await ensurePasskey(maren, p.maren);
        await open(maren.page, `${world.base}/orgs/recovery/${PARISH_ORG.slug}`);
        await hostedSignIn(maren.page, p.maren);
        await maren.say("Maren carries the greeter tag, so she may vouch for people. The page is the office's recovery desk", 3800);
        await maren.page.locator("#person").selectOption({ label: p.agnes.name });
        await maren.page.locator("#issueForm select[name=verification]").selectOption("known_by_sight");
        await maren.say("She knows Agnes by sight, and says so. That answer goes into the audit log");
        await press(maren.page.locator("#issueForm button[type=submit]"), maren.page, 800);
        await expect(maren.page.locator("#codeText")).not.toBeEmpty({ timeout: 20_000 });
        const code = (await maren.page.locator("#codeText").innerText()).trim();
        await maren.say("A fresh passkey check, then a code. It lives fifteen minutes, works once, and is read aloud, never sent", 4500);

        await agnes.say("Maren reads the code out. Agnes types it on her own phone");
        await typeIn(agnes.page.locator("#codeForm input[name=code]"), code);
        await press(agnes.page.locator("#codeForm button[type=submit]"), agnes.page, 1200);
        await expect(agnes.page.locator("#enrollBtn")).toBeVisible();
        await agnes.say("The code is good: now she creates a passkey on this device");
        await typeIn(agnes.page.locator("#deviceName"), "Agnes's new phone");
        await press(agnes.page.locator("#enrollBtn"), agnes.page, 1500);
        await expect(agnes.page.locator("#done")).toBeVisible();
        await agnes.say("Done. Her old passkeys and sessions no longer work");

        await maren.say("The code is spent. Staff never saw or set a password", 3000);
        await open(agnes.page, `${world.base}/passkeys`);
        await typeIn(agnes.page.locator("#identity"), p.agnes.email);
        await press(agnes.page.locator("#btnLogin"), agnes.page, 1000);
        await expect(agnes.page.locator("#me")).toContainText(p.agnes.email);
        await agnes.say("She signs in with the new passkey");
        await beat(agnes.page, 1500);
    } finally {
        await scene.end();
    }
}

/** Scene 6: consent purposes, and a guardian giving consent for a child. */
export async function consentScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world);
    await ensureParishBase(world);
    const hh = await ensureHousehold(world);
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "consent",
        "Consent, for a child too",
        "The parish defines what it asks for; a guardian answers for a minor",
        [
            { name: "maren", device: "desktop", label: "Maren Castellano (registrar)" },
            { name: "rosa", device: "desktop", label: "Rosa Vasquez-Hale (guardian)" },
        ],
        "row",
    );
    const maren = scene.actor("maren");
    const rosa = scene.actor("rosa");
    try {
        const p = world.people;
        await open(rosa.page, `${world.base}/households/mine`);
        await rosa.say("Rosa is Teo's guardian. She will be asked about photos");

        await open(maren.page, `${world.base}/orgs/consents/${PARISH_ORG.slug}`);
        await hostedSignIn(maren.page, p.maren);
        await expect(maren.page.getByRole("heading", { name: "Consent purposes" })).toBeVisible();
        await maren.say("Registrars manage the parish's consent purposes. Each has its own text, and a version");
        await typeIn(maren.page.getByLabel("Key", { exact: true }), "parish-photos");
        await typeIn(maren.page.getByLabel("Name", { exact: true }), "Parish photos");
        await typeIn(maren.page.getByLabel("Text shown to the person"), "We may publish photos of parish events, with children's faces, on the parish notice board and website.");
        await press(maren.page.getByRole("button", { name: "Add purpose" }), maren.page, 1200);
        await expect(maren.page.locator("#pstatus")).toContainText("The purpose was added.");
        await maren.say("Added as version 1. Changing the wording later makes version 2, and earlier answers stay tied to the text they saw", 4500);

        await hostedSignIn(rosa.page, p.rosa);
        await expect(rosa.page.getByRole("heading", { name: "Vasquez-Hale family", level: 2 })).toBeVisible();
        await skip(world, "consent", "a guardian's consent screen", "the consent grant has routes (/api/me/consents) but no hosted page yet; the calls are shown with their answers", rosa);
        const list = await asUser(p.rosa, "GET", "/api/me/consents");
        const teoRow = JSON.stringify(list.json).includes(hh.teo);
        await apiCard(rosa, "What is Rosa asked about?", "GET /api/me/consents", list.status, { purposes_for_her_and_for_Teo: teoRow ? "listed, status none" : list.json });
        await rosa.say("The consent request lists the purpose for her and for her child. Nothing is pre-ticked");
        const grant = await asUser(p.rosa, "POST", "/api/me/consents/parish-photos/grant", { member: hh.teo });
        await apiCard(rosa, "Rosa agrees for Teo", "POST /api/me/consents/parish-photos/grant   {member: <Teo>}", grant.status, grant.json);
        await rosa.say("As his guardian she grants it for Teo. A minor has no login of his own", 3500);

        const hist = await asUser(p.maren, "GET", `/api/orgs/${world.ids.parish}/consents?member=${hh.teo}`);
        await apiCard(maren, "What the office can show", `GET /api/orgs/<parish>/consents?member=<Teo>`, hist.status, hist.json, 5500);
        await maren.say("The office sees the grant, who gave it, the text version and the time. Withdrawing is as easy as granting", 4500);
        void api;
        await beat(maren.page, 800);
    } finally {
        await scene.end();
    }
}
