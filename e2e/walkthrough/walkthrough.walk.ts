import { test, type Browser } from "@playwright/test";
import { STORIES, storyOf } from "./lib/story";
import { configureInstance, ensureSuperuser, startStack, stopStack } from "./lib/stack";
import { currentWorld, saveWorld, type World } from "./lib/world";
import { SCENES } from "./scenes";

/**
 * The walkthrough: scenes run in order and share one World (people, ids, notes) saved in the run
 * directory. WALK_STORY=developer|parish picks the story and its scenes; WALK_ONLY=2,3 (numbers or
 * names) runs some of them; WALK_RUN_DIR resumes an earlier run (same data dir, same sign-ins).
 */
export type SceneFn = (browser: Browser, world: World, index: number) => Promise<void>;

const only = (process.env.WALK_ONLY ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);

let shared: World | undefined;
const getWorld = (): World => {
    if (!shared) {
        shared = currentWorld();
        const story = STORIES[shared.story];
        if (!shared.admin.email) shared.admin.email = story.adminEmail;
        for (const [k, p] of Object.entries(story.cast)) shared.people[k] ??= { ...p };
        saveWorld(shared);
    }
    return shared;
};

test.describe.configure({ mode: "serial" });

test.beforeAll(async () => {
    await startStack(getWorld());
});
test.afterAll(async () => {
    const w = shared;
    await stopStack();
    if (w) {
        saveWorld(w);
        console.log(`[summary] run directory: ${w.dir}`);
        console.log(`[summary] skipped steps: ${JSON.stringify(w.skipped)}`);
    }
});

// One slot per scene of the longest story. A slot past the end of the run's story is skipped.
const SLOTS = Math.max(...Object.values(STORIES).map((s) => s.scenes.length));
Array.from({ length: SLOTS }, (_, i) => i + 1).forEach((number) => {
    test(`scene ${String(number)}`, async ({ browser }) => {
        const world = getWorld();
        const id = storyOf(world).scenes[number - 1];
        const run = id ? SCENES[id] : undefined;
        test.skip(!id || !run, "this story has fewer scenes");
        test.skip(only.length > 0 && !only.includes(String(number)) && !only.includes(id ?? ""));
        console.log(`[scene] ${String(number)}: ${id ?? ""} (${world.story})`);
        if (id !== "first-run" && world.story === "developer") {
            await ensureSuperuser(world);
            await configureInstance(world, "Alder and Finch Auth");
        }
        await (run as SceneFn)(browser, world, number);
        saveWorld(world);
    });
});
