/**
 * The safety scan of a recording (fleet presence standard, requirements 2 and 5). It runs the same
 * rules as the screenshot kit (e2e/guides/scan.mjs: emails off the example.* domains, phone numbers
 * outside 555-0100..0199, production or internal hostnames, the deny list) over the page text while
 * an actor records, the way MassCast's watchPageSafe does:
 *
 *   - on every DOM change (at most every 100 ms; text added and removed inside that gap, a toast, is
 *     still seen, because each added node's text is kept until the next report),
 *   - on a fixed sample every 500 ms,
 *   - and once more when the recording stops.
 *
 * Each report is one scanned frame, stamped in seconds from the start of the actor's recording, so a
 * cut of the clip can say how many scanned frames it holds. It reads the whole document, not only the
 * body, so the captions and cards of the overlay are scanned too. Like the screenshot scan it reads
 * text, not pixels.
 */
import { join } from "node:path";
import type { BrowserContext, Page } from "@playwright/test";

export interface ScanFailure {
    t: number;
    rule: string;
    match: string;
}

export interface ClipScan {
    /** Scanned frames. */
    frames: number;
    /** When each frame was scanned, in seconds from the start of the recording. */
    samples: number[];
    failures: ScanFailure[];
}

type ScanModule = {
    scanText: (text: string, deny: string[], opts?: { typing?: boolean }) => { rule: string; match: string }[];
    loadDenyList: () => string[];
};

let mod: Promise<ScanModule> | undefined;
// scan.mjs is ESM and this file is compiled to CommonJS by Playwright, so it is loaded dynamically
const scanModule = (): Promise<ScanModule> => (mod ??= import(join(__dirname, "..", "..", "guides", "scan.mjs")) as Promise<ScanModule>);

/** In the page: report the document text after DOM changes, at most every `gapMs`. Sent whole, so self-contained. */
function installObserver(gapMs: number): void {
    const w = window as unknown as { __walkScanInstalled?: boolean; __walkScan?: (t: string) => void };
    if (w.__walkScanInstalled) return;
    w.__walkScanInstalled = true;
    let added: string[] = [];
    let timer: ReturnType<typeof setTimeout> | null = null;
    const text = (): string => {
        const parts = [document.documentElement?.innerText ?? ""];
        for (const el of Array.from(document.querySelectorAll("input, textarea, select")) as HTMLInputElement[]) {
            if (el.type !== "password" && el.type !== "hidden" && el.value) parts.push(el.value);
        }
        return parts.join("\n");
    };
    const send = (): void => {
        timer = null;
        const extra = added.join("\n");
        added = [];
        w.__walkScan?.(`${text()}\n${extra}`);
    };
    const start = (): void => {
        new MutationObserver((records) => {
            for (const r of records) {
                if (r.type === "characterData") added.push((r.target as CharacterData).data ?? "");
                // each text node on its own line: textContent would glue "Auto" and "tomas@example.org" together
                r.addedNodes.forEach((n) => {
                    if (n.nodeType === Node.TEXT_NODE) return void added.push(n.textContent ?? "");
                    const walker = document.createTreeWalker(n, NodeFilter.SHOW_TEXT);
                    while (walker.nextNode()) added.push(walker.currentNode.textContent ?? "");
                });
            }
            if (timer === null) timer = setTimeout(send, gapMs);
        }).observe(document.documentElement, { subtree: true, childList: true, characterData: true });
        send();
    };
    if (document.documentElement) start();
    else document.addEventListener("DOMContentLoaded", start, { once: true });
}

/** The document text and input values, for the fixed samples. */
const pageText = (page: Page): Promise<string> =>
    page.evaluate(() => {
        const parts = [document.documentElement?.innerText ?? ""];
        for (const el of Array.from(document.querySelectorAll("input, textarea, select")) as HTMLInputElement[]) {
            if (el.type !== "password" && el.type !== "hidden" && el.value) parts.push(el.value);
        }
        return parts.join("\n");
    });

/** Starts scanning an actor's pages. Call before the first page opens; `attach` each page. */
export async function watchContext(ctx: BrowserContext, t0: () => number, { sampleMs = 500, gapMs = 100 } = {}) {
    const { scanText, loadDenyList } = await scanModule();
    const deny = loadDenyList();
    const result: ClipScan = { frames: 0, samples: [], failures: [] };
    const seen = new Set<string>();
    let stopped = false;
    const scan = (text: string): void => {
        if (stopped) return;
        const t = Math.round(((Date.now() - t0()) / 1000) * 1000) / 1000;
        result.frames++;
        result.samples.push(t);
        for (const h of scanText(text, deny, { typing: true })) {
            const key = `${h.rule}\u0000${h.match}`;
            if (seen.has(key)) continue;
            seen.add(key);
            result.failures.push({ t, ...h });
        }
    };
    await ctx.exposeBinding("__walkScan", (_source, text: unknown) => scan(String(text ?? "")));
    await ctx.addInitScript(installObserver, gapMs);
    const pages: Page[] = [];
    const sample = (): Promise<void> =>
        Promise.all(pages.filter((p) => !p.isClosed()).map((p) => pageText(p).then(scan, () => undefined))).then(() => undefined);
    const timer = setInterval(() => void sample(), sampleMs);
    return {
        result,
        attach(page: Page): void {
            pages.push(page);
        },
        /** The last sample, then no more. Call before the context closes. */
        async stop(): Promise<ClipScan> {
            clearInterval(timer);
            await sample();
            stopped = true;
            return result;
        },
    };
}
