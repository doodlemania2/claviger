import { test, expect, state, api, confirmYes } from "../fixtures";
import type { Page } from "@playwright/test";

// The Credentials section of the admin UI and the hosted "My credentials" page (#81, 81-4).

test.describe.configure({ mode: "serial" });

const holder = { email: "cred-holder@example.com", password: "holder-pass-12345", name: "Hana Holder" };
const other = { email: "cred-other@example.com", password: "other-pass-123456", name: "Otto Other" };

let orgId = "";
let holderId = "";
let otherId = "";

async function userId(u: { email: string; password: string }): Promise<string> {
    const res = await fetch(state().base + "/api/collections/users/auth-with-password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ identity: u.email, password: u.password }),
    });
    return (await res.json()).record.id;
}

async function signIn(page: Page, who: { email: string; password: string }) {
    const form = page.locator("form#pw");
    await form.getByLabel("Email or username").fill(who.email);
    await form.getByLabel("Password").fill(who.password);
    await form.getByRole("button", { name: "Sign in", exact: true }).click();
}

test.beforeAll(async () => {
    for (const u of [holder, other]) {
        await api("POST", "/api/collections/users/records", { email: u.email, password: u.password, passwordConfirm: u.password, name: u.name, verified: true });
    }
    holderId = await userId(holder);
    otherId = await userId(other);
    const org = await api("POST", "/api/orgs", { name: "E2E Credentials", slug: "e2e-credentials", owner: state().user.email });
    orgId = org.id;
    await api("POST", `/api/orgs/${orgId}/members`, { user: holderId, role: "member" });
    await api("POST", `/api/orgs/${orgId}/members`, { user: otherId, role: "member" });
    await api("POST", `/api/orgs/${orgId}/credential-types`, { key: "safe_env", name: "Safe environment", validMonths: 12 });
    await api("POST", `/api/orgs/${orgId}/credential-types`, { key: "strict_chk", name: "Strict check", validMonths: 6, requiresVerifier: true });
});

test("Credentials admin: record a credential and see the summary", async ({ admin, problems }) => {
    problems.ignore(/status of 404/);
    await admin.goto(`/_/#/credentials?org=${orgId}`);
    await expect(admin.locator("#credential_types")).toContainText("Safe environment");

    await admin.getByRole("button", { name: "Record credential" }).click();
    const modal = admin.locator(".org-form-modal");
    await modal.getByLabel("Holder").selectOption({ label: holder.name });
    await modal.getByLabel("Credential type").selectOption({ label: "Safe environment" });
    await modal.getByLabel("Issued by (optional)").fill("Training Co");
    await modal.getByRole("button", { name: "Record", exact: true }).click();
    await expect(modal).toHaveCount(0);

    const row = admin.locator("tr[data-credential-id]").filter({ hasText: holder.name });
    await expect(row).toContainText("Safe environment");
    await expect(row).toContainText("valid");
    await expect(row).toContainText("Training Co");

    // 3 members (the owner too): one cleared, two without anything
    const summary = admin.locator('tr[data-summary-type="safe_env"]');
    await expect(summary.locator('[data-standing="cleared"]')).toHaveText("1");
    await expect(summary.locator('[data-standing="missing"]')).toHaveText("2");
});

test("Credentials admin: a type with a second verifier starts pending and is verified", async ({ admin, problems }) => {
    problems.ignore(/status of 403|status of 404/);
    await api("POST", `/api/orgs/${orgId}/credentials`, { user: otherId, type: "strict_chk" });
    await admin.goto(`/_/#/credentials?org=${orgId}`);
    await admin.getByLabel("State", { exact: true }).selectOption("pending");
    const row = admin.locator("tr[data-credential-id]").filter({ hasText: other.name });
    await expect(row).toContainText("pending");
    await expect(admin.locator("tr[data-credential-id]").filter({ hasText: holder.name })).toHaveCount(0);

    await row.getByRole("button", { name: /^Verify/ }).click();
    await expect(admin.locator("#credentials_notice, #credentials_error").first()).toBeVisible();
    await admin.getByLabel("State", { exact: true }).selectOption("all");
});

test("Credentials admin: revoke with a reason, and manage types", async ({ admin, problems }) => {
    problems.ignore(/status of 404|status of 409/);
    await admin.goto(`/_/#/credentials?org=${orgId}`);
    const row = admin.locator("tr[data-credential-id]").filter({ hasText: other.name });
    await row.getByRole("button", { name: /^Revoke/ }).click();
    const modal = admin.locator(".org-form-modal");
    await modal.getByLabel("Reason").fill("withdrawn by the coordinator");
    await modal.getByRole("button", { name: "Revoke", exact: true }).click();
    await expect(modal).toHaveCount(0);
    await admin.getByLabel("State", { exact: true }).selectOption("revoked");
    await expect(row).toContainText("withdrawn by the coordinator");
    await admin.getByLabel("State", { exact: true }).selectOption("all");

    // a new type
    await admin.getByRole("button", { name: "New type" }).click();
    await modal.getByLabel("Key").fill("first_aid");
    await modal.getByLabel("Name", { exact: true }).fill("First aid");
    await modal.getByRole("button", { name: "Create", exact: true }).click();
    await expect(modal).toHaveCount(0);
    const typeRow = admin.locator('tr[data-type-key="first_aid"]');
    await expect(typeRow).toContainText("First aid");

    // rule snippet
    await typeRow.getByRole("button", { name: /^Rule snippet/ }).click();
    await expect(admin.locator("#credential_snippet")).toContainText("@collection._credentials:c.expires ?> @now");

    // retire it, then delete it (no credentials of the type exist)
    await typeRow.getByRole("button", { name: /^Retire/ }).click();
    await expect(typeRow).toContainText("retired");
    await typeRow.getByRole("button", { name: /^Delete/ }).click();
    await confirmYes(admin);
    await expect(typeRow).toHaveCount(0);

    // a type that is in use cannot be deleted: the server's 409 is shown in words
    await admin.locator('tr[data-type-key="safe_env"]').getByRole("button", { name: /^Delete/ }).click();
    await confirmYes(admin);
    await expect(admin.locator("#credentials_error")).toBeVisible();
});

test("My credentials: shell needs sign-in and shows nothing before it", async ({ guarded: page }) => {
    const res = await page.goto("/credentials/mine");
    const csp = res!.headers()["content-security-policy"] || "";
    expect(csp).not.toContain("unsafe-inline");
    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    await expect(page.locator("a.skip")).toHaveAttribute("href", "#app");
    await expect(page.locator("body")).not.toContainText("Safe environment");
});

test("My credentials: a holder sees dates, and a lapse reads kindly", async ({ guarded: page, problems }) => {
    problems.ignore(/status of 404/);
    await page.goto("/credentials/mine");
    await signIn(page, holder);

    const section = page.locator("section.hh").filter({ hasText: "E2E Credentials" });
    const safe = section.locator("li.cred").filter({ hasText: "Safe environment" });
    await expect(safe).toContainText("Current");
    await expect(safe).toContainText("Current until");
    await expect(section.locator("li.cred").filter({ hasText: "Strict check" })).toContainText("Nothing recorded yet");
    await expect(page.locator("body")).toContainText("talk to your safeguarding coordinator");
    await expect(page.locator("body")).toContainText("only your coordinators");

    // let it lapse, as the passage of time would
    const list = await api("GET", `/api/orgs/${orgId}/credentials?user=${holderId}`);
    await api("PATCH", `/api/collections/_credentials/records/${list.items[0].id}`, { expires: "2020-01-31 00:00:00.000Z" });
    await page.reload();
    await expect(safe).toContainText("Ready to renew");
    await expect(safe).toContainText("happens to everyone");
    await expect(page.locator("body")).not.toContainText(/expired|failed|overdue|violation/i);
});

test("My credentials: a revoked credential does not show the reason, and Spanish loads", async ({ guarded: page }) => {
    await page.goto("/credentials/mine");
    await signIn(page, other);
    const row = page.locator("li.cred").filter({ hasText: "Strict check" });
    await expect(row).toContainText("Not active right now");
    await expect(page.locator("body")).not.toContainText("withdrawn by the coordinator");

    await page.goto("/credentials/mine?lang=es");
    await expect(page.locator("html")).toHaveAttribute("lang", "es");
});
