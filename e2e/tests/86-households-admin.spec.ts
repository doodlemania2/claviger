import { test, expect, state, api } from "../fixtures";

// The Households section of the admin UI (#80, 80-5), including merge, split and duplicates (80-4).

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
    // stays behind when Ann and Cleo split off (a split may not move everyone)
    await api("POST", `/api/households/${alpha.id}/members`, { name: "Dan Alpha", role: "adult" });
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

test("Households admin: split, merge and duplicates", async ({ admin, problems }) => {
    problems.ignore(/status of 409/);
    await admin.goto(`/_/#/households?org=${orgId}&household=${alpha.id}`);
    const detail = admin.locator("#household_detail");
    await expect(detail).toContainText("Cleo Alpha");
    const modal = admin.locator(".org-form-modal");

    // moving the child away from every guardian is refused by the guard, in words
    await detail.getByRole("button", { name: "Split" }).click();
    await modal.getByLabel("Name of the new household").fill("Cleo household");
    await modal.getByLabel(/Move Cleo Alpha/).check();
    await modal.getByRole("button", { name: "Split", exact: true }).click();
    await expect(modal.locator(".alert.danger")).toContainText("active adult guardian");

    // with her guardian it goes through
    await modal.getByLabel(/Move Ann Alpha/).check();
    await modal.getByRole("button", { name: "Split", exact: true }).click();
    await expect(modal).toHaveCount(0);
    await expect(detail).toContainText("Cleo household");

    const list = await api("GET", `/api/households?org=${orgId}`);
    const cleo = list.items.find((h: any) => h.name === "Cleo household");
    expect(cleo, "the new household exists").toBeTruthy();

    // an archived household (Beta, archived by the first test) cannot be merged
    await expect(api("POST", `/api/households/${cleo.id}/merge`, { from: beta.id })).rejects.toThrow(/409/);

    // an active one can; the UI then shows the moved person
    const gamma = await api("POST", "/api/households", { org: orgId, name: "Gamma family", first: { name: "Gus Gamma", role: "head", guardian: true } });
    const merged = await api("POST", `/api/households/${cleo.id}/merge`, { from: gamma.id });
    expect((merged.members || []).map((m: any) => m.name).sort()).toEqual(["Ann Alpha", "Cleo Alpha", "Gus Gamma"]);
    // the merge happened outside this page, so load it again
    await admin.goto(`/_/#/households?org=${orgId}&household=${cleo.id}`);
    await admin.reload();
    await expect(detail).toContainText("Gus Gamma");

    await admin.getByRole("button", { name: "Find duplicates" }).click();
    await expect(admin.locator("#household_duplicates table, #household_duplicates :text('No candidates found.')").first()).toBeVisible();
});
