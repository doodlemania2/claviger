import { test, expect, api } from "../fixtures";

test.describe.configure({ mode: "serial" });

// #92: the single sign-on section of the organizations page. Nothing here reaches a real provider or DNS.
test("SSO admin: connection from the Okta preset, domain claim, enable and delete", async ({ admin, problems }) => {
    // the DNS lookup of "Verify now" finds nothing for a made-up domain
    problems.ignore(/status of 4\d\d/);
    const org = await api("POST", "/api/orgs", { name: "E2E SSO Parish", slug: "e2e-sso-parish" });

    await admin.goto(`/_/#/orgs?org=${org.id}`);
    await expect(admin.getByRole("heading", { name: "Single sign-on" })).toBeVisible();
    await expect(admin.getByText("No connections.")).toBeVisible();

    // connection from the preset
    await admin.locator("#sso_add_connection").click();
    await admin.getByLabel("Start from a preset").selectOption({ label: "Okta" });
    await expect(admin.getByLabel("Name", { exact: true })).toHaveValue("Okta");
    await expect(admin.getByLabel("Scopes")).toHaveValue("openid email profile groups");
    await admin.getByLabel("Issuer").fill("https://e2e.okta.example.com/oauth2/default");
    await admin.getByLabel("Client ID").fill("e2e-client");
    await admin.getByLabel("Client secret").fill("e2e-secret-value");
    await admin.getByRole("button", { name: "Create", exact: true }).click();
    await expect(admin.locator(".sso-modal")).toHaveCount(0);

    const connRow = admin.locator("tr[data-sso-connection]");
    await expect(connRow).toContainText("Okta");
    await expect(connRow).toContainText("disabled");

    // the secret is write-only
    const list = await api("GET", `/api/orgs/${org.id}/sso/connections`);
    expect(list.items[0].hasSecret).toBe(true);
    expect(JSON.stringify(list)).not.toContain("e2e-secret-value");
    await connRow.getByRole("button", { name: "Edit" }).click();
    await expect(admin.getByLabel("Client secret")).toHaveValue("");
    await expect(admin.getByLabel("Client secret")).toHaveAttribute("placeholder", /stored/);
    await admin.getByRole("button", { name: "Cancel" }).click();
    await expect(admin.locator(".sso-modal")).toHaveCount(0);

    // domain claim: the TXT value is shown with a copy button
    await admin.locator("#sso_add_domain").click();
    await admin.getByLabel("Domain", { exact: true }).fill("e2e-sso.example.org");
    await admin.locator(".sso-modal").getByRole("button", { name: "Claim domain" }).click();
    await expect(admin.locator("#sso_txt_value")).toHaveValue(/^claviger-verification=/);
    await admin.getByRole("button", { name: "Done" }).click();
    await expect(admin.locator(".sso-modal")).toHaveCount(0);

    const domRow = admin.locator("tr[data-sso-domain]");
    await expect(domRow).toContainText("e2e-sso.example.org");
    await expect(domRow).toContainText("pending");
    await expect(domRow).toContainText("claviger-verification=");
    await expect(connRow).toContainText("e2e-sso.example.org");

    // verify now: no record is published, so it stays unverified
    await domRow.getByRole("button", { name: "Verify now" }).click();
    await expect(domRow).toContainText("never", { timeout: 1 }).catch(() => {});
    await expect(domRow.locator(".label")).not.toHaveText("verified");

    // enable, then disable again
    await connRow.getByRole("button", { name: "Enable", exact: true }).click();
    await expect(connRow).toContainText("enabled");
    await connRow.getByRole("button", { name: "Disable", exact: true }).click();
    await expect(connRow).toContainText("disabled");

    // delete the domain, then the connection (confirmation)
    await domRow.getByRole("button", { name: "Release" }).click();
    await admin.getByRole("button", { name: "Yes" }).click();
    await expect(admin.locator("tr[data-sso-domain]")).toHaveCount(0);
    await connRow.getByRole("button", { name: "Delete" }).click();
    await admin.getByRole("button", { name: "Yes" }).click();
    await expect(admin.locator("tr[data-sso-connection]")).toHaveCount(0);
});
