import { test, expect, state, api } from "../fixtures";

// The Consents section of the admin UI (#88, 88-2): purposes and a person's history.

test.describe.configure({ mode: "serial" });

let orgId = "";
let kid: any;
let guardian: any;

test.beforeAll(async () => {
    const org = await api("POST", "/api/orgs", { name: "E2E Consent", slug: "e2e-consent", owner: state().user.email });
    orgId = org.id;
    const hh = await api("POST", "/api/households", { org: orgId, name: "Consent family", first: { name: "Gail Guardian", role: "head", guardian: true } });
    guardian = hh.members.find((m: any) => m.name == "Gail Guardian");
    kid = await api("POST", `/api/households/${hh.id}/members`, { name: "Kai Kid", role: "child", isMinor: true });
});

test("Consents admin: create a purpose, change its text, see the version", async ({ admin }) => {
    await admin.goto("/_/#/consents");
    await admin.getByRole("button", { name: "New purpose" }).click();
    const modal = admin.locator(".org-form-modal");
    await modal.getByLabel(/^Key/).fill("e2e-news");
    await modal.getByLabel("Name", { exact: true }).fill("E2E news");
    await modal.getByLabel(/^Text shown/).fill("We send you news.");
    await modal.getByRole("button", { name: "Create", exact: true }).click();
    await expect(modal).toHaveCount(0);

    const row = admin.locator("tr[data-purpose-key='e2e-news']");
    await expect(row).toContainText("E2E news");
    await expect(row).toContainText("consent");

    await row.getByRole("button", { name: "Edit E2E news" }).click();
    await modal.getByLabel(/^Text shown/).fill("We send you news by email.");
    await modal.getByRole("button", { name: "Save", exact: true }).click();
    await expect(modal).toHaveCount(0);
    await expect(admin.locator("#consents_notice")).toContainText("version 2");
    await expect(row.locator("td").nth(2)).toHaveText("2");
});

test("Consents admin: a marketing purpose gets a minimum age, and staff evidence shows in the history", async ({ admin }) => {
    const promo = await api("POST", "/api/consent-purposes", { key: "e2e-promo", name: "E2E promo", text: "Offers.", marketing: true });
    expect(promo.minAge).toBe(18);

    // staff record a paper consent for the child, given by the guardian
    const res = await api("POST", `/api/orgs/${orgId}/consents`, {
        purpose: "e2e-news", member: kid.id, action: "grant", method: "paper", evidenceRef: "form 12, binder A", givenBy: guardian.id,
    });
    expect(res.changed).toBe(true);

    // direct marketing cannot be granted for a minor
    await expect(api("POST", `/api/orgs/${orgId}/consents`, {
        purpose: "e2e-promo", member: kid.id, action: "grant", method: "paper", evidenceRef: "form 13", givenBy: guardian.id,
    })).rejects.toThrow();

    await admin.goto("/_/#/consents");
    await admin.getByLabel("Household member id or user id").fill(kid.id);
    await admin.getByRole("button", { name: "Show history" }).click();
    const history = admin.locator("#consent_history");
    await expect(history.locator(".consent-states-table")).toContainText("e2e-news");
    await expect(history.locator(".consent-states-table")).toContainText("granted");
    await expect(history.locator(".consent-states-table")).toContainText("form 12, binder A");
    await expect(history.locator(".consent-events-table")).toContainText("grant");
    await expect(history.locator(".consent-events-table")).toContainText("paper");
});

test("Consents admin: a used purpose cannot be deleted and shows as given on an older version", async ({ admin }) => {
    await api("PATCH", "/api/consent-purposes/e2e-news", { text: "A third wording." });
    await admin.goto("/_/#/consents");
    await admin.getByLabel("Household member id or user id").fill(kid.id);
    await admin.getByRole("button", { name: "Show history" }).click();
    await expect(admin.locator("#consent_history .consent-states-table")).toContainText("older version");

    await expect(api("DELETE", "/api/consent-purposes/e2e-news")).rejects.toThrow();
});
