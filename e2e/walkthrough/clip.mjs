// Cut a short clip out of a stitched scene, between two marks the scene set while recording:
//
//   node e2e/walkthrough/clip.mjs <run dir> <scene id> <from mark> <to mark> [--name <file>] [--pad <s>]
//   e.g. node e2e/walkthrough/clip.mjs e2e/walkthrough/.out/<run> passkey-signin signin-start signin-done
//
// It writes <run dir>/video/clips/<file>.mp4 (default: <scene id>) and, beside it, the fleet video
// kit's record <file>.mp4.scan.json = {"passed": true, "sha256", "frames"}, where frames counts the
// scanned frames inside the cut. A cut with no scanned frame, or from a scene whose recording did not
// pass its scan, is refused. Run it after stitch.mjs (it reads video/manifest.json).
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { writeScanRecord } from "./lib/record.mjs";

const args = process.argv.slice(2);
const opt = (k, d) => {
    const i = args.indexOf(k);
    if (i < 0) return d;
    const v = args[i + 1];
    args.splice(i, 2);
    return v;
};
const name = opt("--name", undefined);
const pad = Number(opt("--pad", "0.4"));
const [runDir, sceneId, fromName, toName] = args;
if (!runDir || !sceneId || !fromName || !toName) {
    console.error("usage: clip.mjs <run dir> <scene id> <from mark> <to mark> [--name <file>] [--pad <s>]");
    process.exit(2);
}
const FFMPEG = process.env.FFMPEG ?? "ffmpeg";
const FFPROBE = process.env.FFPROBE ?? "ffprobe";
const fail = (msg) => {
    console.error(`clip: ${msg}`);
    process.exit(1);
};
const run = (bin, a) => {
    const r = spawnSync(bin, a, { stdio: ["ignore", "pipe", "inherit"], encoding: "utf8" });
    if (r.status !== 0) fail(`${bin} failed`);
    return r.stdout;
};

const manifest = JSON.parse(readFileSync(join(runDir, "video", "manifest.json"), "utf8"));
const entry = manifest.scenes.find((s) => s.id === sceneId);
if (!entry) fail(`no stitched scene "${sceneId}" in ${runDir}/video/manifest.json`);
const sceneDir = readdirSync(join(runDir, "scenes")).find((d) => d.endsWith(`-${sceneId}`));
const scene = JSON.parse(readFileSync(join(runDir, "scenes", sceneDir, "scene.json"), "utf8"));
if ((scene.scanFailed ?? []).length) fail(`scene ${sceneId} failed its safety scan`);

const markOf = (n) => scene.marks?.find((m) => m.name === n) ?? fail(`scene ${sceneId} has no mark "${n}" (marks: ${(scene.marks ?? []).map((m) => m.name).join(", ") || "none"})`);
const from = markOf(fromName);
const to = markOf(toName);
if (from.actor !== to.actor) fail("both marks must belong to the same actor");
if (!(to.t > from.t)) fail(`"${toName}" is not after "${fromName}"`);
const clip = scene.clips.find((c) => c.actor === from.actor);
if (!clip?.scan || clip.scan.failures.length) fail(`the recording of ${from.actor} has no passing scan`);

// the actor's recording starts after the title card, at its offset in the scene body
const base = entry.titleLen + (entry.offsets?.[from.actor] ?? 0);
const t0 = Math.max(0, from.t - pad);
const t1 = to.t + pad;
const frames = clip.scan.samples.filter((t) => t >= t0 && t <= t1).length;
if (frames === 0) fail("no scanned frame falls inside the cut");

const src = join(runDir, "video", entry.mp4);
if (!existsSync(src)) fail(`${src} is missing`);
const outDir = join(runDir, "video", "clips");
mkdirSync(outDir, { recursive: true });
const out = join(outDir, `${name ?? sceneId}.mp4`);
run(FFMPEG, ["-hide_banner", "-loglevel", "error", "-y", "-ss", (base + t0).toFixed(3), "-to", (base + t1).toFixed(3), "-i", src,
    "-c:v", "libx264", "-preset", "medium", "-crf", "26", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-an", out]);
const dur = Number(run(FFPROBE, ["-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", out]).trim());
writeScanRecord(out, frames);
console.log(JSON.stringify({ clip: out, seconds: Math.round(dur * 10) / 10, frames }));
