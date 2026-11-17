// The fleet video kit's clip record: <file>.mp4.scan.json = {"passed": true, "sha256": <of the mp4>, "frames": N}.
// Written last, from the file on disk, so the hash is of exactly what ships.
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";

export function writeScanRecord(mp4, frames, extra = {}) {
    if (!(frames > 0)) throw new Error(`${mp4}: no scanned frames, so no scan record`);
    const sha256 = createHash("sha256").update(readFileSync(mp4)).digest("hex");
    writeFileSync(`${mp4}.scan.json`, JSON.stringify({ passed: true, sha256, frames, ...extra }, null, 2) + "\n");
}
