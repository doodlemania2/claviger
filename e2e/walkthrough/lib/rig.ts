// The small local servers of the rig, all on localhost and never reachable from outside:
//   - a mailbox viewer, so an email-link step can be shown (the app's mail goes to the SMTP sink)
//   - a sample "relying party" web app that signs in with Claviger (OIDC authorization code + PKCE)
//   - a webhook receiver that checks the Standard Webhooks signature
import { createHash, createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { Mail } from "../../smtp";

export interface ParsedMail {
    index: number;
    from: string;
    to: string[];
    subject: string;
    html: string;
    text: string;
}

function decodeBody(body: string, cte: string): string {
    const enc = cte.trim().toLowerCase();
    if (enc === "base64") return Buffer.from(body.replace(/\s+/g, ""), "base64").toString("utf8");
    if (enc === "quoted-printable") {
        const bytes: number[] = [];
        const s = body.replace(/=\r?\n/g, "");
        for (let i = 0; i < s.length; i++) {
            if (s[i] === "=" && /^[0-9A-Fa-f]{2}$/.test(s.slice(i + 1, i + 3))) {
                bytes.push(parseInt(s.slice(i + 1, i + 3), 16));
                i += 2;
            } else bytes.push(...Buffer.from(s[i], "utf8"));
        }
        return Buffer.from(bytes).toString("utf8");
    }
    return body;
}

function headerMap(block: string): Record<string, string> {
    const out: Record<string, string> = {};
    const unfolded = block.replace(/\r?\n[ \t]+/g, " ");
    for (const line of unfolded.split(/\r?\n/)) {
        const i = line.indexOf(":");
        if (i > 0) out[line.slice(0, i).trim().toLowerCase()] = line.slice(i + 1).trim();
    }
    return out;
}

function encodedWords(s: string): string {
    return s.replace(/=\?([^?]+)\?([bBqQ])\?([^?]*)\?=/g, (_m, _cs, kind: string, text: string) =>
        kind.toLowerCase() === "b" ? Buffer.from(text, "base64").toString("utf8") : decodeBody(text.replace(/_/g, " "), "quoted-printable"),
    );
}

function parts(raw: string): { html: string; text: string } {
    const split = raw.search(/\r?\n\r?\n/);
    const head = headerMap(raw.slice(0, split));
    const body = raw.slice(split).replace(/^\r?\n\r?\n/, "");
    const ct = head["content-type"] || "text/plain";
    const boundary = /boundary="?([^";]+)"?/i.exec(ct)?.[1];
    if (boundary && /multipart/i.test(ct)) {
        let html = "";
        let text = "";
        for (const seg of body.split("--" + boundary)) {
            if (!/\S/.test(seg) || seg.startsWith("--")) continue;
            const r = parts(seg.replace(/^\r?\n/, ""));
            html ||= r.html;
            text ||= r.text;
        }
        return { html, text };
    }
    const decoded = decodeBody(body, head["content-transfer-encoding"] || "");
    return /text\/html/i.test(ct) ? { html: decoded, text: "" } : { html: "", text: decoded };
}

export function parseMail(m: Mail, index: number): ParsedMail {
    const split = m.raw.search(/\r?\n\r?\n/);
    const head = headerMap(m.raw.slice(0, split));
    const p = parts(m.raw);
    return { index, from: m.from, to: m.to, subject: encodedWords(head["subject"] || ""), html: p.html, text: p.text };
}

const esc = (s: string) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c] as string);

const PAGE = (title: string, body: string, refresh = false) => `<!doctype html><html><head><meta charset="utf-8">${refresh ? '<meta http-equiv="refresh" content="2">' : ""}<title>${esc(title)}</title></head>
<body style="margin:0;font-family:system-ui,sans-serif;background:#f1f5f9;color:#0f172a">${body}</body></html>`;

export interface Delivery {
    id: string;
    at: string;
    type: string;
    valid: boolean;
    body: string;
}

export interface RigOptions {
    port: number;
    base: string;
    mails: () => Mail[];
}

export class Rig {
    readonly url: string;
    readonly server: Server;
    deliveries: Delivery[] = [];
    hookSecrets: string[] = [];
    oidc = { clientId: "", clientSecret: "", scope: "openid profile email" };
    /** Who the sample app thinks is signed in, by the session cookie. */
    private sessions = new Map<string, { claims: Record<string, unknown> }>();
    private pending = new Map<string, { verifier: string }>();

    constructor(private readonly o: RigOptions) {
        this.url = `http://localhost:${o.port}`;
        this.server = createServer((req, res) => {
            this.route(req, res).catch((e) => {
                res.writeHead(500, { "Content-Type": "text/plain" }).end(String(e));
            });
        });
    }

    listen(): Promise<void> {
        return new Promise((ok) => this.server.listen(this.o.port, "127.0.0.1", ok));
    }

    close(): Promise<void> {
        this.server.closeAllConnections?.();
        return new Promise((ok) => this.server.close(() => ok()));
    }

    redirectUri(): string {
        return `${this.url}/callback`;
    }

    private async readBody(req: IncomingMessage): Promise<string> {
        const chunks: Buffer[] = [];
        for await (const c of req) chunks.push(c as Buffer);
        return Buffer.concat(chunks).toString("utf8");
    }

    private cookie(req: IncomingMessage): string {
        return /(?:^|; )rp=([^;]+)/.exec(req.headers.cookie || "")?.[1] || "";
    }

    private async route(req: IncomingMessage, res: ServerResponse): Promise<void> {
        const u = new URL(req.url || "/", this.url);
        const html = (status: number, body: string) => res.writeHead(status, { "Content-Type": "text/html; charset=utf-8" }).end(body);
        if (u.pathname === "/mail") return html(200, this.mailPage(u.searchParams.get("to") || "", u.searchParams.get("n")));
        if (u.pathname === "/hook" && req.method === "POST") {
            const body = await this.readBody(req);
            const valid = this.verify(req, body);
            let type = "?";
            try {
                type = JSON.parse(body).type;
            } catch {
                /* not JSON */
            }
            this.deliveries.unshift({ id: String(req.headers["webhook-id"] || ""), at: new Date().toLocaleTimeString(), type, valid, body });
            return void res.writeHead(valid ? 200 : 401).end(valid ? "ok" : "bad signature");
        }
        if (u.pathname === "/blank") return html(200, PAGE("Terminal", `<div style="height:100vh;background:#0b1220"></div>`));
        if (u.pathname === "/received") return html(200, this.receivedPage());
        if (u.pathname === "/") return html(200, this.appHome(req));
        if (u.pathname === "/login") {
            const verifier = randomBytes(32).toString("base64url");
            const state = randomBytes(8).toString("hex");
            this.pending.set(state, { verifier });
            const a = new URL(this.o.base + "/oidc/authorize");
            a.searchParams.set("client_id", this.oidc.clientId);
            a.searchParams.set("redirect_uri", this.redirectUri());
            a.searchParams.set("response_type", "code");
            a.searchParams.set("scope", this.oidc.scope);
            a.searchParams.set("state", state);
            a.searchParams.set("code_challenge", createHash("sha256").update(verifier).digest("base64url"));
            a.searchParams.set("code_challenge_method", "S256");
            const prompt = u.searchParams.get("prompt");
            if (prompt) a.searchParams.set("prompt", prompt);
            return void res.writeHead(302, { Location: a.toString() }).end();
        }
        if (u.pathname === "/callback") {
            const err = u.searchParams.get("error");
            if (err) return html(200, PAGE("Sample app", this.appShell(`<h2>Sign-in was not completed</h2><p>Claviger answered <code>${esc(err)}</code>.</p><p><a href="/">Back</a></p>`)));
            const st = this.pending.get(u.searchParams.get("state") || "");
            if (!st) return html(400, PAGE("Sample app", this.appShell("<h2>Unknown sign-in</h2>")));
            const tok = await fetch(this.o.base + "/oidc/oauth/token", {
                method: "POST",
                headers: {
                    "Content-Type": "application/x-www-form-urlencoded",
                    Authorization: "Basic " + Buffer.from(`${this.oidc.clientId}:${this.oidc.clientSecret}`).toString("base64"),
                },
                body: new URLSearchParams({
                    grant_type: "authorization_code",
                    code: u.searchParams.get("code") || "",
                    redirect_uri: this.redirectUri(),
                    code_verifier: st.verifier,
                }),
            });
            const body = (await tok.json()) as Record<string, string>;
            if (!tok.ok || !body.id_token) return html(502, PAGE("Sample app", this.appShell(`<h2>Token exchange failed</h2><pre>${esc(JSON.stringify(body))}</pre>`)));
            const claims = JSON.parse(Buffer.from(body.id_token.split(".")[1], "base64url").toString());
            const sid = randomBytes(12).toString("hex");
            this.sessions.set(sid, { claims });
            return void res.writeHead(302, { Location: "/", "Set-Cookie": `rp=${sid}; Path=/; HttpOnly` }).end();
        }
        if (u.pathname === "/logout") {
            this.sessions.delete(this.cookie(req));
            return void res.writeHead(302, { Location: "/", "Set-Cookie": "rp=; Path=/; Max-Age=0" }).end();
        }
        res.writeHead(404).end("not found");
    }

    private verify(req: IncomingMessage, body: string): boolean {
        const id = String(req.headers["webhook-id"] || "");
        const ts = String(req.headers["webhook-timestamp"] || "");
        const sigs = String(req.headers["webhook-signature"] || "").split(" ");
        for (const secret of this.hookSecrets) {
            const key = Buffer.from(secret.replace(/^whsec_/, ""), "base64");
            const want = "v1," + createHmac("sha256", key).update(`${id}.${ts}.${body}`).digest("base64");
            for (const s of sigs) {
                if (s.length === want.length && timingSafeEqual(Buffer.from(s), Buffer.from(want))) return true;
            }
        }
        return false;
    }

    private appShell(inner: string): string {
        return `<header style="background:#1e3a8a;color:#fff;padding:16px 28px;font-size:22px;font-weight:700">Alder Notes <span style="font-weight:400;font-size:14px;opacity:.8">a sample app that uses Claviger to sign people in</span></header>
<main style="max-width:760px;margin:36px auto;background:#fff;padding:28px 34px;border-radius:12px;box-shadow:0 2px 12px rgba(0,0,0,.08);font-size:18px;line-height:1.5">${inner}</main>`;
    }

    private appHome(req: IncomingMessage): string {
        const s = this.sessions.get(this.cookie(req));
        const btn = "display:inline-block;background:#1d4ed8;color:#fff;padding:10px 20px;border-radius:8px;text-decoration:none;font-weight:600;margin-right:10px";
        if (!s) {
            return PAGE("Alder Notes", this.appShell(`<h2>Welcome to Alder Notes</h2><p>You are not signed in.</p><p><a style="${btn}" href="/login">Sign in with Claviger</a></p>`));
        }
        const c = s.claims;
        const rows = ["sub", "name", "email", "iss"].filter((k) => c[k]).map((k) => `<tr><td style="padding:4px 14px 4px 0;color:#475569">${k}</td><td><code>${esc(String(c[k]))}</code></td></tr>`);
        return PAGE(
            "Alder Notes",
            this.appShell(`<h2>You are signed in</h2><p>Hello, <b>${esc(String(c.name || c.email || c.sub))}</b>. Alder Notes never saw a password: Claviger signed you in and sent these claims.</p><table>${rows.join("")}</table><p style="margin-top:22px"><a style="${btn}" href="/login?prompt=select_account">Switch account</a><a style="${btn};background:#475569" href="/logout">Sign out of the app</a></p>`),
        );
    }

    private receivedPage(): string {
        const rows = this.deliveries
            .map(
                (d) =>
                    `<div style="background:#fff;margin:12px 0;padding:12px 16px;border-radius:10px;border-left:6px solid ${d.valid ? "#16a34a" : "#dc2626"}"><div style="font-weight:700">${esc(d.type)} <span style="font-weight:400;color:#475569">at ${esc(d.at)}</span></div><div style="color:${d.valid ? "#15803d" : "#b91c1c"};font-weight:600">${d.valid ? "Signature verified with the signing secret" : "Signature did NOT verify"}</div><pre style="margin:6px 0 0;white-space:pre-wrap;word-break:break-all;font-size:13px">${esc(prettyJson(d.body)).slice(0, 700)}</pre></div>`,
            )
            .join("");
        return PAGE(
            "Webhook receiver",
            `<header style="background:#14532d;color:#fff;padding:14px 26px;font-size:20px;font-weight:700">CRM receiver <span style="font-weight:400;font-size:14px;opacity:.85">POST ${esc(this.url)}/hook</span></header><main style="max-width:860px;margin:18px auto;padding:0 16px">${rows || '<p style="color:#475569">No deliveries yet.</p>'}</main>`,
            true,
        );
    }

    private mailPage(to: string, n: string | null): string {
        const all = this.o.mails().map(parseMail);
        const mine = all.filter((m) => !to || m.to.includes(to));
        const bar = `<header style="background:#334155;color:#fff;padding:12px 26px;font-size:18px;font-weight:700">Mailbox of ${esc(to || "everyone")} <span style="font-weight:400;font-size:13px;opacity:.8">(a local capture; no mail leaves this machine)</span></header>`;
        const pick = n ? all.find((m) => m.index === Number(n)) : mine[mine.length - 1];
        if (!pick) return PAGE("Mailbox", bar + '<main style="padding:24px">No mail yet.</main>');
        const body = pick.html
            ? pick.html.replace(/<script[\s\S]*?<\/script>/gi, "")
            : `<pre style="white-space:pre-wrap;font:16px/1.5 system-ui">${esc(pick.text).replace(/(https?:\/\/[^\s<]+)/g, '<a href="$1">$1</a>')}</pre>`;
        const list = mine
            .slice()
            .reverse()
            .map((m) => `<div style="padding:6px 0"><a href="/mail?to=${encodeURIComponent(to)}&n=${m.index}">${esc(m.subject)}</a></div>`)
            .join("");
        return PAGE(
            "Mailbox",
            `${bar}<main style="display:flex;gap:24px;padding:18px 26px"><aside style="width:230px;font-size:14px"><b>Inbox</b>${list}</aside><section style="flex:1;background:#fff;border-radius:10px;padding:18px 24px"><div style="color:#475569;font-size:14px">From ${esc(pick.from)}<br>To ${esc(pick.to.join(", "))}</div><h2 style="margin:8px 0 14px">${esc(pick.subject)}</h2>${body}</section></main>`,
        );
    }
}

function prettyJson(s: string): string {
    try {
        return JSON.stringify(JSON.parse(s), null, 2);
    } catch {
        return s;
    }
}
