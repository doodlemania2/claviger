// The local instance the walkthrough films: a real claviger binary with a temp data dir, the SMTP
// sink the e2e suite uses (so email-link steps can be shown), and the rig's small servers.
// Nothing here ever points at a deployment: the base URL is always http://localhost:<port>.
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, openSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { startSmtp } from "../../smtp";
import { Rig } from "./rig";
import { saveWorld, type Person, type World } from "./world";

const E2E = resolve(__dirname, "..", "..");
const ROOT = resolve(E2E, "..");
export const BIN = join(E2E, ".bin", "claviger");

export interface Stack {
    world: World;
    rig: Rig;
    smtp: Awaited<ReturnType<typeof startSmtp>>;
    child: ChildProcess | null;
    metricsToken: string;
}

let current: Stack | null = null;

export function stack(): Stack {
    if (!current) throw new Error("the stack is not started");
    return current;
}

async function waitFor(url: string, ms = 60_000): Promise<void> {
    const end = Date.now() + ms;
    while (Date.now() < end) {
        try {
            if ((await fetch(url)).ok) return;
        } catch {
            /* not up yet */
        }
        await new Promise((r) => setTimeout(r, 200));
    }
    throw new Error(`timed out waiting for ${url}`);
}

export function serverEnv(world: World): NodeJS.ProcessEnv {
    return {
        ...process.env,
        PB_ENCRYPTION_KEY: world.encKey,
        // lets the webhook endpoint be the rig's receiver on localhost (the rig is the only target)
        CLAVIGER_WEBHOOKS_ALLOW_LOCALHOST: "true",
        CLAVIGER_METRICS_TOKEN: "walkthrough-metrics-token",
    };
}

/** Builds the binary unless E2E_SKIP_BUILD is set and it already exists (the same rule as global-setup.ts). */
export function ensureBinary(): void {
    mkdirSync(join(E2E, ".bin"), { recursive: true });
    if (!process.env.E2E_SKIP_BUILD || !existsSync(BIN)) {
        console.log("walkthrough: building claviger");
        execFileSync("go", ["build", "-o", BIN, "./examples/base"], { cwd: ROOT, stdio: "inherit" });
    }
}

export async function startStack(world: World): Promise<Stack> {
    ensureBinary();
    const sandbox = join(world.dir, "sandbox");
    mkdirSync(sandbox, { recursive: true });
    const env = serverEnv(world);
    const flags = ["--dir", world.dataDir, "--encryptionEnv", "PB_ENCRYPTION_KEY"];

    // The parish story starts from an existing superuser; the developer story creates it on camera.
    if (world.story === "parish" && world.admin.email && !world.notes.superuserMade) {
        execFileSync(BIN, ["superuser", "upsert", world.admin.email, world.admin.password, ...flags], { env, cwd: sandbox, stdio: "ignore" });
        world.notes.superuserMade = "yes";
        saveWorld(world);
    }

    const logFile = join(world.dir, "server.log");
    const out = openSync(logFile, "a");
    const child = spawn(BIN, ["serve", "--http", `127.0.0.1:${world.port}`, ...flags], { env, cwd: sandbox, stdio: ["ignore", out, out] });
    child.on("exit", (code) => {
        if (code) console.error(`walkthrough: claviger exited with ${code}, see ${logFile}`);
    });
    await waitFor(`${world.base}/api/health`);

    const smtpPort = world.port + 400;
    const smtp = await startSmtp(smtpPort);
    const rig = new Rig({ port: world.port + 900, base: world.base, mails: () => smtp.mails });
    await rig.listen();
    current = { world, rig, smtp, child, metricsToken: "walkthrough-metrics-token" };
    return current;
}

export async function stopStack(): Promise<void> {
    const s = current;
    if (!s) return;
    current = null;
    await s.rig.close();
    await s.smtp.close();
    s.child?.kill("SIGTERM");
    await new Promise((r) => setTimeout(r, 500));
}

/** The installer link the server prints while it has no superuser. */
export function installerUrl(world: World): string {
    const log = readFileSync(join(world.dir, "server.log"), "utf8");
    const m = [...log.matchAll(/http:\/\/[^\s]+\/_\/#\/pbinstall\/\S+/g)].pop();
    if (!m) throw new Error("no installer link in the server log (is there a superuser already?)");
    return m[0].replace("127.0.0.1", "localhost");
}

// ---- API helpers (off camera: they prepare the stage, the camera shows the screens) ----------------

export async function rawCall(method: string, path: string, body?: unknown, token?: string): Promise<{ status: number; json: any; text: string }> {
    const res = await fetch(stack().world.base + path, {
        method,
        headers: { "Content-Type": "application/json", ...(token ? { Authorization: token } : {}) },
        body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    let json: any = {};
    try {
        json = text ? JSON.parse(text) : {};
    } catch {
        /* not JSON */
    }
    return { status: res.status, json, text };
}

const tokens = new Map<string, string>();

export async function login(collection: string, identity: string, password: string): Promise<string> {
    const r = await rawCall("POST", `/api/collections/${collection}/auth-with-password`, { identity, password });
    if (r.status !== 200) throw new Error(`sign-in as ${identity} failed: ${r.status} ${r.text}`);
    tokens.set(`${collection}:${identity}`, r.json.token);
    return r.json.token;
}

export async function adminToken(): Promise<string> {
    const w = stack().world;
    const cached = w.notes.adminToken;
    if (cached) return cached;
    const t = await login("_superusers", w.admin.email, w.admin.password);
    w.notes.adminToken = t;
    saveWorld(w);
    return t;
}

/** A superuser call that throws on a non-2xx answer. */
export async function api(method: string, path: string, body?: unknown): Promise<any> {
    const r = await rawCall(method, path, body, await adminToken());
    if (r.status >= 300) throw new Error(`${method} ${path}: ${r.status} ${r.text}`);
    return r.json;
}

/** A call as a signed-in person (their password sign-in). Returns status and body, never throws. */
export async function asUser(p: Person, method: string, path: string, body?: unknown, collection = "users") {
    const key = `${collection}:${p.email}`;
    const token = tokens.get(key) ?? (await login(collection, p.email, p.password));
    return rawCall(method, path, body, token);
}

export async function ensureUser(world: World, key: string): Promise<Person> {
    const p = world.people[key];
    if (!p) throw new Error(`no person ${key}`);
    if (p.id) return p;
    const made = await rawCall(
        "POST",
        "/api/collections/users/records",
        { email: p.email, password: p.password, passwordConfirm: p.password, verified: true, name: p.name },
        await adminToken(),
    );
    if (made.status < 300) p.id = made.json.id;
    else {
        // already there (made on camera, or by an earlier run): find it
        const found = await api("GET", `/api/collections/users/records?filter=${encodeURIComponent(`email="${p.email}"`)}`);
        p.id = found.items[0].id;
    }
    saveWorld(world);
    return p;
}

/** Points the app at the local sink and sets the public URL. Safe to run again. */
export async function configureInstance(world: World, appName: string): Promise<void> {
    if (world.notes.configured) return;
    await api("PATCH", "/api/settings", {
        meta: { appName, appUrl: world.base, senderName: appName, senderAddress: "no-reply@example.org" },
        smtp: { enabled: true, host: "127.0.0.1", port: world.port + 400, tls: false },
    });
    world.notes.configured = "yes";
    saveWorld(world);
}

/** For a scene run on its own (WALK_ONLY): make the superuser the first-run scene would have made. */
export async function ensureSuperuser(world: World): Promise<void> {
    if (world.notes.superuserMade) return;
    execFileSync(BIN, ["superuser", "upsert", world.admin.email, world.admin.password, "--dir", world.dataDir, "--encryptionEnv", "PB_ENCRYPTION_KEY"], {
        env: serverEnv(world),
        cwd: join(world.dir, "sandbox"),
        stdio: "ignore",
    });
    world.notes.superuserMade = "yes";
    saveWorld(world);
}
