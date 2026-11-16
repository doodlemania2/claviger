// The safety scan of the guide kit (fleet presence standard, requirement 2). Every picture the kit
// takes is of an invented world, and this proves it: before a shot is written, the visible text of
// the page (and the values of its inputs) is checked, and a shot that shows a real-looking contact
// detail, a production or internal hostname, or anything on the deny list is not written.
//
// Rules (a match fails the shot):
//   email     an address whose domain is not example.org, example.com or example.net (or a subdomain)
//   phone     a phone number that is not in 555-0100 to 555-0199
//   hostname  a production or internal name (HOSTNAMES below)
//   deny      a substring from the deny list (deny.json next to screens.json), case-insensitive
//   error     an error state: error wording in the page's prose ("Something went wrong", "Try again",
//             "unexpected error", "failed to load"; code samples are not prose) or a visible error
//             panel, empty or not. A screen that shows an error on purpose (an expired link page)
//             says so with "showsError": "<why>" in screens.json, and this rule is skipped for it.
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
export const DEFAULT_DENY_FILE = join(here, "deny.json");

const SAFE_EMAIL_DOMAINS = ["example.org", "example.com", "example.net"];

export const HOSTNAMES = [
    /\b(?:[a-z0-9-]+\.)*claviger\.app\b/gi,
    /\b(?:[a-z0-9-]+\.)*thedoodleproject\.com\b/gi,
    /stfoa/gi,
    /doodlessd/gi,
    /doodlemanage/gi,
    /\b(?:[a-z0-9-]+\.)+ts\.net\b/gi,
    /tailscale/gi,
    /derekmartin/gi,
];

const EMAIL = /[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+)/g;

// A number written the way phone numbers are: an optional +country code and area code, then
// groups that end in 3 + 4 digits, or +E.164 written without separators. A bare run of digits
// (a timestamp, a count) is not a phone number, and neither is a date (2026-03-14).
const PHONE = [
    /(?<![\w./:-])(?:\+\d{1,3}[ .-]?)?(?:\(\d{1,4}\)[ .-]?)?(?:\d{1,4}[ .-]){0,2}\d{3}[ .-]\d{4}(?![\w/:]|[.-]\d)/g,
    /(?<![\w./:-])\+\d{8,15}\b/g,
];

const isSafeEmailDomain = (domain) => {
    const d = domain.toLowerCase().replace(/\.+$/, "");
    return SAFE_EMAIL_DOMAINS.some((s) => d === s || d.endsWith("." + s));
};

/** 555-0100 to 555-0199, whatever the country code, area code or separators. */
const isSafePhone = (text) => /55501\d\d$/.test(text.replace(/\D/g, ""));

/** Reads {"substrings": [...]}. A missing or malformed file is an error: the scan never runs without its list. */
export function loadDenyList(file = DEFAULT_DENY_FILE) {
    const raw = JSON.parse(readFileSync(file, "utf8"));
    if (!raw || !Array.isArray(raw.substrings) || raw.substrings.some((s) => typeof s !== "string" || !s.trim())) {
        throw new Error(`${file}: expected {"substrings": [non-empty strings]}`);
    }
    return raw.substrings;
}

/** Every rule the text breaks, as [{rule, match}], each (rule, match) once. */
export function scanText(text, deny = []) {
    const hits = new Map();
    const add = (rule, match) => hits.set(`${rule}\u0000${match}`, { rule, match: match.slice(0, 200) });
    for (const m of text.matchAll(EMAIL)) if (!isSafeEmailDomain(m[1])) add("email", m[0]);
    for (const re of PHONE) for (const m of text.matchAll(re)) if (!isSafePhone(m[0])) add("phone", m[0].trim());
    for (const re of HOSTNAMES) for (const m of text.matchAll(re)) add("hostname", m[0]);
    const lower = text.toLowerCase();
    for (const s of deny) {
        const i = lower.indexOf(s.toLowerCase());
        if (i >= 0) add("deny", text.slice(i, i + s.length));
    }
    return [...hits.values()];
}

/** The visible text of a page plus the values of its inputs (never a password field). */
export async function pageText(page) {
    return page.evaluate(() => {
        const parts = [document.body?.innerText ?? ""];
        for (const el of document.querySelectorAll("input, textarea, select")) {
            if (el.type === "password" || el.type === "hidden") continue;
            if (el.value) parts.push(el.value);
        }
        return parts.join("\n");
    });
}

const ERROR_WORDS = [/something went wrong/gi, /\btry again\b/gi, /unexpected error/gi, /failed to load/gi];

/** The error-state rule over a page view from errorView(). */
export function scanErrorState(view) {
    const hits = new Map();
    for (const re of ERROR_WORDS) for (const m of view.prose.matchAll(re)) hits.set(m[0].toLowerCase(), { rule: "error", match: m[0] });
    for (const t of view.panels) {
        const match = t ? t.slice(0, 200) : "(empty error panel)";
        hits.set("panel\u0000" + match, { rule: "error", match });
    }
    return [...hits.values()];
}

/** The page's prose (visible text without code samples) and the text of every visible error panel. */
export async function errorView(page) {
    return page.evaluate(() => {
        const visible = (el) => !el.closest("[hidden]") && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== "hidden";
        const panels = [...document.querySelectorAll('[role="alert"], .msg.err, .alert.danger, .toast.error')]
            .filter(visible)
            .map((el) => (el.innerText ?? "").trim());
        const clone = document.body.cloneNode(true);
        for (const el of clone.querySelectorAll("pre, code, script, style, [hidden]")) el.remove();
        // a detached clone has no layout, so textContent stands in for innerText
        return { prose: clone.textContent ?? "", panels };
    });
}

/** Thrown by a shot that fails the scan; the shot is not written. */
export class ScanError extends Error {
    constructor(hits) {
        super("safety scan: " + hits.map((h) => `${h.rule} "${h.match}"`).join(", "));
        this.hits = hits;
    }
}
