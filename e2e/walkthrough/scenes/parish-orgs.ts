import { expect, type Browser } from "@playwright/test";
import { ensureParishBase, ensureOrg, orgBySlug, prepParish } from "../lib/parish";
import { SceneRun } from "../lib/scene";
import { api, stack } from "../lib/stack";
import { DIOCESE, PARISH_ORG, SECOND_PARISH } from "../lib/story";
import { beat, ensureAdmin, hostedSignIn, open, openMail, press, typeIn } from "../lib/ui";
import { saveWorld, type World } from "../lib/world";

/** Scene 1: the superuser creates the diocese and the parish as organizations in a tree. */
export async function orgsScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world, ["maren"]);
    const scene = await SceneRun.begin(browser, world, index, "orgs", "A diocese and its parish", "The instance operator creates the organization tree", [
        { name: "it", device: "desktop", label: "Diocesan IT (superuser)" },
    ]);
    const { page, say } = scene.actor("it");
    try {
        const p = world.people;
        await ensureAdmin(page, world.admin);
        await open(page, `${world.base}/_/#/orgs`);
        await say("Claviger organizations form a tree. The operator starts at the top, with the diocese");
        const modal = page.locator(".org-form-modal");
        await press(page.getByRole("button", { name: "New organization" }), page, 800);
        await typeIn(modal.getByLabel("Name", { exact: true }), DIOCESE.name);
        await typeIn(modal.getByLabel("Slug"), DIOCESE.slug);
        await typeIn(modal.getByLabel(/^Owner/), p.hugo.email);
        await typeIn(modal.getByLabel("Kind"), "diocese");
        await say("Its owner is the bishop's delegate, Hugo Marsh. The kind is a free label");
        await press(modal.getByRole("button", { name: "Create", exact: true }), page, 1200);
        await expect(page.locator(".org-form-modal")).toHaveCount(0);
        await expect(page.getByRole("row", { name: new RegExp(DIOCESE.name) })).toBeVisible();
        const diocese = await orgBySlug(DIOCESE.slug);

        await press(page.getByRole("button", { name: "New organization" }), page, 800);
        await typeIn(modal.getByLabel("Name", { exact: true }), PARISH_ORG.name);
        await typeIn(modal.getByLabel("Slug"), PARISH_ORG.slug);
        await typeIn(modal.getByLabel(/^Owner/), p.basil.email);
        await modal.getByLabel("Parent organization").selectOption(diocese.id);
        await typeIn(modal.getByLabel("Kind"), "parish");
        await say("The parish sits under the diocese, with its own owner: the pastor, Fr. Basil Thorne");
        await press(modal.getByRole("button", { name: "Create", exact: true }), page, 1200);
        await expect(page.locator(".org-form-modal")).toHaveCount(0);
        await expect(page.getByRole("row", { name: new RegExp(PARISH_ORG.name) })).toBeVisible();

        await ensureOrg(SECOND_PARISH.slug, { name: SECOND_PARISH.name, kind: "parish", parent: diocese.id, owner: p.hugo.email });
        await page.getByRole("button", { name: "Refresh" }).first().click();
        await say("A second parish, Wayside Chapel, exists too: it is used for a transfer later");
        await ensureParishBase(world, { orgs: true, staff: false });
        await page.getByRole("row", { name: new RegExp(PARISH_ORG.name) }).click();
        await beat(page, 1500);
        await say("Selecting an org shows its members and its status: counts only, never names, for the tree above it", 4000);
    } finally {
        await scene.end();
    }
}

/** Scene 2: the owner invites staff, sets roles and tags. */
export async function staffScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world, ["maren"]);
    await ensureParishBase(world, { staff: false });
    // the people who are already members before the story reaches them
    const { ensureMember } = await import("../lib/parish");
    for (const k of ["dora", "ignatius", "ellis", "rosa", "agnes", "tobias"] as const) await ensureMember(world.ids.parish, world.people[k].email, "member", []);
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "staff",
        "Staff, roles and tags",
        "The pastor invites the parish secretary, and gives roles and tags",
        [
            { name: "basil", device: "desktop", label: "Fr. Basil Thorne (owner)" },
            { name: "maren", device: "desktop", label: "Maren Castellano (new staff)" },
        ],
        "row",
    );
    const basil = scene.actor("basil");
    const maren = scene.actor("maren");
    try {
        const { rig } = stack();
        const p = world.people;
        await open(maren.page, `${rig.url}/mail?to=${encodeURIComponent(p.maren.email)}`);
        await maren.say("Maren Castellano is the parish secretary. She has no account yet");

        await open(basil.page, `${world.base}/orgs/${PARISH_ORG.slug}`);
        await basil.say("The pastor opens the parish page. It is for the people who run the parish: no admin UI needed");
        await hostedSignIn(basil.page, p.basil);
        await expect(basil.page.locator("#myRole")).toContainText("owner");
        await basil.say("Members, roles, tags and invitations are managed here");

        const tagForm = basil.page.locator("form#tagForm");
        for (const t of ["registrar", "greeter", "safeguarding"]) {
            await typeIn(tagForm.getByLabel("New tag"), t);
            await press(tagForm.getByRole("button"), basil.page, 700);
        }
        await basil.say("Tags are labels: registrar signs register entries, greeter may vouch for people, safeguarding records credentials", 4500);

        const inv = basil.page.locator("form#inviteForm");
        await typeIn(inv.getByLabel("Email"), p.maren.email);
        await inv.locator("#inviteRole").selectOption("admin");
        await typeIn(inv.getByLabel(/Tags/), "registrar, greeter");
        await press(inv.getByRole("button", { name: "Send invitation" }), basil.page, 1200);
        await expect(basil.page.locator("#invitations")).toContainText(p.maren.email);
        await basil.say("An invitation by email: role admin, with two tags");

        // Maren's side: the mail, the link, the account
        await open(maren.page, `${rig.url}/mail?to=${encodeURIComponent(p.maren.email)}`);
        await stack().smtp.waitFor((m) => m.to.includes(p.maren.email));
        await open(maren.page, `${rig.url}/mail?to=${encodeURIComponent(p.maren.email)}`);
        await maren.say("The invitation arrives. In the real world this is her inbox");
        await press(maren.page.locator("a[href*='/orgs/invite/']").first(), maren.page, 1200);
        await expect(maren.page.getByRole("heading", { name: new RegExp(`Join ${PARISH_ORG.name}`) })).toBeVisible();
        const signup = maren.page.locator("form#signupForm");
        await typeIn(signup.getByLabel("Name"), p.maren.name);
        await typeIn(signup.getByLabel("Password", { exact: true }), p.maren.password);
        await typeIn(signup.getByLabel("Repeat the password"), p.maren.password);
        await maren.say("The invitation is tied to her address. She sets a name and a password, and joins in one step");
        await press(signup.getByRole("button", { name: "Create the account and join" }), maren.page, 1500);
        await expect(maren.page.locator("#doneText")).toContainText(`You joined ${PARISH_ORG.name}`);

        // back to the owner: Maren is on the list; Dora gets her tag
        await basil.page.reload();
        await basil.page.waitForLoadState("networkidle").catch(() => undefined);
        await expect(basil.page.locator("#members")).toContainText("Maren");
        await basil.say("Maren is a member now, as admin, with her tags");
        const dora = basil.page.locator("#members tr", { hasText: "Dora Quill" });
        await typeIn(dora.locator("input"), "safeguarding");
        await press(dora.getByRole("button", { name: "Save" }), basil.page, 1200);
        await basil.say("Dora Quill, a volunteer, becomes the safeguarding coordinator with one tag", 3500);
        const { ensureMember } = await import("../lib/parish");
        const { ORG_ROLES } = await import("../lib/parish");
        for (const [k, w] of Object.entries(ORG_ROLES)) if (k !== "basil" && k !== "maren" && k !== "dora") await ensureMember(world.ids.parish, p[k].email, w.role, [...w.tags]);
        void api;
        saveWorld(world);
    } finally {
        await scene.end();
    }
}
