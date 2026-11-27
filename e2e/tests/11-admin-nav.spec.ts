import { test, expect } from "../fixtures";

// The header carries a dozen admin pages. On a laptop-width window no label may wrap
// into a taller bar, and every page must stay reachable (a visible link, no clipping).
for (const width of [1280, 1440, 1920]) {
    test(`admin header fits at ${width}px`, async ({ admin }) => {
        await admin.setViewportSize({ width, height: 800 });
        await admin.goto("/_/#/collections");
        await expect(admin.locator(".app-main-nav .header-link").first()).toBeVisible();

        const links = admin.locator(".app-main-nav .header-link");
        const n = await links.count();
        expect(n).toBeGreaterThanOrEqual(12);

        const boxes = [];
        for (let i = 0; i < n; i++) {
            boxes.push(await links.nth(i).boundingBox());
        }
        const heights = new Set(boxes.map((b) => Math.round(b!.height)));
        expect(heights.size, `link heights differ (a label wrapped): ${[...heights]}`).toBe(1);

        // nothing is scrolled out of the nav
        const overflow = await admin.locator(".app-main-nav").evaluate((el) => el.scrollWidth - el.clientWidth);
        expect(overflow).toBeLessThanOrEqual(1);

        await admin.screenshot({ path: `test-results/nav-${width}.png`, clip: { x: 0, y: 0, width, height: 70 } });
    });
}
