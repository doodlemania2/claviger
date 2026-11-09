import { test, expect } from "../fixtures";

// Every admin UI page loads, renders its title and produces no console error
// and no Content-Security-Policy violation (the `admin` fixture asserts both).
const pages: Array<[route: string, marker: RegExp]> = [
    ["collections", /Collections/],
    ["logs", /Include requests by superusers/],
    ["audit", /Record id/],
    ["webhooks", /New webhook/],
    ["orgs", /SCIM provisioning/],
    ["oidc", /Signing keys/],
    ["settings", /Application URL/],
    ["settings/mail", /Sender address/],
    ["settings/security", /authenticator app/],
    ["settings/storage", /storage/i],
    ["settings/backups", /Initialize new backup/],
    ["settings/crons", /__pbDBOptimize__/],
    ["settings/api-keys", /New API key/],
    ["settings/encryption-keys", /Rotate all/],
    ["settings/captcha", /Provider/],
    ["settings/export-collections", /collections/i],
    ["settings/import-collections", /collections/i],
    ["settings/sql", /SQL/],
];

for (const [route, marker] of pages) {
    test(`admin page #/${route} loads cleanly`, async ({ admin }) => {
        await admin.goto(`/_/#/${route}`);
        await expect(admin.locator("body")).toContainText(marker);
        // let late fetches and renders settle so their errors are collected
        await admin.waitForTimeout(400);
    });
}
