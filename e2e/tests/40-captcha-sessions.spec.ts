import { test, expect, state, api, confirmYes } from "../fixtures";

test("Captcha settings: configure a provider, the hosted pages' CSP opens only for it, then turn it off", async ({ admin }) => {
    await admin.goto("/_/#/settings/captcha");
    await admin.locator("#captcha_provider").selectOption("turnstile");

    // Cloudflare's published always-pass test keys: nothing here talks to the provider.
    await admin.getByLabel("Site key (public)").fill("1x00000000000000000000AA");
    await admin.getByLabel("Secret key").fill("1x0000000000000000000000000000000AA");
    await admin.getByLabel("Passkey sign-in (login-begin)").check();
    await admin.getByRole("button", { name: "Save changes" }).click();
    await expect(admin.getByText("Successfully saved captcha settings.").first()).toBeVisible();

    const withCaptcha = await fetch(`${state().base}/passkeys`);
    const csp = withCaptcha.headers.get("content-security-policy") || "";
    expect(csp).toContain("challenges.cloudflare.com");

    // The secret is write-only: the API never hands it back.
    const settings = await api("GET", "/api/settings");
    expect(JSON.stringify(settings.captcha)).not.toContain("1x0000000000000000000000000000000AA");

    await admin.reload();
    await admin.locator("#captcha_provider").selectOption("");
    await admin.getByRole("button", { name: "Save changes" }).click();
    await expect(admin.getByText("Successfully saved captcha settings.").last()).toBeVisible();

    const without = await fetch(`${state().base}/passkeys`);
    expect(without.headers.get("content-security-policy") || "").not.toContain("cloudflare");
});

test("Sessions and ban: list a user's sessions, ban the account, unban it", async ({ admin }) => {
    const { user, base } = state();
    const signIn = () =>
        fetch(`${base}/api/collections/users/auth-with-password`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ identity: user.email, password: user.password }),
        });
    expect((await signIn()).status).toBe(200);

    await admin.goto("/_/#/collections?collection=users");
    await admin.getByRole("row", { name: new RegExp(user.email) }).click();
    await admin.getByRole("button", { name: "More options" }).click();
    await admin.getByText("Sessions and ban").click();

    const dialog = admin.locator(".modal", { hasText: "Sessions and ban:" });
    await expect(dialog).toContainText(user.email);
    await expect(dialog.getByText("Active").first()).toBeVisible();
    await expect(dialog).toContainText("This account is not banned.");

    await dialog.getByLabel("Reason (visible to superusers only)").fill("e2e ban");
    await dialog.getByRole("button", { name: "Ban account" }).click();
    await confirmYes(admin);
    await expect(dialog).toContainText("Banned");
    await expect(dialog).toContainText("e2e ban");
    expect((await signIn()).status).toBeGreaterThanOrEqual(400);

    await dialog.getByRole("button", { name: "Unban" }).click();
    await expect(dialog).toContainText("This account is not banned.");
    expect((await signIn()).status).toBe(200);
});
