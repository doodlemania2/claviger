import { existsSync, mkdirSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { devices, type Browser, type BrowserContext, type Page } from "@playwright/test";
import { overlayInit } from "./overlay";
import { savePasskeys, virtualAuthenticator } from "./passkey";
import { saveWorld, type World } from "./world";

export type DeviceKind = "desktop" | "phone";

const PROFILES: Record<DeviceKind, Record<string, unknown>> = {
    desktop: { viewport: { width: 1280, height: 800 } },
    phone: { ...devices["Pixel 7"] },
};

export interface ActorSpec {
    name: string;
    device: DeviceKind;
    /** The label in the corner of the recording. */
    label?: string;
    /** Give this actor a virtual passkey authenticator, holding the passkeys it made in earlier scenes. */
    passkey?: boolean;
}

export interface Actor {
    name: string;
    page: Page;
    ctx: BrowserContext;
    /** Show a caption at the bottom of this actor's screen, and hold long enough to read it. */
    say: (text: string, ms?: number) => Promise<void>;
    /** Show a card (a title and monospace lines) for something that has no screen. An empty title clears it. */
    card: (title: string | null, lines?: string[]) => Promise<void>;
    passkeys?: { count: () => Promise<number> };
}

export type Layout = "sequence" | "row";

/** One caption as shown on screen, in seconds from the start of that actor's recording. It feeds the .vtt files. */
export interface Cue {
    actor: string;
    label: string;
    start: number;
    end: number;
    text: string;
}

/** The overlay hides a caption by itself after this long (see overlay.ts). */
const CAPTION_MS = 9000;

export interface Clip {
    actor: string;
    file: string;
    width: number;
    height: number;
}

/** 0 turns the reading pauses off (a fast run to check that the scenes work). */
export const PACE = process.env.WALK_FAST ? 0 : Number(process.env.WALK_PACE ?? 1);

/** One scene: a few actors recorded at the same time, a title card and a manifest for the stitcher. */
export class SceneRun {
    private readonly actors = new Map<string, Actor>();
    private readonly specs = new Map<string, ActorSpec>();
    readonly cues: Cue[] = [];

    private constructor(
        readonly world: World,
        readonly index: number,
        readonly id: string,
        readonly title: string,
        readonly subtitle: string,
        readonly dir: string,
        readonly layout: Layout,
    ) {}

    static async begin(browser: Browser, world: World, index: number, id: string, title: string, subtitle: string, specs: ActorSpec[], layout: Layout = "sequence"): Promise<SceneRun> {
        const dir = join(world.dir, "scenes", `${String(index).padStart(2, "0")}-${id}`);
        mkdirSync(dir, { recursive: true });
        const scene = new SceneRun(world, index, id, title, subtitle, dir, layout);
        // Contexts open in parallel and finish in any order; the clips keep the order given here.
        specs.forEach((spec) => scene.specs.set(spec.name, spec));
        await Promise.all(specs.map((spec) => scene.addActor(browser, spec)));
        return scene;
    }

    private async addActor(browser: Browser, spec: ActorSpec): Promise<void> {
        const profile = PROFILES[spec.device];
        const viewport = (profile["viewport"] ?? { width: 1280, height: 800 }) as { width: number; height: number };
        const auth = join(this.world.dir, "auth", `${spec.name}.json`);
        const ctx = await browser.newContext({
            ...profile,
            acceptDownloads: true,
            recordVideo: { dir: join(this.dir, "raw", spec.name), size: viewport },
            ...(existsSync(auth) ? { storageState: auth } : {}),
        });
        ctx.setDefaultTimeout(20_000);
        await ctx.addInitScript(overlayInit);
        const page = await ctx.newPage();
        const t0 = Date.now();
        const secs = () => (Date.now() - t0) / 1000;
        // the cue that is on screen now, closed when it is replaced, cleared or times out
        let live: Cue | null = null;
        const close = (): void => {
            if (live) live.end = Math.min(secs(), live.start + CAPTION_MS / 1000);
            live = null;
        };
        const open = (text: string): void => {
            close();
            live = { actor: spec.name, label: spec.label ?? spec.name, start: secs(), end: secs() + 2, text };
            this.cues.push(live);
        };
        page.on("pageerror", (e) => console.log(`[${spec.name}] pageerror ${e.message.slice(0, 160)}`));
        page.on("response", (r) => {
            if (r.status() >= 500) console.log(`[${spec.name}] ${r.status()} ${r.request().method()} ${new URL(r.url()).pathname}`);
        });
        const label = spec.label ?? spec.name;
        await page.addInitScript((l) => {
            try {
                sessionStorage.setItem("walk-label", l);
            } catch {
                /* storage can be blocked */
            }
        }, label);
        const say = async (text: string, ms?: number): Promise<void> => {
            close();
            if (text) open(text);
            await page
                .evaluate((t) => {
                    (window as unknown as { __walkCaption?: (x: string | null) => void }).__walkCaption?.(t || null);
                }, text)
                .catch(() => undefined);
            // Reading time: about 45 ms a character, at least a second and a half.
            if (text && PACE > 0) await page.waitForTimeout((ms ?? Math.max(1500, text.length * 45)) * PACE);
        };
        const card = async (title: string | null, lines?: string[]): Promise<void> => {
            close();
            if (title) open(title);
            await page
                .evaluate(([t, l]) => {
                    (window as unknown as { __walkCard?: (a: string | null, b?: string[]) => void }).__walkCard?.(t as string | null, l as string[]);
                }, [title, lines ?? []] as const)
                .catch(() => undefined);
        };
        const actor: Actor = { name: spec.name, page, ctx, say, card };
        if (spec.passkey) actor.passkeys = await virtualAuthenticator(page, this.world, spec.name);
        this.actors.set(spec.name, actor);
    }

    actor(name: string): Actor {
        const a = this.actors.get(name);
        if (!a) throw new Error(`no actor ${name} in scene ${this.id}`);
        return a;
    }

    /** Close every context, keep the sign-ins for the next scene and write the manifest. */
    async end(): Promise<void> {
        const clips: Clip[] = [];
        for (const [name, spec] of this.specs) {
            const a = this.actors.get(name);
            if (!a) continue;
            const video = a.page.video();
            if (a.passkeys) await savePasskeys(name).catch(() => undefined);
            await a.ctx.storageState({ path: join(this.world.dir, "auth", `${name}.json`) }).catch(() => undefined);
            await a.ctx.close();
            if (!video) continue;
            const raw = await video.path();
            const file = join(this.dir, `${name}.webm`);
            renameSync(raw, file);
            const vp = (PROFILES[spec.device]["viewport"] ?? { width: 1280, height: 800 }) as { width: number; height: number };
            clips.push({ actor: name, file, width: vp.width, height: vp.height });
        }
        writeFileSync(join(this.dir, "scene.json"), JSON.stringify({ index: this.index, id: this.id, title: this.title, subtitle: this.subtitle, layout: this.layout, clips, cues: this.cues.filter((c) => c.end > c.start) }, null, 2));
        saveWorld(this.world);
    }
}
