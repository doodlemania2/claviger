import { defineConfig } from "@playwright/test";

// The suite owns one Claviger instance (started by global-setup.ts) and runs
// its files in order against it: later specs rely on fixtures earlier ones made.
export const PORT = Number(process.env.E2E_PORT || 18090);

export default defineConfig({
    globalSetup: "./global-setup.ts",
    globalTeardown: "./global-teardown.ts",
    fullyParallel: false,
    workers: 1,
    retries: process.env.CI ? 1 : 0,
    timeout: 60_000,
    expect: { timeout: 10_000 },
    reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
    projects: [
        { name: "e2e", testDir: "./tests" },
        // the screenshot kit: starts its own instance on free ports, so it needs nothing from global-setup
        { name: "guides", testDir: "./guides", testMatch: "*.spec.ts" },
    ],
    use: {
        baseURL: `http://localhost:${PORT}`,
        trace: "retain-on-failure",
        screenshot: "only-on-failure",
        // pinned build, so the cached browser matches without a download
        browserName: "chromium",
    },
});
