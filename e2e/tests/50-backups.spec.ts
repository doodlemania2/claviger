import { test, expect, state, api } from "../fixtures";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

test("Backups: switch encryption on, create a backup, it is stored as .zip.enc and is not a zip", async ({ admin }) => {
    await admin.goto("/_/#/settings/backups");

    await admin.getByRole("button", { name: "Backup options" }).click();
    await admin.getByLabel("Encrypt new backups").check({ force: true });
    await expect(admin.getByText(/lose the key-encryption key/)).toBeVisible();
    await admin.getByRole("button", { name: "Save changes" }).click();
    await expect(admin.getByText(/saved/i).first()).toBeVisible();
    expect((await api("GET", "/api/settings")).backups.encrypt).toBe(true);

    await admin.getByRole("button", { name: "Initialize new backup" }).click();
    const dialog = admin.locator(".modal", { hasText: "Initialize new backup" }).last();
    await dialog.getByLabel(/Backup name/i).fill("e2e.zip");
    await dialog.getByRole("button", { name: /Start backup|Create/i }).click();

    const row = admin.locator(".list-item", { hasText: "e2e.zip.enc" });
    await expect(row).toBeVisible({ timeout: 30_000 });
    await expect(row.locator(".ri-lock-2-line")).toBeVisible();

    const dir = join(state().data, "backups");
    const files = readdirSync(dir).filter((f) => f.startsWith("e2e") && !f.endsWith(".attrs"));
    expect(files).toEqual(["e2e.zip.enc"]);
    const head = readFileSync(join(dir, "e2e.zip.enc")).subarray(0, 4);
    expect(head.toString("hex")).not.toBe("504b0304"); // a zip starts with PK\x03\x04

    // switch it off again: the next backup is a plain zip, the encrypted one stays listed
    if (!(await admin.getByLabel("Encrypt new backups").isVisible())) {
        await admin.getByRole("button", { name: "Backup options" }).click();
    }
    await admin.getByLabel("Encrypt new backups").uncheck({ force: true });
    await admin.getByRole("button", { name: "Save changes" }).click();
    await expect.poll(async () => (await api("GET", "/api/settings")).backups.encrypt).toBe(false);
});
