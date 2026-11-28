import { test, expect, state, watch, signInAdmin } from "../fixtures";
import { overlayInit } from "../walkthrough/lib/overlay";

// The recording rig (e2e/walkthrough, not part of this suite) puts a caption bar, a name badge, a click
// ripple, a cursor dot and a card on top of the pages. They must be built with DOM and CSSOM only: the
// pages' Content-Security-Policy refuses injected <style> and <script>, and a violation here would also
// break every recording. This check installs the same init script on the admin UI and on a hosted page.

type Walk = { __walkCaption: (t: string | null) => void; __walkLabel: (t: string | null) => void; __walkCard: (t: string | null, l?: string[]) => void };

test("walkthrough overlay installs on the admin UI and a hosted page with no CSP violation or console error", async ({ browser, problems }) => {
    const ctx = await browser.newContext({ baseURL: state().base });
    await ctx.addInitScript(overlayInit);
    const page = await ctx.newPage();
    watch(page, problems);
    // /passkeys starts a discoverable passkey sign-in in the background when it loads. The whole suite
    // runs from one IP, and by this spec it can have spent the per-IP login-begin budget (20 a minute),
    // so that call may answer 429. This spec is about the overlay and the CSP, not the rate limit.
    problems.ignore(/status of 429 .*\/auth-with-webauthn\/login-begin-discoverable/);

    await signInAdmin(page);
    await page.evaluate(() => {
        const w = window as unknown as Walk;
        w.__walkLabel("Overlay check");
        w.__walkCaption("A caption on the admin UI");
        w.__walkCard("A card", ["-> GET /api/health", "<- 200 ok"]);
    });
    await page.mouse.move(300, 300);
    await page.mouse.click(300, 300);
    await expect(page.locator("#walk-caption")).toHaveText("A caption on the admin UI");
    await expect(page.locator("#walk-label")).toHaveText("Overlay check");
    await expect(page.locator("#walk-card")).toContainText("GET /api/health");
    await expect(page.locator("#walk-cursor")).toBeAttached();

    // the same on a hosted page, which has the strictest policy
    await page.goto("/passkeys");
    await page.evaluate(() => (window as unknown as Walk).__walkCaption("A caption on /passkeys"));
    await expect(page.locator("#walk-caption")).toHaveText("A caption on /passkeys");

    const csp = await page.evaluate(() => (window as any).__cspViolations || []);
    expect(csp, "securitypolicyviolation events").toEqual([]);
    await ctx.close();
});
