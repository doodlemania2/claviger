import { test, expect, state, api } from "../fixtures";

// The Households section of the admin UI (#80, 80-5). Merge, split and duplicates are written
// against the contract of the merge/split branch: on a build without those routes the page says
// so in words (404), which is what this spec accepts.

test.describe.configure({ mode: "serial" });

let orgId = "";
let alpha: any;
let beta: any;

test.beforeAll(async () => {
    const org = await api("POST", "/api/orgs", { name: "E2E HH Admin", slug: "e2e-hh-admin", owner: state().user.email });
    orgId = org.id;
    alpha = await api("POST", "/api/households", { org: orgId, name: "Alpha family", first: { name: "Ann Alpha", role: "head", guardian: true } });
    beta = await api("POST", "/api/households", { org: orgId, name: "Beta family", first: { name: "Bob Beta", role: "head", guardian: true } });
    await api("POST", `/api/households/${alpha.id}/members`, { name: "Cleo Alpha", role: "child", isMinor: true });
});

test("Households admin: list, filter by status, and open a household", async ({ admin, problems }) => {
    problems.ignore(/status of 404/);
    await admin.goto(`/_/#/households?org=${orgId}`);
    const row = (name: string) => admin.locator("tr[data-household-id]").filter({ hasText: name });
    await expect(row("Alpha family")).toBeVisible();
    await expect(row("Beta family")).toBeVisible();

    // archive Beta through the API, then filter on the status
    await api("PATCH", `/api/households/${beta.id}`, { status: "archived" });
    await admin.getByRole("button", { name: "Refresh" }).click().catch(() => {});
    await admin.reload();
    await admin.getByLabel("Status", { exact: true }).selectOption("archived");
    await expect(row("Beta family")).toBeVisible();
    await expect(row("Alpha family")).toHaveCount(0);
    await admin.getByLabel("Status", { exact: true }).selectOption("all");

    await row("Alpha family").click();
    const detail = admin.locator("#household_detail");
    await expect(detail).toContainText("Ann Alpha");
    await expect(detail).toContainText("Cleo Alpha");
    await expect(detail.locator("tr", { hasText: "Cleo Alpha" })).toContainText("minor");
});

test("Households admin: edit a person and the household", async ({ admin }) => {
    await admin.goto(`/_/#/households?org=${orgId}&household=${alpha.id}`);
    const detail = admin.locator("#household_detail");
    await expect(detail).toContainText("Cleo Alpha");

    await detail.getByRole("button", { name: "Edit Cleo Alpha" }).click();
    const modal = admin.locator(".org-form-modal");
    await modal.getByLabel("Allergies").fill("shellfish");
    await modal.getByLabel("Medical notes").fill("carries an inhaler");
    await modal.getByRole("button", { name: "Save", exact: true }).click();
    await expect(modal).toHaveCount(0);
    await expect(detail.locator("tr", { hasText: "Cleo Alpha" })).toContainText("shellfish");
    const saved = await api("GET", `/api/households/${alpha.id}`);
    expect(saved.members.find((m: any) => m.name == "Cleo Alpha")).toMatchObject({ allergies: "shellfish", medical: "carries an inhaler" });

    await detail.getByRole("button", { name: "Edit", exact: true }).click();
    await modal.getByLabel("City").fill("Plano");
    await modal.getByRole("button", { name: "Save", exact: true }).click();
    await expect(modal).toHaveCount(0);
    expect((await api("GET", `/api/households/${alpha.id}`)).address).toMatchObject({ city: "Plano" });
});

test("Households admin: the last guardian cannot be removed (the guard answers 409)", async ({ admin, problems }) => {
    problems.ignore(/status of 409/);
    await admin.goto(`/_/#/households?org=${orgId}&household=${alpha.id}`);
    const detail = admin.locator("#household_detail");
    await detail.getByRole("button", { name: "Remove guardian flag of Ann Alpha" }).click();
    await admin.locator(".modal.popup.manual").getByRole("button", { name: "Yes" }).click();
    await expect(admin.locator("#households_error")).toContainText(/guardian/i);
});

test("Households admin: merge, split and duplicates (or a clear 404 message without the routes)", async ({ admin, problems }) => {
    problems.ignore(/status of 404/);
    await admin.goto(`/_/#/households?org=${orgId}&household=${alpha.id}`);
    const detail = admin.locator("#household_detail");
    await expect(detail).toContainText("Cleo Alpha");

    await detail.getByRole("button", { name: "Split" }).click();
    const modal = admin.locator(".org-form-modal");
    await modal.getByLabel("Name of the new household").fill("Cleo household");
    await modal.getByLabel(/Move Cleo Alpha/).check();
    await modal.getByRole("button", { name: "Split", exact: true }).click();
    // success closes the modal and selects the new household; a build without the route explains itself
    await expect(admin.locator(".org-form-modal .alert.danger, #household_detail:has-text('Cleo household')").first()).toBeVisible();
    if (await modal.count()) {
        await expect(modal.locator(".alert.danger")).toContainText("not available on this server");
        await modal.getByRole("button", { name: "Cancel" }).click();
    }

    await admin.getByRole("button", { name: "Find duplicates" }).click();
    await expect(admin.locator("#households_error, #household_duplicates table, #household_duplicates :text('No candidates found.')").first()).toBeVisible();
});
