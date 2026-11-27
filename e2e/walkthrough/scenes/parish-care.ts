import { expect, type Browser } from "@playwright/test";
import { ensureParishBase, prepParish } from "../lib/parish";
import { SceneRun } from "../lib/scene";
import { api, asUser, stack } from "../lib/stack";
import { PARISH_ORG } from "../lib/story";
import { apiCard, expectStatus, beat, ensureAdmin, hostedSignIn, open, press, typeIn } from "../lib/ui";
import { saveWorld, type World } from "../lib/world";

const modalOf = (page: import("@playwright/test").Page) => page.locator(".org-form-modal").last();

/** Scene 3: a household with a guardian and a child; the guardian edits what the child needs on the hosted page. */
export async function householdsScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world);
    await ensureParishBase(world);
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "households",
        "A household: guardian and child",
        "Staff create the family; the guardian keeps the child's details up to date",
        [
            { name: "it", device: "desktop", label: "Parish office (admin UI)" },
            { name: "rosa", device: "desktop", label: "Rosa Vasquez-Hale (guardian)" },
        ],
        "row",
    );
    const it = scene.actor("it");
    const rosa = scene.actor("rosa");
    try {
        const p = world.people;
        await open(rosa.page, `${world.base}/households/mine`);
        await rosa.say("Rosa is a parishioner. She will see her household after it is created");

        await ensureAdmin(it.page, world.admin);
        await open(it.page, `${world.base}/_/#/households?org=${world.ids.parish}`);
        // the short households clip (clip.mjs ... households households-start households-done): the family is created
        it.mark("households-start");
        await it.say("The parish office opens Households and creates a family");
        await press(it.page.getByRole("button", { name: "New household" }), it.page, 800);
        const m = modalOf(it.page);
        await typeIn(m.getByLabel("Household name"), "Vasquez-Hale family");
        await typeIn(m.getByLabel(/^First person/), p.rosa.name);
        await typeIn(m.getByLabel("Street"), "14 Willow Lane");
        await typeIn(m.getByLabel("City"), "Marlowe Vale");
        await press(m.getByRole("button", { name: "Create", exact: true }), it.page, 1200);
        await expect(it.page.locator("#household_detail")).toContainText(p.rosa.name);
        await it.say("The first person is the head of the household and a guardian");
        await beat(it.page, 1500);
        it.mark("households-done");

        // Rosa's login
        const detail = it.page.locator("#household_detail");
        await press(detail.getByRole("button", { name: `Edit ${p.rosa.name}` }), it.page, 800);
        await typeIn(modalOf(it.page).getByLabel(/^Login/), p.rosa.id as string);
        await press(modalOf(it.page).getByRole("button", { name: "Save", exact: true }), it.page, 1200);
        await expect(modalOf(it.page)).toHaveCount(0);
        await it.say("Rosa's account is linked to her row, so she can see the household when she signs in");

        // the child: a minor never gets a login
        await press(detail.getByRole("button", { name: "Add person" }), it.page, 800);
        const c = modalOf(it.page);
        await typeIn(c.getByLabel("Name", { exact: true }), "Teo Vasquez-Hale");
        await c.getByLabel("Role").selectOption("child");
        await typeIn(c.getByLabel(/^Birth date/), "2016-04-02");
        await press(c.getByRole("button", { name: "Add", exact: true }), it.page, 1200);
        await expect(detail).toContainText("Teo Vasquez-Hale");
        await expect(detail.locator("tr", { hasText: "Teo Vasquez-Hale" })).toContainText("minor");
        await it.say("Teo is a minor: no login, and his details are visible only to guardians", 3500);

        // the guardian's side
        await rosa.page.reload();
        await hostedSignIn(rosa.page, p.rosa);
        await expect(rosa.page.getByRole("heading", { name: "Vasquez-Hale family", level: 2 })).toBeVisible();
        await rosa.say("Rosa signs in: this is her household, a hosted page, no admin UI");
        const child = rosa.page.getByRole("form", { name: "Teo Vasquez-Hale" });
        await typeIn(child.getByLabel("Allergies"), "peanuts");
        await typeIn(child.getByLabel("Medical notes"), "asthma, inhaler in the school bag");
        await press(child.getByRole("button", { name: "Save" }), rosa.page, 1200);
        await expect(child.getByRole("status")).toHaveText("Saved.");
        await rosa.say("She records Teo's allergy and medical notes. These fields are encrypted at rest", 4000);
        world.notes.householdName = "Vasquez-Hale family";
        saveWorld(world);
    } finally {
        await scene.end();
    }
}

/** Scene 4: a safeguarding credential, and a roster that opens only while it is current. */
export async function safeguardingScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world);
    await ensureParishBase(world);
    // a roster collection whose rules read the credential (the gate from docs/credentials.md)
    if (!world.notes.rosterMade) {
        const orgs = await api("GET", "/api/collections/_orgs");
        const gate =
            '@collection._credentials:c.user ?= @request.auth.id && @collection._credentials:c.type ?= "safe_environment" && @collection._credentials:c.expires ?> @now && @collection._credentials:c.revoked ?= false && @collection._credentials:c.verified ?= true && @collection._credentials:c.org ?= org';
        const member = 'org.archived = false && org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active"';
        await api("DELETE", "/api/collections/youth_roster").catch(() => undefined);
        await api("POST", "/api/collections", {
            name: "youth_roster",
            type: "base",
            fields: [
                { name: "org", type: "relation", collectionId: orgs.id, maxSelect: 1, required: true },
                { name: "child", type: "text" },
                { name: "pickup", type: "text" },
            ],
            listRule: `${member} && ${gate}`,
            viewRule: `${member} && ${gate}`,
        });
        await api("POST", "/api/collections/youth_roster/records", { org: world.ids.parish, child: "Teo Vasquez-Hale", pickup: "Collected by Rosa at 4 pm" });
        world.notes.rosterMade = "yes";
        saveWorld(world);
    }
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "safeguarding",
        "A safeguarding credential",
        "Access to the youth roster opens when a current credential exists, and closes when it lapses",
        [
            { name: "it", device: "desktop", label: "Parish office (admin UI)" },
            { name: "ellis", device: "desktop", label: "Ellis Fenner (youth volunteer)" },
        ],
        "row",
    );
    const it = scene.actor("it");
    const ellis = scene.actor("ellis");
    try {
        const p = world.people;
        await open(ellis.page, `${world.base}/credentials/mine`);
        await hostedSignIn(ellis.page, p.ellis);
        await expect(ellis.page.locator("body")).toContainText("Nothing recorded yet").catch(() => undefined);
        await ellis.say("Ellis Fenner wants to help with the youth group. His page shows what he holds: so far, nothing");
        const roster = await asUser(p.ellis, "GET", "/api/collections/youth_roster/records");
        expect(roster.json.totalItems ?? 0).toBe(0);
        await apiCard(ellis, "The youth roster, asked for by Ellis", "GET /api/collections/youth_roster/records", roster.status, { items: roster.json.items, totalItems: roster.json.totalItems });
        await ellis.say("The roster rule reads credentials: no current credential, no rows. No code decides this, only a rule", 4200);

        await ensureAdmin(it.page, world.admin);
        await open(it.page, `${world.base}/_/#/credentials?org=${world.ids.parish}`);
        await it.say("The office defines what the parish recognizes: a safe environment credential, valid for twelve months");
        await press(it.page.getByRole("button", { name: "New type" }), it.page, 800);
        const m = modalOf(it.page);
        await typeIn(m.getByLabel("Key"), "safe_environment");
        await typeIn(m.getByLabel("Name", { exact: true }), "Safe environment");
        await typeIn(m.getByLabel(/^Valid for/), "12");
        await press(m.getByRole("button", { name: "Create", exact: true }), it.page, 1200);
        await expect(it.page.locator('tr[data-type-key="safe_environment"]')).toBeVisible();

        // the short safeguarding clip (clip.mjs ... safeguarding safeguarding-start safeguarding-done): a credential is recorded and the roster opens
        it.mark("safeguarding-start");
        await press(it.page.getByRole("button", { name: "Record credential" }), it.page, 800);
        const r = modalOf(it.page);
        await r.getByLabel("Holder").selectOption({ label: p.ellis.name });
        await r.getByLabel("Credential type").selectOption({ label: "Safe environment" });
        await typeIn(r.getByLabel("Issued by (optional)"), "Diocesan safeguarding office");
        await it.say("Ellis's training and check came back clear. The credential is an attestation, not the check itself", 4000);
        await press(r.getByRole("button", { name: "Record", exact: true }), it.page, 1200);
        await expect(it.page.locator("tr[data-credential-id]", { hasText: p.ellis.name })).toContainText("valid");

        await ellis.page.reload();
        await expect(ellis.page.locator("li.cred", { hasText: "Safe environment" })).toContainText("Current");
        await ellis.say("Ellis sees it: current, with its end date");
        const after = await asUser(p.ellis, "GET", "/api/collections/youth_roster/records");
        expect(after.json.totalItems).toBe(1);
        await apiCard(ellis, "The same request, now", "GET /api/collections/youth_roster/records", after.status, { totalItems: after.json.totalItems, items: after.json.items.map((i: any) => ({ child: i.child, pickup: i.pickup })) });
        await ellis.say("The rule found a current credential: the roster opens. When it expires, or a coordinator revokes it, it closes by itself", 5000);
        await beat(it.page, 500);
        it.mark("safeguarding-done");
    } finally {
        await scene.end();
    }
}
