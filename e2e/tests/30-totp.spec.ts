import { test, expect, api, state, totp, watch } from "../fixtures";
import { randomBytes } from "node:crypto";

// A dedicated superuser: enrolling TOTP changes how that account signs in, and
// the shared admin account must keep working for the other specs.
test("TOTP: enroll a superuser in the UI, then sign in with a generated code and a backup code", async ({ browser, problems }) => {
    const email = "totp-admin@example.com";
    const password = randomBytes(12).toString("hex");
    await api("POST", "/api/collections/_superusers/records", { email, password, passwordConfirm: password });

    async function open() {
        const ctx = await browser.newContext({ baseURL: state().base });
        const page = await ctx.newPage();
        watch(page, problems);
        return { ctx, page };
    }

    async function passwordLogin(page: import("@playwright/test").Page) {
        await page.goto("/_/#/login");
        await page.getByRole("textbox", { name: /^Email/ }).fill(email);
        await page.getByRole("textbox", { name: /^Password/ }).fill(password);
        await page.getByRole("button", { name: "Login" }).click();
    }

    // ---- enroll
    const first = await open();
    await passwordLogin(first.page);
    await expect(first.page).toHaveURL(/#\/collections/);
    await first.page.goto("/_/#/settings/security");
    await first.page.getByRole("textbox", { name: /^Current password/ }).fill(password);
    await first.page.getByRole("button", { name: "Set up authenticator app" }).click();

    const secret = await first.page.locator(".field", { hasText: "Setup key" }).locator("input").inputValue();
    expect(secret).toMatch(/^[A-Z2-7]{16,}$/);
    expect(await first.page.locator(".field", { hasText: "otpauth URI" }).locator("input").inputValue()).toContain("otpauth://totp/");

    await first.page.getByLabel("First 6 digit code").fill(totp(secret));
    await first.page.getByRole("button", { name: "Confirm and enable" }).click();

    await expect(first.page.getByText("Save these backup codes now.")).toBeVisible();
    const codes = (await first.page.locator("#totp-backup-codes").innerText()).trim().split(/\s+/);
    expect(codes.length).toBeGreaterThanOrEqual(8);
    await first.page.getByRole("button", { name: "I saved them" }).click();
    await expect(first.page.getByText("enabled", { exact: true })).toBeVisible();
    await first.ctx.close();

    // ---- password alone no longer signs in; the next time step's code does
    const second = await open();
    await passwordLogin(second.page);
    await expect(second.page.getByLabel("Authenticator or backup code")).toBeVisible();
    await expect(second.page).not.toHaveURL(/#\/collections/);
    // a wrong code is refused and keeps the form
    await second.page.getByLabel("Authenticator or backup code").fill("000000");
    await second.page.getByRole("button", { name: /next|login|confirm/i }).last().click();
    await expect(second.page).not.toHaveURL(/#\/collections/);
    await second.page.getByLabel("Authenticator or backup code").fill(totp(secret, Date.now() + 30_000));
    await second.page.getByRole("button", { name: /next|login|confirm/i }).last().click();
    await expect(second.page).toHaveURL(/#\/collections/);
    await second.ctx.close();

    // ---- a backup code works once
    const third = await open();
    await passwordLogin(third.page);
    await third.page.getByLabel("Authenticator or backup code").fill(codes[0]);
    await third.page.getByRole("button", { name: /next|login|confirm/i }).last().click();
    await expect(third.page).toHaveURL(/#\/collections/);
    await third.ctx.close();

    const fourth = await open();
    await passwordLogin(fourth.page);
    await fourth.page.getByLabel("Authenticator or backup code").fill(codes[0]);
    await fourth.page.getByRole("button", { name: /next|login|confirm/i }).last().click();
    await expect(fourth.page).not.toHaveURL(/#\/collections/);
    await fourth.ctx.close();

    // The wrong-code attempts above are expected 4xx answers: Chromium logs
    // them as console errors. They are the only ones allowed here.
    problems.ignore(/status of 4\d\d/);
});
