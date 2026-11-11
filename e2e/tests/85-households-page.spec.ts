import { test, expect, state, api, watch } from "../fixtures";
import type { Page } from "@playwright/test";

// The hosted "My household" page (#80, 80-6): a guardian edits what the API allows, a
// person who is not a guardian sees the household read-only.

test.describe.configure({ mode: "serial" });

const guardian = { email: "hh-guardian@example.com", password: "guardian-pass-1234" };
const adult = { email: "hh-adult@example.com", password: "adult-pass-12345" };

let orgId = "";
let household: any;
let kid: any;

async function signIn(page: Page, who: { email: string; password: string }) {
    const form = page.locator("form#pw");
    await form.getByLabel("Email or username").fill(who.email);
    await form.getByLabel("Password").fill(who.password);
    await form.getByRole("button", { name: "Sign in", exact: true }).click();
}

async function userId(email: string, password: string): Promise<string> {
    const res = await fetch(state().base + "/api/collections/users/auth-with-password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ identity: email, password }),
    });
    return (await res.json()).record.id;
}

test.beforeAll(async () => {
    for (const u of [guardian, adult]) {
        await api("POST", "/api/collections/users/records", { email: u.email, password: u.password, passwordConfirm: u.password, verified: true });
    }
    const org = await api("POST", "/api/orgs", { name: "E2E Households", slug: "e2e-households", owner: state().user.email });
    orgId = org.id;
    household = await api("POST", "/api/households", {
        org: orgId,
        name: "Garcia family",
        first: { name: "Ana Garcia", role: "head", guardian: true, user: await userId(guardian.email, guardian.password) },
    });
    await api("POST", `/api/households/${household.id}/members`, { name: "Luis Garcia", role: "adult", user: await userId(adult.email, adult.password) });
    kid = await api("POST", `/api/households/${household.id}/members`, { name: "Mia Garcia", role: "child", isMinor: true, birthDate: new Date(Date.now() - 8 * 365.25 * 864e5).toISOString().slice(0, 10) });
});

test("Households page: the shell needs sign-in and is accessible", async ({ guarded: page }) => {
    const res = await page.goto("/households/mine");
    const csp = res!.headers()["content-security-policy"] || "";
    expect(csp).not.toContain("unsafe-inline");
    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    await expect(page.locator("main")).toHaveCount(1);
    await expect(page.locator("a.skip")).toHaveAttribute("href", "#app");
    // nothing about any household before sign-in
    await expect(page.locator("body")).not.toContainText("Garcia");
});

test("Households page: a guardian edits the household and a child", async ({ guarded: page, problems }) => {
    problems.ignore(/status of 404|status of 409/); // the photo routes may not exist on this build
    await page.goto("/households/mine");
    await signIn(page, guardian);

    await expect(page.getByRole("heading", { name: "Garcia family", level: 2 })).toBeVisible();
    await expect(page.locator(".person")).toHaveCount(3);

    // household details
    const details = page.getByRole("form", { name: "Household details" });
    await details.getByLabel("Street").fill("12 Oak Street");
    await details.getByLabel("City").fill("Frisco");
    await details.getByLabel("Home phone").fill("+1 555 0100");
    await details.getByRole("button", { name: "Save" }).click();
    await expect(details.getByRole("status")).toHaveText("Saved.");

    // the child
    const child = page.getByRole("form", { name: "Mia Garcia" });
    await child.getByLabel("Allergies").fill("peanuts");
    await child.getByLabel("Medical notes").fill("asthma, inhaler in the bag");
    await child.getByRole("button", { name: "Save" }).click();
    await expect(child.getByRole("status")).toHaveText("Saved.");

    const saved = await api("GET", `/api/households/${household.id}`);
    expect(saved.address).toMatchObject({ street: "12 Oak Street", city: "Frisco" });
    expect(saved.homePhone).toBe("+1 555 0100");
    const mia = saved.members.find((m: any) => m.id === kid.id);
    expect(mia.allergies).toBe("peanuts");

    // photo: either the backend has the routes, or the page says so (and stays usable)
    await page.locator(".person").filter({ hasText: "Mia Garcia" }).getByRole("button", { name: "Upload photo" }).click();
    await expect(page.getByRole("alert").filter({ hasText: "Choose a photo first." })).toBeVisible();
});

test("Households page: a person who is not a guardian sees it read-only", async ({ guarded: page }) => {
    await page.goto("/households/mine");
    await signIn(page, adult);
    await expect(page.getByRole("heading", { name: "Garcia family", level: 2 })).toBeVisible();
    await expect(page.locator("body")).toContainText("only a guardian can change it");
    await expect(page.locator("form.hhform")).toHaveCount(0);
    // the child's health data is not shown to another adult who is not a guardian
    await expect(page.locator("body")).not.toContainText("peanuts");
    await expect(page.locator("body")).not.toContainText("asthma");
});

test("Households page: Spanish on request", async ({ guarded: page }) => {
    await page.goto("/households/mine?lang=es");
    await expect(page.getByRole("heading", { name: "Mi hogar" })).toBeVisible();
});
