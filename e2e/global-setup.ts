import { execFileSync, spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { randomBytes } from "node:crypto";

const root = resolve(__dirname, "..");
const bin = join(__dirname, ".bin", "claviger");
const runDir = join(__dirname, ".run");

async function waitFor(url: string, ms = 60_000) {
    const end = Date.now() + ms;
    while (Date.now() < end) {
        try {
            const res = await fetch(url);
            if (res.ok) return;
        } catch {}
        await new Promise((r) => setTimeout(r, 200));
    }
    throw new Error(`timed out waiting for ${url}`);
}

export default async function globalSetup() {
    const port = Number(process.env.E2E_PORT || 18090);
    const base = `http://127.0.0.1:${port}`;

    mkdirSync(join(__dirname, ".bin"), { recursive: true });
    rmSync(runDir, { recursive: true, force: true });
    mkdirSync(runDir, { recursive: true });

    if (!process.env.E2E_SKIP_BUILD || !existsSync(bin)) {
        console.log("e2e: building claviger");
        execFileSync("go", ["build", "-o", bin, "./examples/base"], { cwd: root, stdio: "inherit" });
    }

    // A private parent directory: the binary writes automigrate files to
    // <dir>/../pb_migrations and would otherwise share them with every other run.
    const sandbox = mkdtempSync(join(tmpdir(), "claviger-e2e-"));
    const data = join(sandbox, "pb_data");
    // 32 chars: enables the settings encryption, the OIDC provider and field encryption
    const env = { ...process.env, PB_ENCRYPTION_KEY: randomBytes(24).toString("base64").slice(0, 32) };
    const admin = { email: "admin@example.com", password: randomBytes(12).toString("hex") };

    execFileSync(bin, ["superuser", "upsert", admin.email, admin.password, "--dir", data, "--encryptionEnv", "PB_ENCRYPTION_KEY"], { env, cwd: sandbox, stdio: "inherit" });

    // The settings allow plain http only for localhost; the application URL is
    // the OIDC issuer base, so it has to match what the browser uses.
    const logFile = join(runDir, "server.log");
    const out = require("node:fs").openSync(logFile, "a");
    const child = spawn(
        bin,
        ["serve", "--http", `127.0.0.1:${port}`, "--dir", data, "--encryptionEnv", "PB_ENCRYPTION_KEY"],
        { env, cwd: sandbox, stdio: ["ignore", out, out], detached: false },
    );
    child.on("exit", (code) => {
        if (code) console.error(`e2e: claviger exited with ${code}, see ${logFile}`);
    });

    await waitFor(`${base}/api/health`);

    const res = await fetch(`${base}/api/collections/_superusers/auth-with-password`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ identity: admin.email, password: admin.password }),
    });
    if (!res.ok) throw new Error(`superuser sign-in failed: ${res.status} ${await res.text()}`);
    const { token } = (await res.json()) as { token: string };

    const state = { base, port, pid: child.pid, data, sandbox, admin, token, log: logFile };
    writeFileSync(join(runDir, "state.json"), JSON.stringify(state));

    const api = async (method: string, path: string, body?: unknown) => {
        const r = await fetch(base + path, {
            method,
            headers: { Authorization: token, "Content-Type": "application/json" },
            body: body === undefined ? undefined : JSON.stringify(body),
        });
        if (!r.ok) throw new Error(`${method} ${path}: ${r.status} ${await r.text()}`);
        return r.json().catch(() => ({}));
    };

    await api("PATCH", "/api/settings", { meta: { appUrl: base } });

    // The shared end-user fixture used by the hosted pages tests.
    const user = { email: "alice@example.com", password: randomBytes(12).toString("hex") };
    await api("POST", "/api/collections/users/records", {
        email: user.email,
        password: user.password,
        passwordConfirm: user.password,
        verified: true,
        name: "Alice Example",
    });
    writeFileSync(join(runDir, "state.json"), JSON.stringify({ ...state, user }));

    process.env.E2E_STATE = join(runDir, "state.json");
}
