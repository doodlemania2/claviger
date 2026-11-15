import type { Browser } from "@playwright/test";
import type { World } from "../lib/world";
import { firstRun } from "./dev-first-run";

export type SceneFn = (browser: Browser, world: World, index: number) => Promise<void>;

export const SCENES: Record<string, SceneFn> = {
    "first-run": firstRun,
};
