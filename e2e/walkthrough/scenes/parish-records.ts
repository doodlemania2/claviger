import { expect, type Browser } from "@playwright/test";
import { ensureHousehold, ensureParishBase, ensurePasskey, prepParish } from "../lib/parish";
import { SceneRun } from "../lib/scene";
import { api, asUser, rawCall, stack } from "../lib/stack";
import { PARISH_ORG, SECOND_PARISH } from "../lib/story";
import { apiCard, expectStatus, beat, ensureAdmin, hostedSignIn, open, openMail, press, skip, stepUpCall, typeIn } from "../lib/ui";
import { saveWorld, type World } from "../lib/world";

/** Scene 7: a sacramental register entry with two signers, an annotation and a certificate. */
export async function registerScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world);
    await ensureParishBase(world);
    if (!world.ids.baptism) {
        const have = await rawCall("GET", "/api/registers/baptisms/__none", undefined, await (await import("../lib/stack")).adminToken());
        void have;
        await api("POST", "/api/registers", { template: "sacramental", name: "baptisms", signers: 2, retainReason: "A sacramental record is kept permanently: it is the church's own record of an event." }).catch(() => undefined);
        const rec = await api("POST", "/api/collections/baptisms/records", { org: world.ids.parish, sacrament: "baptism", surname: "Vasquez-Hale", givenNames: "Teodoro" });
        world.ids.baptism = rec.id;
        saveWorld(world);
    }
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "register",
        "A sacramental register",
        "Two registrars sign an entry with passkeys; a note in the margin; a certificate",
        [
            { name: "maren", device: "desktop", label: "Maren Castellano (registrar)", passkey: true },
            { name: "basil", device: "desktop", label: "Fr. Basil Thorne (registrar)", passkey: true },
        ],
        "row",
    );
    const maren = scene.actor("maren");
    const basil = scene.actor("basil");
    try {
        const p = world.people;
        const id = world.ids.baptism;
        await ensurePasskey(maren, p.maren);
        await ensurePasskey(basil, p.basil);

        const openRegisters = async (a: typeof maren, who: typeof p.maren) => {
            await open(a.page, `${world.base}/orgs/registers/${PARISH_ORG.slug}`);
            await hostedSignIn(a.page, who);
            await expect(a.page.getByRole("heading", { name: "Registers", exact: true })).toBeVisible();
            await a.page.locator("#regSelect").selectOption("baptisms");
        };
        await openRegisters(maren, p.maren);
        await skip(world, "register", "keying a new entry on screen", "no screen creates register entries yet: they arrive through the records API, so this draft was entered beforehand", maren);
        await maren.say("The parish keeps a baptism register. Maren, a registrar, opens the entry that was keyed in", 3800);
        await beat(maren.page, 800);
        await press(maren.page.locator(`tr[data-entry='${id}']`).getByRole("button", { name: /Open/ }), maren.page, 1000);
        await expect(maren.page.locator("#finalize")).toHaveText("Put forward for a second signature");
        await maren.say("This register needs two signers. The first puts the entry forward, with a passkey");
        maren.page.once("dialog", (d) => void d.accept());
        await press(maren.page.locator("#finalize"), maren.page, 1500);
        await expect(maren.page.locator("#pstatus")).toContainText("waiting for a second signer");
        await maren.say("It is pending. Maren cannot confirm her own request");

        await openRegisters(basil, p.basil);
        await basil.page.locator("#stateSelect").selectOption("pending");
        await basil.say("Fr. Basil, a different registrar, finds it among the pending entries");
        await press(basil.page.locator(`tr[data-entry='${id}']`).getByRole("button", { name: /Open/ }), basil.page, 1000);
        await expect(basil.page.locator("#finalize")).toHaveText("Confirm and finalize");
        basil.page.once("dialog", (d) => void d.accept());
        await press(basil.page.locator("#finalize"), basil.page, 1500);
        await expect(basil.page.locator("#pstatus")).toContainText("The entry is final.");
        await expect(basil.page.locator("#facts")).toContainText("Register number");
        await basil.say("Confirmed with his own passkey: the entry is final, numbered, and chained to the one before by a hash", 4500);

        const form = basil.page.locator("#noteForm");
        await form.getByLabel("Kind of note").selectOption("correction");
        await typeIn(form.getByLabel("Note", { exact: true }), "Given name is spelled Teodoro on the parish record");
        await typeIn(form.getByLabel(/^Reason/), "Seen against the baptismal card");
        await basil.say("A final entry is never edited. A correction goes in the margin, signed, with its reason", 4000);
        await press(form.getByRole("button", { name: "Add the note" }), basil.page, 1500);
        await expect(basil.page.locator("#notes li").first()).toContainText("Given name is spelled");

        await basil.page.locator("#certBtn").click();
        await expect(basil.page.frameLocator("#certFrame").locator("h1")).toHaveText("Register entry certificate");
        await basil.say("The certificate: the entry, its margin and its place in the chain, ready to print", 4500);
        await beat(basil.page, 1500);
        await basil.page.locator("#certClose").click();
    } finally {
        await scene.end();
    }
}

/** Scene 8: pastoral notes sealed to their author, and a break-glass request with two clergy. */
export async function pastoralScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world);
    await ensureParishBase(world);
    const p = world.people;
    if (!world.notes.pastoralMade) {
        const made = await rawCall("POST", "/api/pastoral", { name: "pastoral_notes", orgs: [world.ids.parish] }, await (await import("../lib/stack")).adminToken());
        if (made.status >= 300 && made.status !== 409) throw new Error(`pastoral setup: ${made.status} ${made.text}`);
        const note = await asUser(p.dora, "POST", "/api/collections/pastoral_notes/records", {
            org: world.ids.parish,
            author: p.dora.id,
            visibility: "private",
            sealed: true,
            kind: "care",
            body: "Visited a parishioner after the funeral. Worried about their safety at home; asked that nobody else be told yet.",
        });
        if (note.status >= 300) throw new Error(`note: ${note.status} ${note.text}`);
        world.ids.note = note.json.id;
        world.notes.pastoralMade = "yes";
        saveWorld(world);
    }
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "pastoral",
        "Pastoral notes and break-glass",
        "A sealed note nobody else can read, until two clergy open it in an emergency",
        [
            { name: "basil", device: "desktop", label: "Fr. Basil Thorne (pastor)", passkey: true },
            { name: "ignatius", device: "desktop", label: "Fr. Ignatius Wren (clergy)", passkey: true },
        ],
        "row",
    );
    const basil = scene.actor("basil");
    const ign = scene.actor("ignatius");
    try {
        const note = world.ids.note;
        await ensurePasskey(basil, p.basil);
        await ensurePasskey(ign, p.ignatius);
        for (const [a, who] of [[basil, p.basil], [ign, p.ignatius]] as const) {
            await open(a.page, `${world.base}/orgs/${PARISH_ORG.slug}`);
            await hostedSignIn(a.page, who);
        }
        await basil.say("Dora Quill wrote a sealed pastoral note. Sealed means: its author only. The pastor is the org's owner");
        const before = await asUser(p.basil, "GET", `/api/collections/pastoral_notes/records/${note}`);
        expectStatus("the owner reading a sealed note", before.status, [404, 403]);
        await apiCard(basil, "The pastor asks for the note", `GET /api/collections/pastoral_notes/records/${note}`, before.status, before.json);
        await basil.say("An owner has no automatic right to pastoral notes. The body is encrypted, and no rule names the owner", 4500);

        await basil.say("Someone may be at risk and Dora cannot be reached. Break-glass: always two people, never silent", 4200);
        const ask = await stepUpCall(basil.page, "users", "POST", "/api/breakglass", {
            collection: "pastoral_notes",
            recordId: note,
            reason: "A person may be at risk and the author cannot be reached today.",
            basis: "safety",
            hours: 2,
        });
        expectStatus("break-glass request", ask.status, [200, 201]);
        await apiCard(basil, "Fr. Basil asks, with a fresh passkey", "POST /api/breakglass   {basis: safety, hours: 2, reason: ...}", ask.status, ask.body);
        await basil.say("A request only records the need. Nothing is open yet, and he cannot approve his own", 3800);

        const grantId = ask.body.id;
        await ign.say("Fr. Ignatius, a second holder of the breakglass tag, reviews the request");
        const approve = await stepUpCall(ign.page, "users", "POST", `/api/breakglass/${grantId}/approve`, {});
        expectStatus("break-glass approval", approve.status);
        await apiCard(ign, "Fr. Ignatius approves, with his own passkey", `POST /api/breakglass/${grantId}/approve`, approve.status, approve.body);
        await ign.say("Before the grant opens Claviger emails the author and the owners. That notice cannot be turned off", 4500);

        await openMail(basil.page, p.basil.email, /emergency|break|access|note/i);
        await basil.say("The pastor's notice: who asked, who approved, why, and when the access ends");
        await beat(basil.page, 2500);

        await open(basil.page, `${world.base}/orgs/${PARISH_ORG.slug}`);
        await hostedSignIn(basil.page, p.basil);
        const read = await asUser(p.basil, "GET", `/api/collections/pastoral_notes/records/${note}`);
        expectStatus("reading under the grant", read.status);
        await apiCard(basil, "While the grant is live, he can read it", `GET /api/collections/pastoral_notes/records/${note}`, read.status, { body: read.json.body, sealed: read.json.sealed }, 5500);
        await basil.say("Every read is logged and counted. The grant ends by the clock, within 24 hours at most", 4200);

        const audit = await api("GET", `/api/collections/_auditLog/records?perPage=10&sort=-created&filter=${encodeURIComponent('action ~ "breakglass" || action = "read"')}`);
        expect(audit.items.map((i: any) => i.action)).toContain("breakglass_open");
        await apiCard(ign, "The audit trail", "GET /api/collections/_auditLog/records  (breakglass_request, breakglass_open, read)", 200, audit.items.map((i: any) => i.action), 5500);
        await skip(world, "pastoral", "a dedicated break-glass screen", "break-glass has routes and emails but no page; the steps are shown as API calls with their answers", ign);
        void stack;
    } finally {
        await scene.end();
    }
}

/** Scene 9: a member moves to another parish of the diocese, with their own consent. */
export async function transferScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world);
    await ensureParishBase(world);
    const p = world.people;
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "transfer",
        "An organization transfer",
        "Tobias moves to Wayside Chapel; the parish starts it, he accepts it",
        [
            { name: "maren", device: "desktop", label: "Maren Castellano (registrar)" },
            { name: "tobias", device: "desktop", label: "Tobias Reed (member)" },
        ],
        "row",
    );
    const maren = scene.actor("maren");
    const tobias = scene.actor("tobias");
    try {
        const { rig } = stack();
        await open(maren.page, `${world.base}/orgs/${PARISH_ORG.slug}`);
        await hostedSignIn(maren.page, p.maren);
        await maren.say("Tobias Reed is moving house, to the next parish. Maren, a registrar, starts a transfer");
        await open(tobias.page, `${rig.url}/mail?to=${encodeURIComponent(p.tobias.email)}`);
        await tobias.say("Tobias is a member of Holy Wayfarer. Nothing happens to him without his consent");

        await skip(world, "transfer", "transfer screens", "an organization transfer has routes and an email but no page yet; the calls are shown with their answers", maren);
        const start = await asUser(p.maren, "POST", `/api/orgs/${world.ids.parish}/transfers`, { user: p.tobias.id, to: SECOND_PARISH.slug, outcome: "suspend" });
        if (start.status >= 300) throw new Error(`transfer: ${start.status} ${start.text}`);
        await apiCard(maren, "Maren starts the transfer", `POST /api/orgs/<holy-wayfarer>/transfers   {user: <Tobias>, to: "${SECOND_PARISH.slug}", outcome: "suspend"}`, start.status, start.json);
        await maren.say("A transfer is pending. The old membership will be suspended, not deleted, so history stays visible", 4200);

        await openMail(tobias.page, p.tobias.email);
        await tobias.say("Tobias gets a notice. Staff can start a transfer; only he can accept it", 3800);
        await beat(tobias.page, 800);
        await open(tobias.page, `${world.base}/orgs/${PARISH_ORG.slug}`);
        await hostedSignIn(tobias.page, p.tobias);
        const accept = await asUser(p.tobias, "POST", `/api/orgs/${world.ids.parish}/transfers/${start.json.id}/accept`, {});
        if (accept.status >= 300) throw new Error(`accept: ${accept.status} ${accept.text}`);
        await apiCard(tobias, "Tobias accepts, signed in as himself", `POST /api/orgs/<holy-wayfarer>/transfers/<id>/accept`, accept.status, accept.json, 5000);
        await tobias.say("One transaction: the new membership, the old one suspended, one audit entry. If a step fails, nothing changes", 4500);

        await open(tobias.page, `${world.base}/orgs/${SECOND_PARISH.slug}`);
        await hostedSignIn(tobias.page, p.tobias);
        await expect(tobias.page.locator("#orgName")).toHaveText(SECOND_PARISH.name);
        await tobias.say("He is now a member of Wayside Chapel");
        await beat(tobias.page, 2000);
        saveWorld(world);
    } finally {
        await scene.end();
    }
}

/** Scene 10: the Data protection screens: data map, an export, a legal hold, a retention preview and an erasure. */
export async function dataProtectionScene(browser: Browser, world: World, index: number): Promise<void> {
    await prepParish(world);
    await ensureParishBase(world);
    const hh = await ensureHousehold(world);
    if (!world.ids.roster) {
        const orgs = await api("GET", "/api/collections/_orgs");
        await api("DELETE", "/api/collections/youth_roster").catch(() => undefined);
        await api("POST", "/api/collections", {
            name: "youth_roster",
            type: "base",
            fields: [
                { name: "org", type: "relation", collectionId: orgs.id, maxSelect: 1, required: true },
                { name: "child", type: "text" },
                { name: "pickup", type: "text" },
                { name: "created", type: "autodate", onCreate: true },
            ],
        });
        world.ids.roster = (await api("POST", "/api/collections/youth_roster/records", { org: world.ids.parish, child: "Teo Vasquez-Hale", pickup: "Collected by Rosa at 4 pm" })).id;
        saveWorld(world);
    }
    // the parishioner who asks to be forgotten: a membership and a consent
    if (!world.ids.odette) {
        const o = await api("POST", "/api/collections/users/records", { email: "odette@example.org", password: "walk-odette-pass-2026", passwordConfirm: "walk-odette-pass-2026", verified: true, name: "Odette Brandt" });
        world.ids.odette = o.id;
        await rawCall("POST", `/api/orgs/${world.ids.parish}/members`, { user: o.id, role: "member" }, await (await import("../lib/stack")).adminToken());
        saveWorld(world);
    }
    const scene = await SceneRun.begin(browser, world, index, "data-protection", "Data protection", "Data map, an export, a legal hold, retention and an erasure", [
        { name: "it", device: "desktop", label: "Diocesan IT (superuser)", passkey: true },
    ]);
    const a = scene.actor("it");
    const { page, say } = a;
    try {
        const p = world.people;
        await api("PATCH", "/api/collections/_superusers", { webauthn: { enabled: true, discoverable: false } });
        await ensurePasskey(a, { email: world.admin.email, password: world.admin.password }, "_superusers");
        await ensureAdmin(page, world.admin);

        await open(page, `${world.base}/_/#/gdpr/data-map`);
        await say("Data protection starts with the data map: every collection, and what happens to it when a person is erased", 4500);
        await expect(page.locator("tr[data-collection='_legalHolds']")).toBeVisible();
        const warn = page.locator("#datamap_unmapped");
        if (await warn.isVisible().catch(() => false)) {
            await say("A collection nobody has classified is flagged, so none is forgotten", 3500);
            const un = warn.locator("[data-unmapped='youth_roster']");
            if (await un.count()) {
                await un.click();
                await page.getByLabel("Treatment on erasure").selectOption("none");
                await typeIn(page.getByLabel("Note", { exact: true }), "A roster of children; no data about the erased person is kept in it");
                await press(page.getByRole("button", { name: "Add", exact: true }), page, 1200);
            }
        }

        // the export
        await skip(world, "data-protection", "requesting and downloading an export on screen", "the Data requests screen lists exports but starts and downloads none: that is the export routes", a);
        const exp = await api("POST", `/api/gdpr/subjects/${p.rosa.id}/export`, {});
        await open(page, `${world.base}/_/#/gdpr/requests`);
        await say("A data request: Rosa asks for a copy of her data. It is listed with its type, state and due date");
        const done = await (async () => {
            for (let i = 0; i < 30; i++) {
                const s = await api("GET", `/api/gdpr/exports/${exp.id}`);
                if (/ok|done|partial|ready|complete/i.test(JSON.stringify(s.state ?? s.status))) return s;
                await new Promise((r) => setTimeout(r, 1000));
            }
            return await api("GET", `/api/gdpr/exports/${exp.id}`);
        })();
        await page.reload();
        await apiCard(a, "The export: a ZIP with a manifest", `GET /api/gdpr/exports/${exp.id}`, 200, { state: done.state, rows: done.counts ?? done.rows, downloadable: done.downloadable ?? done.canDownload }, 5000);

        // legal hold, through the screen, with a passkey step-up
        await open(page, `${world.base}/_/#/gdpr/holds`);
        await say("A legal hold stops erasure and deletion of what a matter needs. Placing one asks for a fresh passkey");
        await press(page.locator("#hold_new"), page, 800);
        await page.getByLabel("Scope").selectOption("record");
        await typeIn(page.getByLabel("Collection, for one record"), "youth_roster");
        await typeIn(page.getByLabel("Record id, for one record"), world.ids.roster);
        await typeIn(page.getByLabel("Reason", { exact: true }), "Records requested by the diocesan safeguarding review");
        await typeIn(page.getByLabel("Reference (a case number, optional)"), "REVIEW-2026-14");
        await press(page.getByRole("button", { name: "Place the hold" }), page, 1500);
        const row = page.locator("tr[data-active='true']", { hasText: world.ids.roster });
        await expect(row).toBeVisible({ timeout: 20_000 });
        await say("The hold is placed after a passkey check. Now the held record cannot be deleted");
        const del = await rawCall("DELETE", `/api/collections/youth_roster/records/${world.ids.roster}`, undefined, await (await import("../lib/stack")).adminToken());
        await apiCard(a, "Even a superuser cannot delete it", `DELETE /api/collections/youth_roster/records/${world.ids.roster}`, del.status, del.json, 4500);

        // retention preview
        await open(page, `${world.base}/_/#/gdpr/retention`);
        await say("Retention policies delete or archive data after a period. A new one starts disabled, as a dry run");
        await press(page.locator("#retention_new"), page, 800);
        await typeIn(page.getByLabel("Name", { exact: true }), "youth roster, 3 years");
        await typeIn(page.getByLabel("Collection", { exact: true }), "youth_roster");
        await typeIn(page.getByLabel("After", { exact: true }), "1095");
        await press(page.getByRole("button", { name: "Create", exact: true }), page, 1200);
        const pol = page.locator("tr[data-name='youth roster, 3 years']");
        await expect(pol).toContainText("dry run");
        await press(pol.getByRole("button", { name: /^Preview/ }), page, 1500);
        await expect(page.locator("#retention_preview")).toContainText("nothing was changed");
        await say("The preview counts what would match, and changes nothing. The held record would be skipped anyway", 4500);

        // erasure
        await open(page, `${world.base}/_/#/gdpr/requests`);
        await say("Last, Odette Brandt asked the parish to forget her. The operator confirms who she is and starts the erasure");
        await page.locator("#erase_panel summary").click();
        await typeIn(page.locator("#erase_id"), world.ids.odette);
        await press(page.locator("#erase_lookup"), page, 1000);
        await expect(page.locator("#erase_person")).toContainText("odette@example.org");
        await typeIn(page.locator("#erase_identity"), "Seen at the parish office, ID checked");
        await typeIn(page.locator("#erase_grace"), "0");
        await typeIn(page.locator("#erase_typed"), "odette@example.org");
        await press(page.locator("#erase_start"), page, 1500);
        await expect(page.locator("#requests_notice")).toContainText("The erasure was requested.", { timeout: 20_000 });
        const reqRow = page.locator("tr[data-type='erase']", { hasText: world.ids.odette });
        await expect(reqRow).toBeVisible();
        const reqId = (await reqRow.getAttribute("data-request"))!;
        await expect.poll(async () => (await api("GET", `/api/gdpr/erasures/${reqId}`)).state, { timeout: 90_000 }).toMatch(/done|partial/);
        await page.reload();
        const fin = page.locator(`tr[data-request='${reqId}']`);
        await press(fin.getByRole("button", { name: /^View erasure/ }), page, 1500);
        const view = page.locator("#erasure_view");
        await expect(view).toContainText("Anonymized");
        await say("The result: what was anonymized, what was erased, and what is kept, each with its reason", 5000);
        await view.scrollIntoViewIfNeeded();
        await beat(page, 3000);
        void hh;
    } finally {
        await scene.end();
    }
}
