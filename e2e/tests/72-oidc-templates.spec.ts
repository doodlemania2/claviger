import { test, expect, state, api } from "../fixtures";

// #85: the New client form can start from a vendor template, and shows which claims leave.

test("template route is superuser only", async () => {
    const res = await fetch(state().base + "/api/oidc/client-templates");
    expect([401, 403]).toContain(res.status);
    const list = await api("GET", "/api/oidc/client-templates");
    expect(list.map((t: { id: string }) => t.id)).toContain("generic");
});

test("pick a template, confirm the claims, create the client", async ({ admin }) => {
    await admin.goto("/_/#/oidc");
    await admin.getByRole("button", { name: "New client" }).click();

    await admin.getByLabel("Start from a template").selectOption("ministry-platform");
    await expect(admin.getByText("Not yet verified end to end")).toBeVisible();
    await expect(admin.getByText("Claims sent to ACS Technologies")).toBeVisible();
    await expect(admin.getByText("The member's email address.")).toBeVisible();

    const apply = admin.getByRole("button", { name: "Apply template" });
    await expect(apply).toBeDisabled();

    await admin.getByLabel("MinistryPlatform domain").fill("mp.example.org");
    await admin.getByLabel("Identity Provider Unique ID (uppercase)").fill("0F8FAD5B-D9CB-469F-A165-70867728950E");
    await admin.getByLabel("I understand these details").check({ force: true });
    await expect(apply).toBeEnabled();
    await apply.click();

    await expect(admin.getByLabel("Name", { exact: true })).toHaveValue("MinistryPlatform (ACS Technologies)");
    await expect(admin.getByLabel("Redirect URIs (one per line)", { exact: true })).toHaveValue(
        "https://mp.example.org/ministryplatformapi/oauth/callback/0F8FAD5B-D9CB-469F-A165-70867728950E",
    );
    await expect(admin.getByLabel("Allowed scopes")).toHaveValue("openid profile email");

    await admin.getByRole("button", { name: "Create", exact: true }).click();
    await expect(admin.getByText("shown only once")).toBeVisible();
    expect((await admin.getByLabel("Client secret").inputValue()).length).toBeGreaterThan(16);
    await admin.getByRole("button", { name: "Done" }).click();
    await expect(admin.getByRole("row", { name: /MinistryPlatform/ })).toBeVisible();
});
