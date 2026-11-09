import { defineConfig } from "@playwright/test";

// The suite owns one Claviger instance (started by global-setup.ts) and runs
// its files in order against it: later specs rely on fixtures earlier ones made.
export const PORT = Number(process.env.E2E_PORT || 18090);

export default defineConfig({
    testDir: "./tests",
    globalSetup: "./global-setup.ts",
    globalTeardown: "./global-teardown.ts",
    fullyParallel: false,
    workers: 1,
    retries: process.env.CI ? 1 : 0,
    timeout: 60_000,
    expect: { timeout: 10_000 },
    reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
    use: {
        baseURL: `http://localhost:${PORT}`,
        trace: "retain-on-failure",
        screenshot: "only-on-failure",
        // pinned build, so the cached browser matches without a download
        browserName: "chromium",
    },
});
