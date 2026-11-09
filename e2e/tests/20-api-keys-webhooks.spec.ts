import { test, expect, state, confirmYes } from "../fixtures";

test("API keys: create one, see the key once, use it, revoke it", async ({ admin }) => {
    await admin.goto("/_/#/settings/api-keys");
    await admin.getByRole("button", { name: "New API key" }).click();

    const dialog = admin.locator(".api-key-create-modal");
    await dialog.getByLabel("Name").fill("e2e key");
    await dialog.getByLabel("All access (*)").check();
    await dialog.getByRole("button", { name: "Create key" }).click();

    await expect(dialog).toContainText("shown only once");
    const key = (await dialog.locator("code").innerText()).trim();
    expect(key).toMatch(/^clv_/);
    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toBeHidden();

    // The list shows a prefix, never the key.
    const row = admin.getByRole("row", { name: /e2e key/ });
    await expect(row).toBeVisible();
    await expect(row).toContainText("Active");
    await expect(admin.locator("body")).not.toContainText(key);
    await admin.reload();
    await expect(admin.getByRole("row", { name: /e2e key/ })).toBeVisible();
    await expect(admin.locator("body")).not.toContainText(key);

    // The key authenticates as its owner.
    const res = await fetch(`${state().base}/api/collections/users/records`, { headers: { Authorization: `Bearer ${key}` } });
    expect(res.status).toBe(200);

    // Revoke through the UI: the same request is refused afterwards.
    await admin.getByRole("row", { name: /e2e key/ }).getByRole("button").last().click();
    await confirmYes(admin);
    await expect(admin.getByRole("row", { name: /e2e key/ })).toContainText("Revoked");
    const after = await fetch(`${state().base}/api/collections/users/records`, { headers: { Authorization: `Bearer ${key}` } });
    expect([401, 403]).toContain(after.status);
});

test("Webhooks: create an endpoint, see the signing secret once", async ({ admin }) => {
    await admin.goto("/_/#/webhooks");
    await admin.getByRole("button", { name: "New webhook" }).click();
    await admin.getByLabel("URL (https)").fill("https://example.com/hooks/claviger");
    await admin.getByLabel("Description").fill("e2e hook");
    await admin.getByLabel("Events (one per line)").fill("users:create");
    await admin.getByRole("button", { name: "Create" }).click();

    await expect(admin.getByLabel("Signing secret")).toHaveValue(/.{16,}/);
    await admin.getByRole("button", { name: "Done" }).click();

    const row = admin.getByRole("row", { name: /example\.com\/hooks\/claviger/ });
    await expect(row).toBeVisible();
    await expect(row).toContainText("users:create");
});
