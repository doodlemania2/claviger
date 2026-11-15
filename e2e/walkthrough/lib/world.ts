import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { randomBytes } from "node:crypto";

export type StoryId = "developer" | "parish";

/** A person of the story. The password is made up and only ever valid on the throwaway local instance. */
export interface Person {
    name: string;
    email: string;
    password: string;
    /** The user record id, once the person exists. */
    id?: string;
}

/** What one walkthrough run knows. It lives in `<out>/world.json`, so a single scene can be re-run. */
export interface World {
    stamp: string;
    dir: string;
    story: StoryId;
    port: number;
    base: string;
    dataDir: string;
    /** 32 characters: the settings encryption, and (through HKDF) the field-encryption key of this throwaway instance. */
    encKey: string;
    admin: { email: string; password: string };
    people: Record<string, Person>;
    ids: Record<string, string>;
    notes: Record<string, string>;
    /** Steps the product could not show, with the reason. The run summary lists them. */
    skipped: { scene: string; step: string; reason: string }[];
}

export const OUT_ROOT = resolve(__dirname, "..", ".out");
export const OTP = "123456";

const pad = (n: number) => String(n).padStart(2, "0");

function stamp(now: Date): string {
    return `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`;
}

export function storyFromEnv(): StoryId {
    const s = process.env.WALK_STORY || "developer";
    if (s !== "developer" && s !== "parish") throw new Error(`WALK_STORY must be developer or parish, got "${s}"`);
    return s;
}

export function newWorld(): World {
    const s = stamp(new Date());
    const dir = join(OUT_ROOT, s);
    mkdirSync(join(dir, "auth"), { recursive: true });
    mkdirSync(join(dir, "scenes"), { recursive: true });
    const port = Number(process.env.WALK_PORT || 18190); // a default: startStack picks a free one when it is taken
    const world: World = {
        stamp: s,
        dir,
        story: storyFromEnv(),
        port,
        base: `http://localhost:${port}`,
        dataDir: join(dir, "pb_data"),
        encKey: randomBytes(24).toString("base64").slice(0, 32),
        admin: { email: "", password: randomBytes(9).toString("hex") },
        people: {},
        ids: {},
        notes: {},
        skipped: [],
    };
    saveWorld(world);
    return world;
}

export function loadWorld(dir: string): World {
    return JSON.parse(readFileSync(join(dir, "world.json"), "utf8")) as World;
}

export function saveWorld(world: World): void {
    writeFileSync(join(world.dir, "world.json"), JSON.stringify(world, null, 2));
}

/** The run directory: `WALK_RUN_DIR` resumes an old run, otherwise a new one starts. */
export function currentWorld(): World {
    const dir = process.env.WALK_RUN_DIR;
    if (dir && existsSync(join(dir, "world.json"))) return loadWorld(dir);
    return newWorld();
}

/** Development aid: with a resumed run a block that finished earlier is not repeated. */
export function once(world: World, key: string): boolean {
    if (world.notes[`done:${key}`]) return false;
    world.notes[`done:${key}`] = "yes";
    saveWorld(world);
    return true;
}
