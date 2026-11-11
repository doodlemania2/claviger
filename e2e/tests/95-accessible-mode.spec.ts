import { test, expect, state } from "../fixtures";
import type { Page } from "@playwright/test";

// Accessible mode of the hosted pages (#83, 83-3). @axe-core/playwright is not a dependency of
// this suite, so labels, focus, landmarks and contrast-relevant styles are checked by hand here.
// A screen reader pass and an axe run are listed in docs/accessibility.md.

const PAGES = [
    { name: "/passkeys", path: "/passkeys" },
    { name: "/orgs error page", path: "/orgs/invite/not-a-real-token" },
    { name: "/oidc hosted page", path: "/oidc/logged-out" },
];

const toggle = (page: Page) => page.locator("#a11yToggle");
const px = (page: Page, sel: string, prop: string) =>
    page.locator(sel).first().evaluate((el, p) => parseFloat(getComputedStyle(el).getPropertyValue(p)), prop);

for (const p of PAGES) {
    test(`Accessible mode on ${p.name}: toggle, persistence, landmarks`, async ({ guarded: page, problems }) => {
        problems.ignore(/status of 404/); // the error page is a 404 on purpose
        const res = await page.goto(p.path);
        const csp = res!.headers()["content-security-policy"] || "";
        expect(csp).not.toContain("unsafe-inline");

        // landmarks, language, and a skip link that leads to <main>
        await expect(page.locator("html")).toHaveAttribute("lang", "en");
        await expect(page.locator("main")).toHaveCount(1);
        await expect(page.locator("header.a11y-bar")).toHaveCount(1);
        await expect(page.locator("a.skip")).toHaveAttribute("href", "#app");

        // a real button, labelled, off at first
        const t = toggle(page);
        await expect(t).toBeVisible();
        await expect(t).toHaveText("Larger text and high contrast");
        await expect(t).toHaveAttribute("aria-pressed", "false");
        await expect(page.locator("html")).not.toHaveAttribute("data-a11y", /.*/);
        const small = await px(page, "body", "font-size");
        expect(small).toBeLessThan(18);
        expect(await px(page, "button", "min-height")).toBeGreaterThanOrEqual(24);

        await t.click();
        await expect(t).toHaveAttribute("aria-pressed", "true");
        await expect(page.locator("html")).toHaveAttribute("data-a11y", "large");
        expect(await px(page, "body", "font-size")).toBeGreaterThanOrEqual(20);
        expect(await px(page, "#a11yToggle", "min-height")).toBeGreaterThanOrEqual(44);
        // high contrast: black text on white (the browser is in light mode)
        expect(await page.locator("body").evaluate((el) => getComputedStyle(el).color)).toBe("rgb(0, 0, 0)");

        // the choice survives a reload, before the first paint
        await page.reload();
        await expect(page.locator("html")).toHaveAttribute("data-a11y", "large");
        await expect(toggle(page)).toHaveAttribute("aria-pressed", "true");

        await toggle(page).click();
        await expect(page.locator("html")).not.toHaveAttribute("data-a11y", /.*/);
        await page.reload();
        await expect(toggle(page)).toHaveAttribute("aria-pressed", "false");
    });
}

test("Accessible mode works when storage is blocked", async ({ browser, problems }) => {
    const ctx = await browser.newContext({ baseURL: state().base });
    const page = await ctx.newPage();
    await page.addInitScript(() => {
        const boom = () => {
            throw new DOMException("blocked", "SecurityError");
        };
        for (const m of ["getItem", "setItem", "removeItem"]) (Storage.prototype as any)[m] = boom;
    });
    page.on("pageerror", (e) => problems.list.push(`pageerror: ${e.message}`));
    await page.goto("/passkeys");
    await toggle(page).click();
    await expect(page.locator("html")).toHaveAttribute("data-a11y", "large");
    await expect(toggle(page)).toHaveAttribute("aria-pressed", "true");
    await ctx.close();
});

test("The operating system's contrast and motion settings apply without the toggle", async ({ guarded: page }) => {
    await page.emulateMedia({ contrast: "more", reducedMotion: "reduce" });
    await page.goto("/passkeys");
    await expect(page.locator("html")).not.toHaveAttribute("data-a11y", /.*/);
    expect(await page.locator("body").evaluate((el) => getComputedStyle(el).color)).toBe("rgb(0, 0, 0)");
    expect(await page.locator("body").evaluate((el) => getComputedStyle(el).backgroundColor)).toBe("rgb(255, 255, 255)");
    // the body is not larger: only the palette changed
    expect(await px(page, "body", "font-size")).toBeLessThan(18);
});

test("Passkeys page: labels, focus and error text are wired for assistive technology", async ({ guarded: page, problems }) => {
    problems.ignore(/auth-with-password/); // the wrong password is the point
    await page.goto("/passkeys");

    // every visible field has a label tied by for/id, and so a name
    for (const id of ["identity"]) {
        await expect(page.locator(`label[for="${id}"]`)).toHaveCount(1);
        await expect(page.locator(`#${id}`)).toHaveAccessibleName("Email or username");
    }
    await page.locator("#linkPasswordLogin").click();
    await expect(page.locator("#linkPasswordLogin")).toHaveAttribute("aria-expanded", "true");
    await expect(page.locator("#pwEmail")).toHaveAccessibleName("Email");
    await expect(page.locator("#pwPass")).toHaveAccessibleName("Password");
    await expect(page.locator("#pwEmail")).toBeFocused();

    // keyboard focus is visible: a real outline of at least 2px
    await page.locator("#pwPass").focus();
    await page.keyboard.press("Tab");
    await expect(page.locator("#btnPwLogin")).toBeFocused();
    const outline = await page.locator("#btnPwLogin").evaluate((el) => {
        const s = getComputedStyle(el);
        return { style: s.outlineStyle, width: parseFloat(s.outlineWidth) };
    });
    expect(outline.style).not.toBe("none");
    expect(outline.width).toBeGreaterThanOrEqual(2);

    // a failed sign-in puts the error next to the form, linked to the fields
    await page.locator("#pwEmail").fill("nobody@example.test");
    await page.locator("#pwPass").fill("wrong-password");
    await page.locator("#btnPwLogin").click();
    const err = page.locator("#pwMsg .msg.err");
    await expect(err).toBeVisible();
    await expect(err).toHaveAttribute("role", "alert");
    await expect(page.locator("#pwEmail")).toHaveAttribute("aria-invalid", "true");
    await expect(page.locator("#pwEmail")).toHaveAttribute("aria-describedby", "pwMsg");
    await expect(page.locator("#pwPass")).toHaveAttribute("aria-describedby", "pwMsg");
});

test("The toggle and the skip link are in the language of the page", async ({ guarded: page }) => {
    await page.goto("/passkeys?lang=es");
    await expect(page.locator("html")).toHaveAttribute("lang", "es");
    await expect(toggle(page)).toHaveText("Texto más grande y alto contraste");
    await expect(page.locator("a.skip")).toHaveText("Saltar al contenido principal");

    await page.goto("/passkeys?lang=ar");
    await expect(page.locator("html")).toHaveAttribute("dir", "rtl");
    await page.goto("/passkeys?lang=en");
});

test("The skip link moves focus to the main content", async ({ guarded: page }) => {
    await page.goto("/passkeys");
    await page.keyboard.press("Tab");
    await expect(page.locator("a.skip")).toBeFocused();
    await expect(page.locator("a.skip")).toBeInViewport();
    await page.keyboard.press("Enter");
    await expect(page.locator("#app")).toBeFocused();
});
