import { defineConfig } from "@playwright/test";

/**
 * The recorded walkthroughs. They are not part of CI and not part of the default e2e run: the main
 * config has `testDir: "./tests"`, so nothing here is picked up there, and no workflow runs this file.
 * Start it with e2e/walkthrough/run.sh, which builds the binary and stitches the videos afterwards.
 */
export default defineConfig({
    testDir: ".",
    testMatch: /walkthrough\.walk\.ts$/,
    fullyParallel: false,
    workers: 1,
    retries: 0,
    timeout: 45 * 60_000,
    expect: { timeout: 20_000 },
    reporter: "list",
    outputDir: "./.out/playwright",
    use: {
        browserName: "chromium",
        launchOptions: {
            slowMo: Number(process.env.WALK_SLOWMO ?? (process.env.WALK_FAST ? 0 : 200)),
        },
    },
});
