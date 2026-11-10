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

test("Orgs: tree view, kind labels, re-parenting with cycle and depth errors, status and rule snippets (#84)", async ({ admin, problems }) => {
    // the refused cycle and depth moves are answered with a 400 on purpose
    problems.ignore(/status of 400/);
    const org = (slug: string) => api("GET", `/api/orgs/${slug}`);
    const region = await api("POST", "/api/orgs", { name: "E2E Region", slug: "e2e-region", kind: "region" });
    const ministry = await api("POST", "/api/orgs", { name: "E2E Ministry", slug: "e2e-ministry", kind: "ministry", parent: region.id });
    expect(region.kind).toBe("region");

    await admin.goto("/_/#/orgs");
    const row = (name: string) =>
        admin.locator("tr[data-org-id]").filter({ has: admin.locator(".txt-bold", { hasText: new RegExp(`^${name}$`) }) });

    // the depth is read-only and comes from the server
    const config = await api("GET", "/api/orgs/config");
    await expect(admin.locator("#orgs_depth")).toContainText(`Maximum tree depth: ${config.maxDepth} levels`);
    expect(config.kinds).toEqual(expect.arrayContaining(["region", "ministry"]));

    // tree: the child sits under its parent, kinds are shown
    await expect(row("E2E Region")).toContainText("region");
    await expect(row("E2E Ministry")).toContainText("under E2E Region");
    await expect(admin.locator(`tr[data-org-id="${ministry.id}"]`)).toHaveAttribute("data-level", "2");

    // collapse hides the subtree, expand brings it back
    await admin.getByRole("button", { name: "Collapse E2E Region" }).click();
    await expect(row("E2E Ministry")).toHaveCount(0);
    await admin.getByRole("button", { name: "Expand E2E Region" }).click();
    await expect(row("E2E Ministry")).toBeVisible();

    // status of the selected org: counts only
    await row("E2E Region").click();
    await expect(admin.locator("#org_status")).toContainText("1 organization below (1 ministry)");
    const status = await api("GET", `/api/orgs/${region.id}/status`);
    expect(status).toMatchObject({ level: 1, height: 1, descendants: 1, members: { active: 0, suspended: 0 } });

    // re-parent the parish from the first test under the ministry
    const parish = await org("e2e-parish");
    await row("E2E Parish").getByRole("button", { name: "Move" }).click();
    await admin.locator(".org-form-modal").getByLabel("New parent").selectOption(ministry.id);
    await admin.locator(".org-form-modal").getByRole("button", { name: "Move", exact: true }).click();
    await expect.poll(async () => (await org("e2e-parish")).parent).toBe(ministry.id);
    await expect(admin.locator(`tr[data-org-id="${parish.id}"]`)).toHaveAttribute("data-level", "3");
    await expect(row("E2E Parish")).toContainText("under E2E Ministry");
    await expect(admin.locator(".org-form-modal")).toHaveCount(0);

    // a cycle is refused with a clear message and nothing changes
    await row("E2E Region").getByRole("button", { name: "Move" }).click();
    await admin.locator(".org-form-modal").getByLabel("New parent").selectOption(parish.id);
    await admin.locator(".org-form-modal").getByRole("button", { name: "Move", exact: true }).click();
    await expect(admin.locator(".org-form-modal .alert.danger")).toContainText("cycle");
    expect((await org("e2e-region")).parent).toBe("");
    await admin.locator(".org-form-modal").getByRole("button", { name: "Cancel" }).click();
    await expect(admin.locator(".org-form-modal")).toHaveCount(0);

    // depth: fill the tree to the limit, then try to go one level deeper
    let parent = parish.id;
    for (let level = 4; level <= config.maxDepth; level++) {
        parent = (await api("POST", "/api/orgs", { name: `E2E Level ${level}`, slug: `e2e-level-${level}`, parent })).id;
    }
    const deep = await api("POST", "/api/orgs", { name: "E2E Deep Root", slug: "e2e-deep-root" });
    await admin.getByRole("button", { name: "Refresh" }).first().click();
    await row("E2E Deep Root").getByRole("button", { name: "Move" }).click();
    await admin.locator(".org-form-modal").getByLabel("New parent").selectOption(parent);
    await admin.locator(".org-form-modal").getByRole("button", { name: "Move", exact: true }).click();
    await expect(admin.locator(".org-form-modal .alert.danger")).toContainText(`at most ${config.maxDepth} levels`);
    expect((await org(deep.id)).parent).toBe("");
    await admin.locator(".org-form-modal").getByRole("button", { name: "Cancel" }).click();
    await expect(admin.locator(".org-form-modal")).toHaveCount(0);

    // rule snippets show exactly what the server renders
    await admin.getByRole("button", { name: "Rule templates for org-owned collections" }).click();
    await admin.locator("#snippet_role_admin").check();
    await admin.locator("#snippet_depth").selectOption("2");
    const want = await api("GET", "/api/orgs/ancestor-rule?field=org&roles=admin&maxDepth=2");
    expect(want.rule).toContain("?<= 2");
    await expect(admin.locator("#snippet_result code").first()).toHaveText(want.rule);
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
