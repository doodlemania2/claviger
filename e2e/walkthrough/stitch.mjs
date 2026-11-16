// Stitch the scene recordings of one run:   node e2e/walkthrough/stitch.mjs <run dir>
// Output in <run dir>/video/ (the files a docs site embeds):
//   NN-<scene>.mp4 + .vtt        one per scene, 1280x720 H.264, a title card first
//   walkthrough-<story>.mp4 + .vtt   all scenes, the .vtt shifted by the title cards
//   manifest.json                story, scene number, title, duration, file names, sizes
//   <file>.mp4.scan.json         beside every mp4: {"passed": true, "sha256", "frames"} (the fleet video kit's clip record)
// Captions come from the on-screen caption log kept while recording (scene.json "cues").
//
// The safety gate: every recording must carry a passing scan (lib/scan.ts wrote it while recording,
// with frames > 0 and no failure), and the scene titles and captions pass the same text rules. One
// miss and nothing is stitched (exit 1).
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { chromium } from "@playwright/test";
import { loadDenyList, scanText } from "../guides/scan.mjs";
import { writeScanRecord } from "./lib/record.mjs";

const runDir = process.argv[2];
if (!runDir) throw new Error("usage: stitch.mjs <run dir>");
const W = 1920; // composed at 1080p, delivered at 720p
const H = 1080;
const OW = 1280;
const OH = 720;
const FPS = 25;
const FFMPEG = process.env.FFMPEG ?? "ffmpeg";
const FFPROBE = process.env.FFPROBE ?? "ffprobe";
const world = JSON.parse(readFileSync(join(runDir, "world.json"), "utf8"));
const outDir = join(runDir, "video");
mkdirSync(outDir, { recursive: true });

function run(bin, args) {
    const r = spawnSync(bin, args, { stdio: ["ignore", "pipe", "inherit"], encoding: "utf8" });
    if (r.status !== 0) throw new Error(`${bin} failed: ${args.join(" ").slice(0, 240)}`);
    return r.stdout;
}
const ffmpeg = (args) => run(FFMPEG, ["-hide_banner", "-loglevel", "error", "-y", ...args]);
const duration = (file) => Number(run(FFPROBE, ["-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file]).trim());

const ENC = ["-c:v", "libx264", "-preset", "veryfast", "-crf", "24", "-pix_fmt", "yuv420p", "-r", String(FPS)];
// the delivered files: 720p, small, streamable
const OUT = ["-c:v", "libx264", "-preset", "medium", "-crf", "29", "-pix_fmt", "yuv420p", "-r", String(FPS), "-movflags", "+faststart", "-an"];
const fit = (w, h) => `scale=${w}:${h}:force_original_aspect_ratio=decrease,pad=${w}:${h}:(ow-iw)/2:(oh-ih)/2:color=0x0f172a,setsar=1,fps=${FPS}`;
const esc = (t) => String(t).replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[c]);

async function titleCards(scenes) {
    const browser = await chromium.launch();
    const page = await browser.newPage({ viewport: { width: W, height: H } });
    for (const s of scenes) {
        await page.setContent(`<body style="margin:0;width:${W}px;height:${H}px;background:linear-gradient(135deg,#0f172a,#1e3a8a);color:#fff;font-family:system-ui,sans-serif;display:flex;flex-direction:column;justify-content:center;align-items:center;text-align:center">
      <div style="font-size:30px;letter-spacing:6px;opacity:.7">SCENE ${s.index}</div>
      <div style="font-size:84px;font-weight:800;margin:24px 80px 18px">${esc(s.title)}</div>
      <div style="font-size:34px;opacity:.85;margin:0 160px">${esc(s.subtitle)}</div>
      <div style="position:absolute;bottom:40px;font-size:24px;opacity:.6">Claviger walkthrough: ${esc(world.story)}</div></body>`);
        await page.screenshot({ path: join(s.dir, "title.png") });
    }
    await browser.close();
}

const pad2 = (n) => String(n).padStart(2, "0");
const stamp = (sec) => {
    const ms = Math.max(0, Math.round(sec * 1000));
    return `${pad2(Math.floor(ms / 3600000))}:${pad2(Math.floor(ms / 60000) % 60)}:${pad2(Math.floor(ms / 1000) % 60)}.${String(ms % 1000).padStart(3, "0")}`;
};
const vtt = (cues) =>
    "WEBVTT\n\n" +
    cues
        .filter((c) => c.end - c.start > 0.2)
        .map((c, i) => `${i + 1}\n${stamp(c.start)} --> ${stamp(c.end)}\n${c.text.replace(/\n+/g, " ")}\n`)
        .join("\n");

function sceneVideo(s) {
    const title = join(s.dir, "title.mp4");
    ffmpeg(["-loop", "1", "-t", "3.5", "-i", join(s.dir, "title.png"), "-vf", `scale=${W}:${H},fps=${FPS}`, ...ENC, title]);
    const titleLen = duration(title);
    const body = join(s.dir, "body.mp4");
    const clips = s.clips.filter((c) => existsSync(c.file));
    const offsets = {}; // seconds into the body at which each actor's own recording starts
    if (clips.length === 0) {
        return { titleLen, file: null, offsets, title };
    }
    if (s.layout === "row" && clips.length > 1) {
        const cell = Math.floor(W / clips.length);
        const inputs = clips.flatMap((c) => ["-i", c.file]);
        const chains = clips.map((_, i) => `[${i}:v]${fit(cell, H)}[v${i}]`).join(";");
        const stack = clips.map((_, i) => `[v${i}]`).join("") + `hstack=inputs=${clips.length}[out]`;
        ffmpeg([...inputs, "-filter_complex", `${chains};${stack}`, "-map", "[out]", ...ENC, body]);
        clips.forEach((c) => (offsets[c.actor] = 0));
    } else {
        let at = 0;
        const parts = clips.map((c, i) => {
            const p = join(s.dir, `part${i}.mp4`);
            ffmpeg(["-i", c.file, "-vf", fit(W, H), ...ENC, p]);
            offsets[c.actor] = at;
            at += duration(p);
            return p;
        });
        concat(parts, body);
    }
    return { titleLen, file: body, offsets, title };
}

function concat(files, out) {
    const list = out + ".txt";
    writeFileSync(list, files.map((f) => `file '${f}'`).join("\n"));
    ffmpeg(["-f", "concat", "-safe", "0", "-i", list, "-c", "copy", out]);
}

const scenes = readdirSync(join(runDir, "scenes"))
    .sort()
    .map((d) => join(runDir, "scenes", d))
    .filter((d) => existsSync(join(d, "scene.json")))
    .map((d) => ({ ...JSON.parse(readFileSync(join(d, "scene.json"), "utf8")), dir: d }));
if (scenes.length === 0) {
    console.log("no scene recordings to stitch");
    process.exit(0);
}

// ---- the safety gate --------------------------------------------------------------------------
const deny = loadDenyList();
const problems = [];
for (const s of scenes) {
    for (const f of s.scanFailed ?? []) problems.push(`${s.id}: ${f}`);
    for (const c of s.clips) {
        if (!c.scan) problems.push(`${s.id}/${c.actor}: the recording has no scan record`);
        else if (!(c.scan.frames > 0)) problems.push(`${s.id}/${c.actor}: no frame was scanned`);
        else if (c.scan.failures.length) problems.push(`${s.id}/${c.actor}: the recording failed its scan`);
    }
    const texts = [s.title, s.subtitle, ...s.cues.map((c) => c.text)];
    for (const h of texts.flatMap((t) => scanText(String(t ?? ""), deny))) problems.push(`${s.id}: title or caption: ${h.rule} "${h.match}"`);
}
if (problems.length) {
    console.error(`stitch: refused, ${problems.length} problem(s):\n` + problems.map((p) => `  ${p}`).join("\n"));
    process.exit(1);
}
const framesOf = (s) => s.clips.reduce((n, c) => n + c.scan.frames, 0);
await titleCards(scenes);

const manifest = { story: world.story, scenes: [] };
const finals = [];
for (const s of scenes) {
    const { titleLen, file, offsets, title } = sceneVideo(s);
    const base = `${pad2(s.index)}-${s.id}`;
    const mp4 = join(outDir, `${base}.mp4`);
    const joined = join(s.dir, "joined.mp4");
    concat(file ? [title, file] : [title], joined);
    ffmpeg(["-i", joined, "-vf", `scale=${OW}:${OH}:flags=lanczos`, ...OUT, mp4]);
    // captions: the title card first, then every caption of every actor, shifted into the scene video
    const multi = new Set(s.cues.map((c) => c.actor)).size > 1;
    const cues = [{ start: 0.2, end: titleLen - 0.2, text: `Scene ${s.index}: ${s.title}. ${s.subtitle}` }];
    for (const c of s.cues) {
        const off = titleLen + (offsets[c.actor] ?? 0);
        cues.push({ start: off + c.start, end: off + c.end, text: multi ? `<v ${c.label.replace(/[<>&]/g, "")}>${c.text}` : c.text });
    }
    cues.sort((a, b) => a.start - b.start);
    writeFileSync(join(outDir, `${base}.vtt`), vtt(cues));
    const dur = duration(mp4);
    writeScanRecord(mp4, framesOf(s));
    finals.push({ mp4, cues, dur, frames: framesOf(s) });
    manifest.scenes.push({
        story: world.story,
        scene: s.index,
        id: s.id,
        title: s.title,
        subtitle: s.subtitle,
        duration: Math.round(dur * 10) / 10,
        bytes: statSync(mp4).size,
        mp4: `${base}.mp4`,
        vtt: `${base}.vtt`,
        // where each actor's recording sits in this mp4 (clip.mjs cuts by the scene's marks with these)
        titleLen,
        offsets,
    });
}

// the stitched film and its captions, shifted by everything before each scene
const name = `walkthrough-${world.story}`;
const list = join(runDir, "scenes", "final.txt");
writeFileSync(list, finals.map((f) => `file '${f.mp4}'`).join("\n"));
ffmpeg(["-f", "concat", "-safe", "0", "-i", list, "-c", "copy", "-movflags", "+faststart", join(outDir, `${name}.mp4`)]);
let at = 0;
const all = [];
for (const f of finals) {
    for (const c of f.cues) all.push({ ...c, start: c.start + at, end: c.end + at });
    at += f.dur;
}
writeFileSync(join(outDir, `${name}.vtt`), vtt(all));
writeScanRecord(join(outDir, `${name}.mp4`), finals.reduce((n, f) => n + f.frames, 0));
manifest.stitched = {
    story: world.story,
    title: world.story === "developer" ? "Claviger for developers" : "Claviger for a parish",
    duration: Math.round(duration(join(outDir, `${name}.mp4`)) * 10) / 10,
    bytes: statSync(join(outDir, `${name}.mp4`)).size,
    mp4: `${name}.mp4`,
    vtt: `${name}.vtt`,
};
manifest.skipped = world.skipped;
writeFileSync(join(outDir, "manifest.json"), JSON.stringify(manifest, null, 2));
console.log(JSON.stringify({ walkthrough: join(outDir, `${name}.mp4`), scenes: finals.length, bytes: manifest.stitched.bytes }));
