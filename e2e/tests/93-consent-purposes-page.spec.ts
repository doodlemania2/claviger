import { test, expect, state, api } from "../fixtures";
import type { Page } from "@playwright/test";

// The hosted consent purposes page of an org (#88): owners, admins and registrars manage the
// purposes of their own org; a plain member is turned away.

test.describe.configure({ mode: "serial" });

const registrar = { email: "cp-registrar@example.com", password: "registrar-pass-123" };
const member = { email: "cp-member@example.com", password: "member-pass-12345" };
const slug = "e2e-purposes";

let orgId = "";
let memberId = "";

async function userId(email: string, password: string): Promise<string> {
    const res = await fetch(state().base + "/api/collections/users/auth-with-password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ identity: email, password }),
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
    for (const u of [registrar, member]) {
        await api("POST", "/api/collections/users/records", { email: u.email, password: u.password, passwordConfirm: u.password, verified: true });
    }
    const org = await api("POST", "/api/orgs", { name: "E2E Purposes", slug, owner: state().user.email });
    orgId = org.id;
    await api("POST", `/api/orgs/${orgId}/members`, { user: await userId(registrar.email, registrar.password), role: "member", tags: ["registrar"] });
    memberId = await userId(member.email, member.password);
    await api("POST", `/api/orgs/${orgId}/members`, { user: memberId, role: "member" });
    await api("POST", "/api/consent-purposes", { key: "e2e-global-news", name: "Global news", text: "Everyone gets news." });
});

test("Purposes page: a shell with a strict policy, and it needs sign-in", async ({ guarded: page }) => {
    const res = await page.goto(`/orgs/consents/${slug}`);
    const csp = res!.headers()["content-security-policy"] || "";
    expect(csp).not.toContain("unsafe-inline");
    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    await expect(page.locator("a.skip")).toHaveAttribute("href", "#app");
    await expect(page.locator("body")).not.toContainText("Global news");
});

test("Purposes page: a registrar adds, edits, retires and offers a purpose again", async ({ guarded: page, problems }) => {
    problems.ignore(/status of 40[49]/);
    await page.goto(`/orgs/consents/${slug}`);
    await signIn(page, registrar);

    await expect(page.getByRole("heading", { name: "Consent purposes" })).toBeVisible();
    // the global purposes are listed, and cannot be changed here
    const global = page.locator("tr[data-purpose-key='e2e-global-news']");
    await expect(global).toContainText("Everyone");
    await expect(global.getByRole("button")).toHaveCount(0);

    await page.getByLabel("Key", { exact: true }).fill("e2e-trips");
    await page.getByLabel("Name", { exact: true }).fill("Trip photos");
    await page.getByLabel("Text shown to the person").fill("We may show photos of the trip.");
    await page.getByRole("button", { name: "Add purpose" }).click();
    await expect(page.locator("#pstatus")).toContainText("The purpose was added.");

    const row = page.locator("tr[data-purpose-key='e2e-trips']");
    await expect(row).toContainText("Trip photos");
    await expect(row).toContainText("Your organization");
    await expect(row.locator("td").nth(2)).toHaveText("1");

    await row.getByRole("button", { name: "Edit" }).click();
    await expect(page.getByRole("heading", { name: "Edit Trip photos" })).toBeVisible();
    await expect(page.getByLabel("Key", { exact: true })).toBeDisabled();
    await page.getByLabel("Text shown to the person").fill("We may show photos of the trip on the notice board.");
    await page.getByRole("button", { name: "Save changes" }).click();
    await expect(page.locator("#pstatus")).toContainText("now version 2");
    await expect(row.locator("td").nth(2)).toHaveText("2");

    await row.getByRole("button", { name: "Retire" }).click();
    await expect(row).toContainText("Retired");
    await row.getByRole("button", { name: "Offer again" }).click();
    await expect(row).toContainText("Offered");
});

test("Purposes page: a marketing purpose gets a minimum age of 18, and a used purpose cannot be deleted", async ({ guarded: page, problems }) => {
    problems.ignore(/status of 40[349]|status of 409/);
    await page.goto(`/orgs/consents/${slug}`);
    await signIn(page, registrar);

    await page.getByLabel("Key", { exact: true }).fill("e2e-offers");
    await page.getByLabel("Name", { exact: true }).fill("Offers");
    await page.getByLabel("Text shown to the person").fill("We send offers.");
    await page.getByLabel("This is direct marketing").check();
    await page.getByRole("button", { name: "Add purpose" }).click();
    const offers = page.locator("tr[data-purpose-key='e2e-offers']");
    await expect(offers).toContainText("Marketing");
    const list = await api("GET", `/api/orgs/${orgId}/consent-purposes`);
    expect(list.items.find((p: any) => p.key === "e2e-offers").minAge).toBe(18);

    // somebody has consented to the trips purpose: it can only be retired
    await api("POST", `/api/orgs/${orgId}/consents`, {
        purpose: "e2e-trips", user: memberId, action: "grant", method: "paper", evidenceRef: "form 7",
    });
    page.once("dialog", (d) => d.accept());
    await page.locator("tr[data-purpose-key='e2e-trips']").getByRole("button", { name: "Delete" }).click();
    await expect(page.locator("#pstatus")).toContainText("cannot be deleted");

    // an unused one can
    page.once("dialog", (d) => d.accept());
    await offers.getByRole("button", { name: "Delete" }).click();
    await expect(page.locator("#pstatus")).toContainText("The purpose was deleted.");
    await expect(offers).toHaveCount(0);
});

test("Purposes page: a plain member is turned away, and Spanish loads", async ({ guarded: page, problems }) => {
    problems.ignore(/status of 403/);
    await page.goto(`/orgs/consents/${slug}`);
    await signIn(page, member);
    await expect(page.getByRole("heading", { name: "No access" })).toBeVisible();
    await expect(page.locator("#denied")).toContainText("Only the owners, admins and registrars");
    await expect(page.locator("body")).not.toContainText("Trip photos");

    await page.goto(`/orgs/consents/${slug}?lang=es`);
    await expect(page.locator("html")).toHaveAttribute("lang", "es");
});
