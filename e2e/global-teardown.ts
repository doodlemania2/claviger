import { readFileSync, rmSync } from "node:fs";
import { join } from "node:path";

export default async function globalTeardown() {
    try {
        const s = JSON.parse(readFileSync(join(__dirname, ".run", "state.json"), "utf8"));
        process.kill(s.pid, "SIGTERM");
        await new Promise((r) => setTimeout(r, 500));
        if (!process.env.E2E_KEEP) rmSync(s.data, { recursive: true, force: true });
    } catch {}
}
