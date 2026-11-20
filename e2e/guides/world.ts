// The demo world of the screenshot kit (e2e/guides/README.md).
//
// It starts the real binary the way e2e/global-setup.ts does (a temp data dir, PB_ENCRYPTION_KEY,
// and a KEK so encrypted fields work), then seeds an INVENTED congregation through the API:
// every name is made up and every address is on example.org or example.com.
//
// Node runs this file as TypeScript by type stripping (Node 22.18+), so it uses erasable syntax
// only and imports its siblings with the .ts extension. Run it alone to explore the world:
//
//     node --disable-warning=ExperimentalWarning e2e/guides/world.ts --keep
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { chromium, type Browser, type BrowserContext, type Page } from "@playwright/test";
import { createServer } from "node:net";
import { startSmtp, type Mail } from "../smtp.ts";

const here = dirname(fileURLToPath(import.meta.url));
export const repoRoot = resolve(here, "..", "..");
const defaultBin = join(repoRoot, "e2e", ".bin", "claviger");

/** Every demo account uses this password. It protects nothing: the data dir is thrown away. */
export const DEMO_PASSWORD = "demo-pass-1234";
/** 32 bytes, base64: the key-encryption key of the demo instance. */
export const DEMO_KEK = Buffer.from("claviger-guide-demo-kek-00000000").toString("base64");
const DEMO_ENCRYPTION_KEY = "claviger-guide-demo-settings-key"; // exactly 32 characters

export type Persona = {
    key: string;
    name: string;
    email: string;
    password: string;
    id: string;
    /** the auth collection the persona signs in to */
    collection: string;
};

export type Passkey = {
    /** a token that carries a passkey assertion made a moment ago */
    fresh: () => Promise<string>;
    /** the credentials of the virtual authenticator, to import into other contexts */
    credentials: () => Promise<any[]>;
};

export type World = {
    base: string;
    port: number;
    dataDir: string;
    sandbox: string;
    superuser: Persona & { token: string };
    personas: Record<string, Persona>;
    /** facts about the seeded data, for route placeholders: {orgSlug}, {inviteToken}, ... */
    vars: Record<string, string>;
    /** placeholders that need a fresh value on every use (an OIDC authorization request, a device code) */
    lazy: Record<string, () => Promise<string>>;
    api: (method: string, path: string, body?: unknown, token?: string) => Promise<any>;
    /** a token for a persona (password sign-in) */
    tokenFor: (key: string) => Promise<string>;
    passkeys: Record<string, Passkey>;
    mails: Mail[];
    stop: () => Promise<void>;
};

async function freePort(): Promise<number> {
    return new Promise((ok, fail) => {
        const srv = createServer();
        srv.on("error", fail);
        srv.listen(0, "127.0.0.1", () => {
            const { port } = srv.address() as { port: number };
            srv.close(() => ok(port));
        });
    });
}

function log(msg: string) {
    if (!process.env.GUIDES_QUIET && !process.env.GUIDES_FORCE_QUIET) console.log(`world: ${msg}`);
}

async function waitFor(url: string, ms = 60_000) {
    const end = Date.now() + ms;
    while (Date.now() < end) {
        try {
            if ((await fetch(url)).ok) return;
        } catch {}
        await new Promise((r) => setTimeout(r, 150));
    }
    throw new Error(`timed out waiting for ${url}`);
}

export type StartOptions = {
    port?: number;
    bin?: string;
    /** an already launched browser, so the world and the capture share one */
    browser?: Browser;
    quiet?: boolean;
};

export async function startWorld(opts: StartOptions = {}): Promise<World> {
    if (opts.quiet) process.env.GUIDES_FORCE_QUIET = "1";
    // free ports, so a run never meets another session's instance (the e2e suite uses 18090)
    const port = opts.port ?? (process.env.CLAVIGER_GUIDES_PORT ? Number(process.env.CLAVIGER_GUIDES_PORT) : await freePort());
    const smtpPort = await freePort();
    // "localhost", not 127.0.0.1: WebAuthn relying party ids must be domain names.
    const base = `http://localhost:${port}`;
    const bin = opts.bin ?? process.env.CLAVIGER_GUIDES_BIN ?? defaultBin;

    // Build every run (go build is incremental), so a stale binary never shows old screens.
    // E2E_SKIP_BUILD=1 reuses an existing binary, as in the e2e suite.
    if (!existsSync(bin) || (!process.env.E2E_SKIP_BUILD && !opts.bin && !process.env.CLAVIGER_GUIDES_BIN)) {
        log("building claviger");
        mkdirSync(dirname(bin), { recursive: true });
        execFileSync("go", ["build", "-o", bin, "./examples/base"], { cwd: repoRoot, stdio: "inherit" });
    }

    // A private parent directory: the binary writes automigrate files to <dir>/../pb_migrations.
    const sandbox = mkdtempSync(join(tmpdir(), "claviger-guides-"));
    const dataDir = join(sandbox, "pb_data");
    const env = { ...process.env, PB_ENCRYPTION_KEY: DEMO_ENCRYPTION_KEY, CLAVIGER_KEK: DEMO_KEK };
    const superEmail = "operator@example.org";

    execFileSync(bin, ["superuser", "upsert", superEmail, DEMO_PASSWORD, "--dir", dataDir, "--encryptionEnv", "PB_ENCRYPTION_KEY"], {
        env,
        cwd: sandbox,
        stdio: "ignore",
    });

    const logFile = join(sandbox, "server.log");
    const out = createWriteStream(logFile);
    await new Promise((ok) => out.on("open", ok));
    const child: ChildProcess = spawn(bin, ["serve", "--http", `127.0.0.1:${port}`, "--dir", dataDir, "--encryptionEnv", "PB_ENCRYPTION_KEY"], {
        env,
        cwd: sandbox,
        stdio: ["ignore", out, out],
    });
    child.on("exit", (code) => {
        if (code) console.error(`world: claviger exited with ${code}, see ${logFile}`);
    });
    await waitFor(`${base}/api/health`);

    const smtp = await startSmtp(smtpPort).catch((e) => {
        child.kill("SIGTERM");
        throw e;
    });

    const ownsBrowser = !opts.browser;
    const browser = opts.browser ?? (await chromium.launch());
    const contexts: BrowserContext[] = [];

    const stop = async () => {
        for (const c of contexts) await c.close().catch(() => {});
        if (ownsBrowser) await browser.close().catch(() => {});
        await smtp.close().catch(() => {});
        child.kill("SIGTERM");
        await new Promise((r) => setTimeout(r, 400));
        if (!process.env.GUIDES_KEEP) rmSync(sandbox, { recursive: true, force: true });
    };

    try {
        return await seed({ base, port, smtpPort, dataDir, sandbox, superEmail, smtp, browser, contexts, stop, logFile });
    } catch (e) {
        await stop();
        throw e;
    }
}

type Ctx = {
    base: string;
    port: number;
    smtpPort: number;
    dataDir: string;
    sandbox: string;
    superEmail: string;
    smtp: Awaited<ReturnType<typeof startSmtp>>;
    browser: Browser;
    contexts: BrowserContext[];
    stop: () => Promise<void>;
    logFile: string;
};

async function seed(c: Ctx): Promise<World> {
    const { base } = c;

    // ---- plumbing ---------------------------------------------------------------------------
    const rawSignIn = async (collection: string, identity: string, password: string) => {
        const r = await fetch(`${base}/api/collections/${collection}/auth-with-password`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ identity, password }),
        });
        if (!r.ok) throw new Error(`sign-in ${identity}: ${r.status} ${await r.text()}`);
        return (await r.json()) as { token: string; record: { id: string } };
    };
    const su = await rawSignIn("_superusers", c.superEmail, DEMO_PASSWORD);
    let suToken = su.token;

    const api = async (method: string, path: string, body?: unknown, token = suToken) => {
        const r = await fetch(base + path, {
            method,
            headers: { Authorization: token, "Content-Type": "application/json" },
            body: body === undefined ? undefined : JSON.stringify(body),
        });
        const text = await r.text();
        if (!r.ok) throw new Error(`${method} ${path}: ${r.status} ${text.slice(0, 500)}`);
        return text ? JSON.parse(text) : {};
    };

    const personas: Record<string, Persona> = {};
    const tokens: Record<string, string> = {};
    const tokenFor = async (key: string) => {
        const p = personas[key];
        tokens[key] = (await rawSignIn(p.collection, p.email, p.password)).token;
        return tokens[key];
    };
    const person = async (key: string, name: string, email: string) => {
        const rec = await api("POST", "/api/collections/users/records", {
            email,
            name,
            password: DEMO_PASSWORD,
            passwordConfirm: DEMO_PASSWORD,
            verified: true,
        });
        personas[key] = { key, name, email, password: DEMO_PASSWORD, id: rec.id, collection: "users" };
        return rec;
    };

    const vars: Record<string, string> = {};
    const lazy: Record<string, () => Promise<string>> = {};
    const passkeys: Record<string, Passkey> = {};

    // ---- instance settings ------------------------------------------------------------------
    log("settings");
    await api("PATCH", "/api/settings", {
        meta: { appName: "Claviger demo", appUrl: base, senderName: "St. Brendan's Parish", senderAddress: "office@example.org" },
        smtp: { enabled: true, host: "127.0.0.1", port: c.smtpPort, tls: false },
        // the 'last N days' audit and the read audit are the settings a guide documents
        audit: { retentionDays: 365 },
        erasure: { graceDays: 14 },
    });
    await api("PATCH", "/api/collections/users", { webauthn: { enabled: true, discoverable: true } });
    await api("PATCH", "/api/collections/_superusers", { webauthn: { enabled: true, discoverable: false } });
    vars.superEmail = c.superEmail;

    // ---- people -----------------------------------------------------------------------------
    log("people");
    await person("org-owner", "Maeve Okafor", "maeve.okafor@example.org");
    await person("coordinator", "Brigid Halloran", "brigid.halloran@example.org");
    await person("registrar", "Tomas Brennan", "tomas.brennan@example.org");
    await person("greeter", "Aoife Rourke", "aoife.rourke@example.org");
    await person("clergy", "Fr. Anselm Whitcombe", "anselm.whitcombe@example.org");
    await person("care-team", "Priya Ellington", "priya.ellington@example.org");
    await person("guardian", "Dara Calloway", "dara.calloway@example.com");
    await person("guardian2", "Idris Calloway", "idris.calloway@example.com");
    await person("member", "Hal Fenwick", "hal.fenwick@example.com");
    await person("leaver", "Quillon Marsh", "quillon.marsh@example.com"); // asks for erasure by email
    await person("leaver2", "Elowen Tate", "elowen.tate@example.com"); // confirmed an erasure, still in its grace period
    await person("lapsed", "Lowell Pennington", "lowell.pennington@example.com"); // erased by staff
    await person("banned", "Corwin Ashby", "corwin.ashby@example.com"); // the sessions and ban screen
    await person("visitor", "Seren Doyle", "seren.doyle@example.com");
    personas["org-admin"] = personas.coordinator; // the same person under the name the docs use
    vars.guardianEmail = personas.guardian.email;
    // the person with an open invitation has no account yet: the persona names the invitation page's visitor
    personas.invitee = { key: "invitee", name: "Ciaran Doyle", email: "ciaran.doyle@example.com", password: DEMO_PASSWORD, id: "", collection: "users" };
    vars.guardianId = personas.guardian.id;
    // the confirm pages of the admin UI read the e-mail and collection from the token's payload
    // before they show a form; this one is unsigned, so the server would refuse it, but the page renders
    const b64 = (o: unknown) => Buffer.from(JSON.stringify(o)).toString("base64url");
    const usersId = (await api("GET", "/api/collections/users")).id;
    vars.demoToken = `${b64({ alg: "HS256", typ: "JWT" })}.${b64({ email: personas.guardian.email, newEmail: "dara.calloway@example.net", collectionId: usersId, exp: 4102444800 })}.demo`;
    vars.demoPassword = DEMO_PASSWORD;
    // a newcomer who has not confirmed their address yet: the confirm-verification page gets a real
    // token from the mail, so it shows the success state (confirming twice still succeeds)
    await api("POST", "/api/collections/users/records", { email: "tamsin.rowe@example.com", name: "Tamsin Rowe", password: DEMO_PASSWORD, passwordConfirm: DEMO_PASSWORD });
    await api("POST", "/api/collections/users/request-verification", { email: "tamsin.rowe@example.com" });
    const verifyMail = await c.smtp.waitFor((m) => m.to.includes("tamsin.rowe@example.com") && /confirm-verification\//.test(m.text));
    vars.verificationToken = verifyMail.text.match(/confirm-verification\/([A-Za-z0-9_.-]+)/)?.[1] ?? "";
    if (!vars.verificationToken) throw new Error("no verification link in the mail");

    // ---- organizations ----------------------------------------------------------------------
    log("organizations");
    const diocese = await api("POST", "/api/orgs", { name: "Diocese of Harrowmere", slug: "harrowmere-diocese", kind: "diocese" });
    const parish = await api("POST", "/api/orgs", {
        name: "St. Brendan's Parish",
        slug: "st-brendans",
        kind: "parish",
        parent: diocese.id,
        owner: personas["org-owner"].email,
    });
    const columba = await api("POST", "/api/orgs", {
        name: "St. Columba's Parish",
        slug: "st-columbas",
        kind: "parish",
        parent: diocese.id,
        owner: personas.coordinator.email,
    });
    await api("POST", "/api/orgs", { name: "Youth Ministry", slug: "st-brendans-youth", kind: "ministry", parent: parish.id });
    const orgId = parish.id as string;
    vars.orgId = orgId;
    vars.orgSlug = parish.slug;
    vars.orgName = parish.name;
    vars.dioceseId = diocese.id;
    vars.columbaSlug = columba.slug;

    for (const [name, description] of [
        ["registrar", "Signs the permanent registers"],
        ["clergy", "Reads notes shared with clergy"],
        ["care-team", "Reads care notes"],
        ["breakglass", "May ask for emergency access"],
        ["moderator", "May archive a pastoral note"],
        ["greeter", "Welcomes people and keeps the households"],
        ["safeguarding", "Coordinates safe-environment credentials"],
    ]) {
        await api("POST", `/api/orgs/${orgId}/tags`, { name, description }).catch((e) => {
            if (!/exist|already|unique/i.test(String(e))) throw e;
        });
    }
    const member = (key: string, role: string, tags: string[]) => api("POST", `/api/orgs/${orgId}/members`, { user: personas[key].id, role, tags });
    await member("coordinator", "admin", ["safeguarding"]);
    await member("registrar", "member", ["registrar"]);
    await member("greeter", "member", ["greeter"]);
    await member("clergy", "member", ["clergy", "breakglass"]);
    await member("care-team", "member", ["care-team", "breakglass"]);
    await member("guardian", "member", []);
    await member("guardian2", "member", []);
    await member("member", "member", []);
    await member("visitor", "viewer", []);
    // the owner can sign in to the hosted pages as staff: give them the safeguarding tag too
    await api("PATCH", `/api/orgs/${orgId}/members/${(await api("GET", `/api/orgs/${orgId}/members?perPage=100`)).items.find((m: any) => m.email == personas["org-owner"].email).id}`, { tags: ["safeguarding"] });

    // an invitation that is still open (the hosted invitation page), and a second one that was sent a while ago
    log("invitations");
    await api("POST", `/api/orgs/${orgId}/invitations`, { email: "ciaran.doyle@example.com", role: "member", tags: [] });
    const inviteMail = await c.smtp.waitFor((m) => m.to.includes("ciaran.doyle@example.com"));
    vars.inviteToken = inviteMail.text.match(/\/orgs\/invite\/([A-Za-z0-9_-]{20,})/)?.[1] ?? "";
    if (!vars.inviteToken) throw new Error("no invitation link in the mail");
    await api("POST", `/api/orgs/${orgId}/invitations`, { email: "nessa.kearney@example.com", role: "member", tags: ["care-team"] });

    // SCIM: a token and settings (the token is shown once, so the world keeps it)
    log("scim");
    const scim = await api("POST", `/api/orgs/${orgId}/scim-tokens`, { name: "Directory sync" });
    vars.scimToken = scim.token ?? "";

    // ---- households -------------------------------------------------------------------------
    log("households");
    const calloway = await api("POST", "/api/households", {
        org: orgId,
        name: "Calloway family",
        address: { line1: "14 Tanner's Row", city: "Harrowmere", region: "Wessex", postalCode: "HM3 4QP", country: "GB" },
        homePhone: "+1 214 555 0142",
        first: { name: "Dara Calloway", role: "head", guardian: true, user: personas.guardian.id },
    });
    vars.householdId = calloway.id;
    await api("POST", `/api/households/${calloway.id}/members`, { name: "Idris Calloway", role: "adult", guardian: true, user: personas.guardian2.id });
    const wren = await api("POST", `/api/households/${calloway.id}/members`, {
        name: "Wren Calloway",
        role: "child",
        isMinor: true,
        birthDate: "2017-04-19",
        allergies: "Peanuts",
    });
    vars.childMemberId = wren.id;
    const fenwick = await api("POST", "/api/households", {
        org: orgId,
        name: "Fenwick household",
        address: { line1: "2 Mill Lane", city: "Harrowmere", region: "Wessex", postalCode: "HM1 9AB", country: "GB" },
        first: { name: "Hal Fenwick", role: "head", guardian: true, user: personas.member.id, birthDate: "1984-03-02" },
    });
    vars.fenwickHouseholdId = fenwick.id;
    // a likely duplicate, for the "find duplicates" panel
    await api("POST", "/api/households", {
        org: orgId,
        name: "Fenwick (Mill Lane)",
        address: { line1: "2 Mill Lane", city: "Harrowmere", region: "Wessex", postalCode: "HM1 9AB", country: "GB" },
        first: { name: "Hal Fenwick", role: "head", guardian: true, birthDate: "1984-03-02" },
    });

    // ---- credentials ------------------------------------------------------------------------
    log("credentials");
    await api("POST", `/api/orgs/${orgId}/credential-types`, { key: "safe_environment", name: "Safe environment training", validMonths: 36 });
    await api("POST", `/api/orgs/${orgId}/credential-types`, { key: "background_check", name: "Background check", validMonths: 12, requiresVerifier: true });
    await api("POST", `/api/orgs/${orgId}/credentials`, { user: personas["care-team"].id, type: "safe_environment" });
    await api("POST", `/api/orgs/${orgId}/credentials`, { user: personas.clergy.id, type: "safe_environment" });
    await api("POST", `/api/orgs/${orgId}/credentials`, { user: personas["care-team"].id, type: "background_check" });
    const bc = await api("POST", `/api/orgs/${orgId}/credentials`, { user: personas.coordinator.id, type: "background_check" });
    vars.pendingCredentialId = bc.id ?? "";

    // ---- consents ---------------------------------------------------------------------------
    log("consents");
    await api("POST", "/api/consent-purposes", {
        key: "parish-newsletter",
        name: "Parish newsletter",
        text: "We email you the weekly newsletter and notices of changes to services.",
    });
    await api("POST", `/api/orgs/${orgId}/consent-purposes`, {
        key: "photo-release",
        name: "Photographs at parish events",
        text: "Photographs of you or your child may appear on the parish noticeboard and website.",
    });
    const gtoken = await tokenFor("guardian");
    await api("POST", "/api/me/consents/parish-newsletter/grant", {}, gtoken);
    await api("POST", `/api/orgs/${orgId}/consents`, {
        purpose: "photo-release",
        member: wren.id,
        action: "grant",
        method: "paper",
        evidenceRef: "Form 12, binder A",
        givenBy: calloway.members?.[0]?.id ?? (await api("GET", `/api/households/${calloway.id}`)).members.find((m: any) => m.name == "Dara Calloway").id,
    });

    // ---- developer-facing data: two collections, one mapped in the data map ------------------
    log("collections");
    await api("POST", "/api/collections", {
        name: "volunteer_rota",
        type: "base",
        fields: [
            { name: "ministry", type: "text", required: true },
            { name: "volunteer", type: "relation", collectionId: (await api("GET", "/api/collections/users")).id, maxSelect: 1 },
            { name: "serviceDate", type: "date" },
            { name: "notes", type: "text" },
        ],
        listRule: "@request.auth.id != ''",
        viewRule: "@request.auth.id != ''",
    });
    const rota = [
        ["Welcome team", "guardian", "2026-03-22 09:00:00.000Z", "Doors at 8:30"],
        ["Readers", "member", "2026-03-22 09:00:00.000Z", "Second reading"],
        ["Coffee after Mass", "guardian2", "2026-03-29 11:00:00.000Z", ""],
        ["Welcome team", "care-team", "2026-03-29 09:00:00.000Z", ""],
    ];
    for (const [ministry, who, serviceDate, notes] of rota) {
        await api("POST", "/api/collections/volunteer_rota/records", { ministry, volunteer: personas[who].id, serviceDate, notes });
    }
    await api("POST", "/api/collections", {
        name: "newsletter_drafts",
        type: "base",
        fields: [
            { name: "title", type: "text", required: true },
            { name: "body", type: "editor" },
            { name: "created", type: "autodate", onCreate: true },
        ],
    });
    await api("POST", "/api/collections/newsletter_drafts/records", { title: "Advent at St. Brendan's", body: "<p>Our Advent schedule begins on 29 November.</p>" });
    await api("POST", "/api/collections", {
        name: "event_signups",
        type: "base",
        fields: [
            { name: "event", type: "text", required: true },
            { name: "attendee", type: "text" },
        ],
    });
    // volunteer_rota names people, so it gets a data map entry; newsletter_drafts stays unmapped (the warning)
    await api("POST", "/api/collections/_dataMap/records", {
        collection: "volunteer_rota",
        treatment: "anonymize",
        subjectPaths: ["volunteer"],
        personalFields: ["notes"],
        note: "Rota entries keep the date and drop the notes.",
    }).catch((e) => log(`data map entry skipped: ${String(e).slice(0, 120)}`));

    // ---- registers (a passkey is the registrar's signature) ---------------------------------
    log("registers");
    await api("POST", "/api/registers", { template: "sacramental", name: "baptisms" });
    await api("POST", "/api/registers", { template: "sacramental", name: "marriages", signers: 2 });
    const rtoken = await tokenFor("registrar");
    const draft = async (collection: string, surname: string, givenNames: string, extra: Record<string, unknown> = {}) =>
        (await api("POST", `/api/collections/${collection}/records`, { org: orgId, sacrament: "baptism", surname, givenNames, ...extra }, rtoken)).id as string;
    const entryFinal = await draft("baptisms", "Calloway", "Wren Aoife", { sacramentDate: "2017-06-04 10:30:00.000Z" });
    const entryDraft = await draft("baptisms", "Fenwick", "Orla");
    await draft("baptisms", "Doyle", "Ciaran Patrick");
    const marriageDraft = await draft("marriages", "Ellington", "Priya and Joss");
    vars.registerFinalId = entryFinal;
    vars.registerDraftId = entryDraft;

    // ---- passkeys: the superuser and the registrar, through the real /passkeys page ---------
    log("passkeys");
    const enroll = (collection: string, who: { email: string; password: string }, key: string) => enrollPasskey(c, collection, who, key);
    passkeys.registrar = await enroll("users", personas.registrar, "registrar");
    passkeys.clergy = await enroll("users", personas.clergy, "clergy");

    const registrarFresh = () => passkeys.registrar.fresh();
    await api("POST", `/api/registers/baptisms/${entryFinal}/finalize`, {}, await registrarFresh());
    await api("POST", `/api/registers/baptisms/${entryFinal}/annotations`, { kind: "correction", text: "Second given name is Aoife", reason: "Parish record seen" }, await registrarFresh());
    await api("POST", `/api/registers/marriages/${marriageDraft}/finalize`, {}, await registrarFresh()); // waits for a second signer
    vars.registerPendingId = marriageDraft;

    // ---- pastoral notes ---------------------------------------------------------------------
    log("pastoral notes");
    await api("POST", "/api/pastoral", { name: "pastoral_notes", orgs: [orgId] });
    // the clergy and the care team need the tags the call above made
    const note = async (who: string, body: Record<string, unknown>) =>
        api("POST", "/api/collections/pastoral_notes/records", { org: orgId, author: personas[who].id, ...body }, await tokenFor(who));
    await note("clergy", { kind: "care", visibility: "clergy", body: "Visited the family after the funeral. They would welcome a call next week." });
    await note("care-team", { kind: "care", visibility: "care", body: "Meal rota for the Calloway family agreed with the volunteers." });
    await note("guardian", { kind: "prayer", visibility: "congregation", body: "Please pray for our son Wren, who starts at a new school this week." });
    await note("member", { kind: "prayer", visibility: "congregation", sealed: false, body: "Thanksgiving for Hal's recovery." });

    // ---- OIDC clients -----------------------------------------------------------------------
    log("OIDC");
    const redirect = "https://directory.example.org/callback";
    const first = await api("POST", "/api/oidc/clients", {
        name: "Parish directory",
        authCollection: "users",
        redirectUris: [redirect],
        postLogoutUris: ["https://directory.example.org/"],
        firstParty: true,
        allowDeviceFlow: true,
        scopes: ["openid", "profile", "email", "offline_access", "orgs"],
    });
    const third = await api("POST", "/api/oidc/clients", {
        name: "Rota planner",
        authCollection: "users",
        redirectUris: ["https://rota.example.com/callback"],
        firstParty: false,
        allowDeviceFlow: true,
    });
    vars.oidcClientId = first.clientId;
    vars.oidcThirdPartyId = third.clientId;
    vars.oidcRedirect = redirect;
    Object.assign(lazy, makeLazy(base, vars));

    // ---- API key ----------------------------------------------------------------------------
    log("API key");
    const key = await api("POST", "/api/apikeys", { name: "Rota planner sync", scopes: ["users:read", "volunteer_rota:read"] }).catch(async () =>
        api("POST", "/api/apikeys", { name: "Rota planner sync", scopes: ["*"] }),
    );
    vars.apiKeyPrefix = String(key.prefix ?? "");

    // ---- a backup with a fixed name ----------------------------------------------------------
    log("backup");
    await api("POST", "/api/backups", { name: "demo-before-advent.zip" }).catch((e) => log(`backup skipped: ${String(e).slice(0, 120)}`));

    // ---- the banned account (the sessions and ban dialog) ------------------------------------
    log("sessions and ban");
    await tokenFor("banned"); // a session to list
    await api("POST", "/api/bans", { collection: "users", record: personas.banned.id, reason: "Repeated spam from this account" });

    // ---- data protection ---------------------------------------------------------------------
    log("data protection");
    // from here the superuser holds a passkey, and the gated routes want a fresh assertion
    passkeys.superuser = await enrollPasskey(c, "_superusers", { email: c.superEmail, password: DEMO_PASSWORD }, "superuser");
    const suFresh = () => passkeys.superuser.fresh();
    await api("POST", "/api/gdpr/retention/policies", { name: "Archive old newsletter drafts", collection: "newsletter_drafts", basis: "created", after: 540, unit: "days", action: "delete" });
    const hold = await api(
        "POST",
        "/api/gdpr/holds",
        { scope: "subject", subject: personas.lapsed.id, subjectCollection: "users", reason: "Parish dispute, kept for the diocesan hearing", reference: "DIO-2026-014" },
        await suFresh(),
    ).catch((e) => log(`hold skipped: ${String(e).slice(0, 200)}`));
    vars.holdId = hold?.id ?? "";
    await api("POST", `/api/gdpr/subjects/${personas.guardian.id}/export`, {});

    // a person asks for their own erasure by email: the confirm and cancel links are the hosted pages
    const leaverToken = await tokenFor("leaver");
    await api("POST", "/api/me/erasure", { password: DEMO_PASSWORD }, leaverToken).catch((e) => log(`self erasure: ${String(e).slice(0, 200)}`));
    const erasureMail = await c.smtp.waitFor((m) => m.to.includes(personas.leaver.email) && /erasure\/(confirm|cancel)/.test(m.text), 8000).catch(() => null);
    if (erasureMail) {
        vars.erasureConfirmToken = erasureMail.text.match(/\/erasure\/confirm\?token=([A-Za-z0-9_-]+)/)?.[1] ?? "";
    }
    // a second person confirmed theirs: the grace email carries the cancel link, which is the cancel page
    await api("POST", "/api/me/erasure", { password: DEMO_PASSWORD }, await tokenFor("leaver2")).catch((e) => log(`self erasure 2: ${String(e).slice(0, 200)}`));
    const confirmMail2 = await c.smtp.waitFor((m) => m.to.includes(personas.leaver2.email) && /erasure\/confirm/.test(m.text), 8000).catch(() => null);
    if (confirmMail2) {
        const tok = confirmMail2.text.match(/\/erasure\/confirm\?token=([A-Za-z0-9_-]+)/)?.[1] ?? "";
        await api("POST", "/api/gdpr/erasure/confirm", { token: tok }, "");
        const graceMail = await c.smtp.waitFor((m) => m.to.includes(personas.leaver2.email) && /erasure\/cancel/.test(m.text), 8000).catch(() => null);
        vars.erasureCancelToken = graceMail?.text.match(/\/erasure\/cancel\?token=([A-Za-z0-9_-]+)/)?.[1] ?? "";
    }
    // and staff start one for the lapsed member, still in its grace period
    const erase = await api(
        "POST",
        `/api/gdpr/subjects/${personas.lapsed.id}/erase`,
        { identityCheck: "Spoke to Lowell in person at the parish office", graceDays: 14 },
        await suFresh(),
    ).catch((e) => log(`staff erasure: ${String(e).slice(0, 200)}`));
    vars.erasureRequestId = erase?.id ?? "";

    // ---- recovery: a code issued to Hal by Tomas, confirmed by Maeve (two staff, both with a passkey) ---
    // the codes are one-time, so the world only makes the staff page worth looking at
    vars.dummy = "";

    suToken = (await rawSignIn("_superusers", c.superEmail, DEMO_PASSWORD)).token;

    // ---- webhooks ------------------------------------------------------------------------------
    // Created last, and never sent a test event: the endpoints are invented hosts, so any delivery
    // ends in a DNS error ("lookup ... no such host") and a failure streak, which the webhooks page
    // shows as an error state (and which differs by resolver: CI retried, a laptop did not). With
    // nothing firing at them (the Rota planner listens to volunteer_rota:update, a collection no capture edits, where a users:update subscription would be hit by every later state that edits a person), the page shows two healthy endpoints and no failures.
    log("webhooks");
    const hook = await api("POST", "/api/webhooks", {
        url: "https://hooks.example.org/claviger",
        events: ["users:create", "orgs:*"],
        description: "Directory sync",
    });
    vars.webhookId = hook.endpoint.id;
    await api("POST", "/api/webhooks", { url: "https://rota.example.com/hooks/claviger", events: ["volunteer_rota:update"], description: "Rota planner" });

    const mails = c.smtp.mails;
    return {
        base,
        port: c.port,
        dataDir: c.dataDir,
        sandbox: c.sandbox,
        superuser: { key: "superuser", name: "Parish operator", email: c.superEmail, password: DEMO_PASSWORD, id: su.record.id, collection: "_superusers", token: suToken },
        personas,
        vars,
        lazy,
        api: (m, p, b, t) => api(m, p, b, t ?? suToken),
        tokenFor,
        passkeys,
        mails,
        stop: c.stop,
    };
}

/**
 * Registers a passkey for a person through the real /passkeys page and keeps the browser context,
 * so the same virtual authenticator can sign in again whenever a fresh assertion is needed.
 */
async function enrollPasskey(c: Ctx, collection: string, who: { email: string; password: string }, _key: string): Promise<Passkey> {
    const ctx = await c.browser.newContext({ baseURL: c.base });
    c.contexts.push(ctx);
    const page = await ctx.newPage();
    await parkConditionalMediation(page);
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("WebAuthn.enable");
    const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
        options: { protocol: "ctap2", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
    });
    const url = `/passkeys?collection=${collection}`;
    const storageKey = `pb_passkeys_auth_${collection}`;

    await page.goto(url);
    await page.locator("#linkPasswordLogin").click();
    await page.locator("#pwEmail").fill(who.email);
    await page.locator("#pwPass").fill(who.password);
    await page.locator("#btnPwLogin").click();
    await page.locator("#me").filter({ hasText: who.email }).waitFor();
    await page.locator("#btnRegister").click();
    await page.getByText("Passkey added.").waitFor({ timeout: 15_000 });

    const fresh = async () => {
        await page.goto(url);
        if (await page.locator("#linkLogout").isVisible().catch(() => false)) await page.locator("#linkLogout").click();
        await page.locator("#identity").fill(who.email);
        await page.locator("#btnLogin").click();
        await page.locator("#me").filter({ hasText: who.email }).waitFor();
        const stored = await page.evaluate((k) => localStorage.getItem(k), storageKey);
        return JSON.parse(stored!).token as string;
    };
    await fresh(); // proves the passkey signs in

    return { fresh, credentials: async () => (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials };
}

/** Parks the sign-in form's conditional passkey autofill, which a virtual authenticator answers at once. */
export async function parkConditionalMediation(page: Page) {
    await page.addInitScript(() => {
        const get = navigator.credentials.get.bind(navigator.credentials);
        navigator.credentials.get = (options?: CredentialRequestOptions) => {
            if (options && (options as any).mediation === "conditional") {
                return new Promise<Credential | null>((_resolve, reject) => {
                    options.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
                });
            }
            return get(options);
        };
    });
}

// `node world.ts --keep` starts the world and waits, to look around in a browser.
if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
    const w = await startWorld();
    const credentials: Record<string, any[]> = {};
    for (const [k, p] of Object.entries(w.passkeys)) credentials[k] = await p.credentials();
    writeFileSync(join(w.sandbox, "world.json"), JSON.stringify({ base: w.base, vars: w.vars, personas: w.personas, superuser: { ...w.superuser, token: undefined }, credentials }, null, 2));
    console.log(`world: ready at ${w.base}/_/  (operator ${w.superuser.email} / ${DEMO_PASSWORD}); state in ${w.sandbox}/world.json`);
    if (!process.argv.includes("--keep")) await w.stop();
    else process.on("SIGINT", () => w.stop().then(() => process.exit(0)));
}

/** Placeholders whose value is made fresh on every use: an authorization request, a device code. */
export function makeLazy(base: string, vars: Record<string, string>): Record<string, () => Promise<string>> {
    const authorize = (clientId: string, redirectUri: string, extra: Record<string, string> = {}, state = "demo") => {
        const u = new URL(base + "/oidc/authorize");
        const q: Record<string, string> = {
            client_id: clientId,
            redirect_uri: redirectUri,
            response_type: "code",
            scope: "openid profile email",
            state,
            code_challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
            code_challenge_method: "S256",
            ...extra,
        };
        for (const [k, v] of Object.entries(q)) u.searchParams.set(k, v);
        return u.pathname + u.search;
    };
    return {
        oidcLogin: async () => authorize(vars.oidcClientId, vars.oidcRedirect),
        oidcConsent: async () => authorize(vars.oidcThirdPartyId, "https://rota.example.com/callback", {}, "rota"),
        oidcError: async () => authorize("unknown-client-id", vars.oidcRedirect),
        oidcBadRedirect: async () => authorize(vars.oidcClientId, "https://elsewhere.example.net/callback"),
        oidcChooser: async () => authorize(vars.oidcClientId, vars.oidcRedirect, { prompt: "select_account" }),
        deviceUserCode: async () => {
            const r = await fetch(base + "/oidc/device_authorization", {
                method: "POST",
                headers: { "Content-Type": "application/x-www-form-urlencoded" },
                body: new URLSearchParams({ client_id: vars.oidcClientId, scope: "openid profile" }),
            });
            const body: any = await r.json();
            if (!body.user_code) throw new Error(`device authorization: ${JSON.stringify(body)}`);
            return body.user_code as string;
        },
    };
}

/** Reconnects to a world that `node world.ts --keep` left running (a dev aid: no passkey signing in). */
export async function attachWorld(file: string): Promise<World> {
    const j = JSON.parse(readFileSync(file, "utf8"));
    const rawSignIn = async (collection: string, identity: string, password: string) => {
        const r = await fetch(`${j.base}/api/collections/${collection}/auth-with-password`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ identity, password }),
        });
        if (!r.ok) throw new Error(`sign-in ${identity}: ${r.status}`);
        return (await r.json()) as { token: string; record: { id: string } };
    };
    const su = await rawSignIn("_superusers", j.superuser.email, DEMO_PASSWORD);
    const api = async (method: string, path: string, body?: unknown, token = su.token) => {
        const r = await fetch(j.base + path, { method, headers: { Authorization: token, "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
        const text = await r.text();
        if (!r.ok) throw new Error(`${method} ${path}: ${r.status} ${text.slice(0, 300)}`);
        return text ? JSON.parse(text) : {};
    };
    const passkeys: Record<string, Passkey> = {};
    for (const [k, creds] of Object.entries<any[]>(j.credentials ?? {})) {
        passkeys[k] = { credentials: async () => creds, fresh: async () => { throw new Error("attached world cannot sign"); } };
    }
    return {
        base: j.base,
        port: Number(new URL(j.base).port),
        dataDir: "",
        sandbox: "",
        superuser: { ...j.superuser, token: su.token },
        personas: j.personas,
        vars: j.vars,
        lazy: makeLazy(j.base, j.vars),
        api,
        tokenFor: async (key) => {
            const p = j.personas[key];
            return (await rawSignIn(p.collection, p.email, p.password)).token;
        },
        passkeys,
        mails: [],
        stop: async () => {},
    };
}
