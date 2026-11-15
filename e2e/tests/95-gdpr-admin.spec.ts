import { test, expect, state, api, watch } from "../fixtures";
import type { BrowserContext, Page } from "@playwright/test";

// The Data protection pages of the admin UI (#88, 88-5): the data map, data requests, legal holds,
// retention policies and the consent history. Placing and lifting a hold need a passkey assertion
// made moments ago, so the superuser gets a passkey (Chromium's virtual authenticator) on
// /passkeys?collection=_superusers first, and signs in to the admin UI afterwards: that session
// has no fresh passkey, so the page runs the step-up itself and repeats the request.

test.describe.configure({ mode: "serial" });

const COLLECTION = "e2egdpr";
let recordId = "";
let userId = "";
let ctx: BrowserContext;
let page: Page;

test.beforeAll(async ({ browser }) => {
    await api("PATCH", "/api/collections/_superusers", { webauthn: { enabled: true, discoverable: false } });
    await api("POST", "/api/collections", {
        name: COLLECTION,
        type: "base",
        fields: [{ name: "title", type: "text" }, { name: "created", type: "autodate", onCreate: true }],
    });
    recordId = (await api("POST", `/api/collections/${COLLECTION}/records`, { title: "held thing" })).id;
    userId = (await api("GET", "/api/collections/users/records?perPage=1")).items[0].id;

    ctx = await browser.newContext({ baseURL: state().base });
    page = await ctx.newPage();
});

test.afterAll(async () => {
    await ctx.close();
});

test("A superuser registers a passkey and signs in to the admin UI", async ({ problems }) => {
    watch(page, problems);
    problems.ignore(/status of 40[34]/);

    // park the conditional autofill, which a virtual authenticator answers at once
    await page.addInitScript(() => {
        const get = navigator.credentials.get.bind(navigator.credentials);
        navigator.credentials.get = (options?: CredentialRequestOptions) => {
            if (options && (options as any).mediation === "conditional") {
                return new Promise<Credential | null>((_r, reject) => {
                    options.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
                });
            }
            return get(options);
        };
    });
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("WebAuthn.enable");
    const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
        options: { protocol: "ctap2", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
    });

    await page.goto("/passkeys?collection=_superusers");
    await page.locator("#linkPasswordLogin").click();
    await page.locator("#pwEmail").fill(state().admin.email);
    await page.locator("#pwPass").fill(state().admin.password);
    await page.locator("#btnPwLogin").click();
    await expect(page.locator("#me")).toContainText(state().admin.email);
    await page.locator("#btnRegister").click();
    await expect.poll(async () => (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials.length, { timeout: 15_000 }).toBe(1);
    await expect(page.getByText("Passkey added.")).toBeVisible();

    await page.goto("/_/#/login");
    await page.getByRole("textbox", { name: /^Email/ }).fill(state().admin.email);
    await page.getByRole("textbox", { name: /^Password/ }).fill(state().admin.password);
    await page.getByRole("button", { name: /Login/i }).click();
    await expect(page.locator(".app-main-nav")).toBeVisible();
});

test("Data map: built-in entries are read-only, an unmapped collection is warned about and can be mapped", async ({ problems }) => {
    watch(page, problems);
    await page.goto("/_/#/gdpr/data-map");
    await expect(page.getByRole("link", { name: "Data protection" }).first()).toBeVisible();

    const builtin = page.locator("tr[data-collection='_apiKeys']");
    await expect(builtin).toBeVisible();
    await expect(builtin).toContainText("built in");
    await expect(builtin.getByRole("button")).toHaveCount(0);
    await expect(page.locator("tr[data-collection='_legalHolds']")).toHaveAttribute("data-treatment", "retain");
    await expect(page.locator("tr[data-collection='_legalHolds']")).toContainText("Evidence that a person");

    const warning = page.locator("#datamap_unmapped");
    await expect(warning).toContainText("no data map entry");
    await warning.locator(`[data-unmapped='${COLLECTION}']`).click();
    await page.getByLabel("Treatment on erasure").selectOption("none");
    await page.getByLabel("Note", { exact: true }).fill("e2e fixture, holds no personal data");
    await page.getByRole("button", { name: "Add", exact: true }).click();

    const row = page.locator(`tr[data-collection='${COLLECTION}']`);
    await expect(row).toContainText("none");
    await expect(row.getByRole("button", { name: `Edit ${COLLECTION}` })).toBeVisible();
    await expect(page.locator(`[data-unmapped='${COLLECTION}']`)).toHaveCount(0);
});

test("Data requests: a requested export is listed with its type, state and due date", async ({ problems }) => {
    watch(page, problems);
    await api("POST", `/api/gdpr/subjects/${userId}/export`, {});
    await page.goto("/_/#/gdpr/requests");
    const row = page.locator("tr[data-type='export']").first();
    await expect(row).toBeVisible();
    await expect(row).toContainText(userId);
    await expect(row.locator("td").nth(4)).not.toHaveText("");
});

test("Legal holds: place one with a passkey step-up, then lift it", async ({ problems }) => {
    watch(page, problems);
    problems.ignore(/status of 403.*\/api\/gdpr\/holds/); // the step-up the page then answers with a passkey
    await page.goto("/_/#/gdpr/holds");

    await page.locator("#hold_new").click();
    await page.getByLabel("Scope").selectOption("record");
    await page.getByLabel("Collection, for one record").fill(COLLECTION);
    await page.getByLabel("Record id, for one record").fill(recordId);
    await page.getByLabel("Reason", { exact: true }).fill("e2e matter");
    await page.getByLabel("Reference (a case number, optional)").fill("CASE-95");
    await page.getByRole("button", { name: "Place the hold" }).click();

    const row = page.locator("tr[data-active='true']", { hasText: recordId });
    await expect(row).toBeVisible({ timeout: 15_000 });
    await expect(row).toContainText("CASE-95");
    await expect(row).toContainText("e2e matter");

    // the held record cannot be deleted
    await expect(api("DELETE", `/api/collections/${COLLECTION}/records/${recordId}`)).rejects.toThrow(/409/);

    await row.getByRole("button", { name: /^Lift hold/ }).click();
    await page.getByRole("button", { name: "Yes", exact: true }).click();
    const lifted = page.locator("tr[data-active='false']", { hasText: recordId });
    await expect(lifted).toBeVisible({ timeout: 15_000 });
    await expect(lifted.getByRole("button")).toHaveCount(0);
    await expect(page.locator("#holds_notice")).toContainText("lifted");
});

test("Retention policies: create (disabled, dry run), preview and see the counts", async ({ problems }) => {
    watch(page, problems);
    await page.goto("/_/#/gdpr/retention");

    await page.locator("#retention_new").click();
    await page.getByLabel("Name", { exact: true }).fill("e2e old items");
    await page.getByLabel("Collection", { exact: true }).fill(COLLECTION);
    await page.getByLabel("After", { exact: true }).fill("30");
    await page.getByRole("button", { name: "Create", exact: true }).click();

    const row = page.locator("tr[data-name='e2e old items']");
    await expect(row).toBeVisible();
    await expect(row).toContainText("disabled");
    await expect(row).toContainText("dry run");
    await expect(row).toContainText("delete 30 days after created");

    await row.getByRole("button", { name: "Preview e2e old items" }).click();
    const preview = page.locator("#retention_preview");
    await expect(preview).toContainText("nothing was changed");
    const counts = preview.locator("tbody tr").first().locator("td");
    await expect(counts).toHaveCount(8);
    await expect(counts.nth(3)).toHaveText("0"); // matched: the fixture record is new
    await expect(counts.nth(7)).toHaveText("0"); // affected

    // delete it again
    await row.getByRole("button", { name: "Delete e2e old items" }).click();
    await page.getByRole("button", { name: "Yes", exact: true }).click();
    await expect(page.locator("tr[data-name='e2e old items']")).toHaveCount(0);
});

test("Consent history renders for a person", async ({ problems }) => {
    watch(page, problems);
    await page.goto("/_/#/consents");
    await page.getByLabel("Household member id or user id").fill(userId);
    await page.getByRole("button", { name: "Show history" }).click();
    const history = page.locator("#consent_history");
    await expect(history).toBeVisible();
    await expect(history).toContainText("History (append-only)");
});

async function throwaway(tag: string) {
    const email = `gdpr-${tag}-${Date.now()}@example.com`;
    const rec = await api("POST", "/api/collections/users/records", { email, password: "throwaway-pass-123", passwordConfirm: "throwaway-pass-123", verified: true, name: `Gone ${tag}` });
    return { id: rec.id as string, email };
}

async function startErasureInUi(id: string, email: string, grace: string) {
    await page.goto("/_/#/gdpr/requests");
    await page.locator("#erase_panel summary").click();
    await page.locator("#erase_id").fill(id);
    await page.locator("#erase_lookup").click();
    await expect(page.locator("#erase_person")).toContainText(email);
    await page.locator("#erase_identity").fill("Seen at the office (e2e)");
    await page.locator("#erase_grace").fill(grace);
    // a deliberate second step: the button stays off until the email or id is typed
    await expect(page.locator("#erase_start")).toBeDisabled();
    await page.locator("#erase_typed").fill("wrong");
    await expect(page.locator("#erase_start")).toBeDisabled();
    await page.locator("#erase_typed").fill(email);
    await expect(page.locator("#erase_start")).toBeEnabled();
    await page.locator("#erase_start").click();
    await expect(page.locator("#requests_notice")).toContainText("The erasure was requested.", { timeout: 15_000 });
}

test("Erasure: start one with no grace period, see it in the list and read its result", async ({ problems }) => {
    watch(page, problems);
    problems.ignore(/status of 403.*\/api\/gdpr\//);
    const u = await throwaway("now");
    await startErasureInUi(u.id, u.email, "0");

    let row = page.locator("tr[data-type='erase']", { hasText: u.id });
    await expect(row).toBeVisible();
    // the finished request no longer names the person (it holds a pseudonym), so follow it by its id
    const reqId = (await row.getAttribute("data-request"))!;
    await expect.poll(async () => (await api("GET", `/api/gdpr/erasures/${reqId}`)).state, { timeout: 60_000 }).toMatch(/done|partial/);
    await page.reload();
    row = page.locator(`tr[data-request='${reqId}']`);
    await expect(row).toHaveAttribute("data-state", /done|partial/);

    await row.getByRole("button", { name: /^View erasure/ }).click();
    const view = page.locator("#erasure_view");
    await expect(view).toContainText("Result");
    await expect(view).toContainText("Anonymized");
    await expect(view).toContainText("Kept");
    await expect(view).toContainText("Seen at the office (e2e)");
});

test("Erasure: cancel one during the grace period", async ({ problems }) => {
    watch(page, problems);
    problems.ignore(/status of 403.*\/api\/gdpr\//);
    const u = await throwaway("grace");
    await startErasureInUi(u.id, u.email, "5");

    const row = page.locator("tr[data-type='erase']", { hasText: u.id });
    await expect(row).toHaveAttribute("data-state", "grace");
    await row.getByRole("button", { name: /^Cancel erasure/ }).click();
    await page.getByRole("button", { name: "Yes", exact: true }).click();
    await expect(page.locator("#requests_notice")).toContainText("cancelled");
    await expect(page.locator("tr[data-type='erase']", { hasText: u.id })).toHaveAttribute("data-state", "cancelled");
    await expect(page.locator("tr[data-type='erase']", { hasText: u.id }).getByRole("button", { name: /^Cancel erasure/ })).toHaveCount(0);
});

test("Erasure: the grace period setting is saved", async ({ problems }) => {
    watch(page, problems);
    await page.goto("/_/#/gdpr/requests");
    await page.locator("#erasure_grace_days").fill("9");
    await page.locator("#erasure_grace_save").click();
    await expect(page.locator("#requests_notice")).toContainText("grace period was saved");
    expect((await api("GET", "/api/settings")).erasure.graceDays).toBe(9);
    await page.locator("#erasure_grace_days").fill("");
    await page.locator("#erasure_grace_save").click();
    await expect.poll(async () => (await api("GET", "/api/settings")).erasure.graceDays).toBeNull();
});
