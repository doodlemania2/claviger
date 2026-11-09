import { test, expect, state, api } from "../fixtures";
import { execFileSync } from "node:child_process";
import { join } from "node:path";

function dbValue(sql: string): string {
    // The server holds the database open in WAL mode; a read-only query is fine.
    return execFileSync("sqlite3", ["-readonly", join(state().data, "data.db"), sql], { encoding: "utf8" }).trim();
}

test("Encrypted fields: add both types in the collection editor, create and edit a record", async ({ admin }) => {
    await api("POST", "/api/collections", {
        name: "e2e_notes",
        type: "base",
        fields: [{ name: "title", type: "text" }],
    });

    await admin.goto("/_/#/collections?collection=e2e_notes");
    await admin.getByRole("button", { name: "Collection settings" }).click();

    const editor = admin.locator(".modal", { hasText: "Edit e2e_notes collection" }).last();
    for (const label of ["Encrypted text", "Encrypted JSON"]) {
        await editor.getByRole("button", { name: "New field" }).click();
        await admin.locator(".field-types-dropdown").getByRole("button", { name: label }).click();
    }
    await editor.getByRole("button", { name: "Save changes" }).click();
    await expect(editor).toBeHidden();

    const col = await api("GET", "/api/collections/e2e_notes");
    expect(col.fields.map((f: any) => f.type)).toEqual(expect.arrayContaining(["encryptedText", "encryptedJSON"]));

    // ---- create a record in the UI
    await admin.locator(".new-record-btn").click();
    await expect(admin.getByRole("heading", { name: "Create e2e_notes record" })).toBeVisible();
    await admin.getByRole("textbox", { name: "title", exact: true }).fill("first");
    await admin.getByRole("textbox", { name: "encryptedText", exact: true }).fill("the pastor's number is 555-0100");
    // the encrypted JSON field uses the json field's editor
    await admin.locator(".field-type-json").getByRole("textbox").fill('{"phone": "555-0100"}');
    await admin.getByRole("button", { name: "Create", exact: true }).click();
    await expect(admin.getByRole("heading", { name: "Create e2e_notes record" })).toBeHidden();

    const list = await api("GET", "/api/collections/e2e_notes/records");
    expect(list.items).toHaveLength(1);
    const item = list.items[0];
    expect(item.encryptedText).toBe("the pastor's number is 555-0100");
    expect(item.encryptedJSON).toEqual({ phone: "555-0100" });

    // at rest it is ciphertext
    const stored = dbValue(`select encryptedText || '|' || encryptedJSON from e2e_notes where id='${item.id}'`);
    expect(stored).toMatch(/^enc:v1:[^|]+\|enc:v1:/);
    expect(stored).not.toContain("555-0100");

    // ---- edit it in the UI
    await admin.getByRole("row", { name: /first/ }).click();
    await expect(admin.getByRole("textbox", { name: "encryptedText", exact: true })).toHaveValue("the pastor's number is 555-0100");
    await admin.getByRole("textbox", { name: "encryptedText", exact: true }).fill("changed");
    await admin.getByRole("button", { name: "Save changes" }).click();
    await expect(admin.getByRole("textbox", { name: "encryptedText", exact: true })).toBeHidden();
    expect((await api("GET", `/api/collections/e2e_notes/records/${item.id}`)).encryptedText).toBe("changed");

    // the encryption keys page now lists the fields
    await admin.goto("/_/#/settings/encryption-keys");
    await expect(admin.getByRole("row", { name: /e2e_notes\.encryptedText/ })).toBeVisible();
    await expect(admin.getByRole("row", { name: /e2e_notes\.encryptedJSON/ })).toBeVisible();
});

test("Encryption keys: rotating a key updates the table without a reload", async ({ admin }) => {
    await admin.goto("/_/#/settings/encryption-keys");
    const row = admin.getByRole("row", { name: /e2e_notes\.encryptedText/ });
    const before = await row.locator("code").first().innerText();
    await row.getByRole("button", { name: /rotate/i }).click();
    // a confirmation may follow
    const yes = admin.locator(".modal.popup.manual").getByRole("button", { name: "Yes" });
    if (await yes.isVisible().catch(() => false)) await yes.click();
    await expect(row.locator("code").first()).not.toHaveText(before, { timeout: 20_000 });
});
