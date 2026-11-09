import { test, expect, state, api, watch } from "../fixtures";
import { startSmtp } from "../smtp";
import type { Page } from "@playwright/test";

test.describe.configure({ mode: "serial" });

let smtp: Awaited<ReturnType<typeof startSmtp>>;
test.beforeAll(async () => {
    smtp = await startSmtp(12525);
    await api("PATCH", "/api/settings", { smtp: { enabled: true, host: "127.0.0.1", port: 12525, tls: false } });
});
test.afterAll(async () => {
    await api("PATCH", "/api/settings", { smtp: { enabled: false } });
    await smtp.close();
});

const inviteToken = (text: string) => text.match(/\/orgs\/invite\/([A-Za-z0-9_-]{20,})/)?.[1] ?? "";

async function hostedSignIn(page: Page, email: string, password: string) {
    const form = page.locator("form#pw");
    await form.getByLabel("Email or username").fill(email);
    await form.getByLabel("Password").fill(password);
    await form.getByRole("button", { name: "Sign in", exact: true }).click();
}

test("Orgs: create an org and invite someone in the admin UI", async ({ admin }) => {
    const { user } = state();
    await admin.goto("/_/#/orgs");
    await admin.getByRole("button", { name: "New organization" }).click();
    await admin.getByLabel("Name", { exact: true }).fill("E2E Parish");
    await admin.getByLabel("Slug").fill("e2e-parish");
    await admin.getByLabel(/^Owner/).fill(user.email);
    await admin.getByRole("button", { name: "Create", exact: true }).click();

    const row = admin.getByRole("row", { name: /E2E Parish/ });
    await expect(row).toBeVisible();
    await row.click();
    await expect(admin.getByRole("row", { name: new RegExp(user.email) }).first()).toBeVisible();

    await admin.getByRole("button", { name: "Invite", exact: true }).click();
    await admin.getByLabel("Email", { exact: true }).fill("carol@example.com");
    await admin.getByRole("button", { name: "Send invitation" }).click();
    await expect(admin.getByRole("row", { name: /carol@example\.com/ })).toBeVisible();
    await smtp.waitFor((m) => m.to.includes("carol@example.com"));
});

test("Orgs: an invitee without an account follows the link, registers and joins; a member sees the org page", async ({ browser, problems }) => {
    const { base } = state();
    const mail = await smtp.waitFor((m) => m.to.includes("carol@example.com"));
    const token = inviteToken(mail.text);
    expect(token, "invitation link in the mail").toBeTruthy();

    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    watch(page, problems);

    await page.goto(`/orgs/invite/${token}`);
    await expect(page.getByRole("heading", { name: /Join E2E Parish/ })).toBeVisible();
    const signup = page.locator("form#signupForm");
    await expect(signup.getByLabel("Email")).toHaveValue("carol@example.com");
    await signup.getByLabel("Name").fill("Carol Example");
    await signup.getByLabel("Password", { exact: true }).fill("carol-password-1");
    await signup.getByLabel("Repeat the password").fill("carol-password-1");
    await signup.getByRole("button", { name: "Create the account and join" }).click();

    await expect(page.locator("#doneText")).toContainText("You joined E2E Parish as member");

    // the link is single use
    const again = await ctx.request.get(`/orgs/invite/${token}`);
    expect(again.status()).toBe(404);

    // org page: sign in, see the members, no management forms
    await page.locator("#doneLink").click();
    await expect(page).toHaveURL(/\/orgs\/e2e-parish/);
    // registering already signed this browser in: the page opens straight on the org
    await expect(page.locator("#orgName")).toHaveText("E2E Parish");
    expect(await page.locator("form#pw").isVisible()).toBe(false);
    await expect(page.locator("#orgName")).toHaveText("E2E Parish");
    await expect(page.locator("#myRole")).toContainText("member");
    await expect(page.locator("#members")).toContainText("Carol Example");
    // members see names, not email addresses
    await expect(page.locator("#members")).not.toContainText("@");
    await expect(page.locator("[data-manage]").first()).toBeHidden();
    await ctx.close();
});

test("Orgs: the owner invites an existing account from the hosted org page, who accepts after signing in", async ({ browser, problems }) => {
    const { base, user } = state();
    // an existing, verified account that is not a member yet
    const erin = { email: "erin@example.com", password: "erin-password-1" };
    await api("POST", "/api/collections/users/records", { email: erin.email, password: erin.password, passwordConfirm: erin.password, verified: true });

    const owner = await browser.newContext({ baseURL: base });
    const op = await owner.newPage();
    watch(op, problems);
    await op.goto("/orgs/e2e-parish");
    await hostedSignIn(op, user.email, user.password);
    await expect(op.locator("#myRole")).toContainText("owner");
    await op.locator("form#inviteForm").getByLabel("Email", { exact: true }).fill(erin.email);
    await op.locator("form#inviteForm").getByRole("button", { name: "Send invitation" }).click();
    await expect(op.locator("#invitations")).toContainText(erin.email);

    const mail = await smtp.waitFor((m) => m.to.includes(erin.email));
    const token = inviteToken(mail.text);
    await owner.close();

    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    watch(page, problems);
    await page.goto(`/orgs/invite/${token}`);
    await hostedSignIn(page, erin.email, erin.password);
    await page.getByRole("button", { name: "Accept the invitation" }).click();
    await expect(page.locator("#doneText")).toContainText("You joined E2E Parish");
    await ctx.close();

    const orgs = await api("GET", "/api/orgs");
    const org = orgs.items.find((o: any) => o.slug === "e2e-parish");
    const members = await api("GET", `/api/orgs/${org.id}/members`);
    expect(members.items.map((m: any) => m.email ?? m.user)).toEqual(expect.arrayContaining([erin.email]));
});

test("Orgs: bad invitation tokens and unknown org pages answer with a page, not a crash", async ({ guarded, problems }) => {
    // the two 404 answers below are what Chromium reports as console errors
    problems.ignore(/status of 404/);
    const res = await guarded.goto("/orgs/invite/not-a-real-token-at-all-0123456789");
    expect(res?.status()).toBe(404);
    await expect(guarded.locator("body")).toContainText(/invitation/i);

    const res2 = await guarded.goto("/orgs/Not_A_Slug!");
    expect(res2?.status()).toBe(404);
});
