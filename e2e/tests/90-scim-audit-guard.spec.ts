import { test, expect, state, api, confirmYes, watch } from "../fixtures";

test("SCIM tokens: create one in the org page, use it on /scim/v2, revoke it", async ({ admin }) => {
    const org = await api("POST", "/api/orgs", { name: "SCIM Org", slug: "scim-org" });
    await admin.goto("/_/#/orgs");
    await admin.getByRole("row", { name: /SCIM Org/ }).click();

    await admin.getByRole("button", { name: "New token" }).click();
    await admin.getByLabel("Name", { exact: true }).fill("Entra e2e");
    await admin.getByRole("button", { name: "Create token" }).click();

    await expect(admin.getByText("It is shown only once.")).toBeVisible();
    const token = (await admin.locator(".alert.success code").innerText()).trim();
    expect(token).toMatch(/^scim_/);
    await expect(admin.getByRole("row", { name: /Entra e2e/ })).toContainText("active");

    const call = (t: string) => fetch(`${state().base}/scim/v2/ServiceProviderConfig`, { headers: { Authorization: `Bearer ${t}` } });
    expect((await call(token)).status).toBe(200);
    expect((await call("scim_nope_nope")).status).toBe(401);

    await admin.getByRole("row", { name: /Entra e2e/ }).getByRole("button").last().click();
    await confirmYes(admin);
    await expect(admin.getByRole("row", { name: /Entra e2e/ })).toContainText("revoked");
    expect((await call(token)).status).toBe(401);
    void org;
});

test("Audit log: shows the writes of this run, filters, and opens an entry", async ({ admin }) => {
    await admin.goto("/_/#/audit");
    await expect(admin.getByRole("table")).toContainText("create");

    // filter by collection
    await admin.getByLabel("Collection").fill("e2e_none_such");
    await admin.getByLabel("Collection").press("Enter");
    await expect(admin.getByText(/no (audit )?entries|nothing/i).first()).toBeVisible();

    await admin.getByLabel("Collection").fill("users");
    await admin.getByLabel("Collection").press("Enter");
    const row = admin.locator("tbody tr", { hasText: "users" }).first();
    await expect(row).toBeVisible();
    await row.click();
    await expect(admin.getByRole("heading", { name: "Audit entry" })).toBeVisible();
    await expect(admin.getByRole("heading", { name: "Changes" })).toBeVisible();
    await admin.getByRole("button", { name: "Close" }).click();
    await expect(admin.getByRole("heading", { name: "Audit entry" })).toBeHidden();
});

test("the console and CSP guard really catches a violation", async ({ browser }) => {
    // Without this, a guard that never fires would make every other test vacuous.
    const ctx = await browser.newContext({ baseURL: state().base });
    const page = await ctx.newPage();
    const problems = { list: [] as string[], ignore: () => {} };
    watch(page, problems);
    await page.goto("/oidc/logged-out"); // CSP: script-src 'self', no inline
    await page.evaluate(() => {
        const s = document.createElement("script");
        s.textContent = "window.__ran = true";
        document.head.appendChild(s);
        console.error("synthetic error");
    });
    await expect.poll(() => page.evaluate(() => (window as any).__cspViolations.length)).toBeGreaterThan(0);
    expect(await page.evaluate(() => (window as any).__ran)).toBeUndefined();
    expect(problems.list.join("\n")).toContain("synthetic error");
    await ctx.close();
});
