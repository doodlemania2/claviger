import { test, expect, state, api } from "../fixtures";
import { authorizeUrl, exchangeCode, pkce, verifyJwt } from "../oidc";
import type { Page } from "@playwright/test";
import { createServer, type Server } from "node:http";

const REDIRECT = "http://localhost:19001/callback";
const POST_LOGOUT = "http://localhost:19001/loggedout";

type Client = { id: string; secret: string };
const clients: Record<string, Client> = {};

async function createClientInUi(admin: Page, name: string, firstParty: boolean): Promise<Client> {
    await admin.goto("/_/#/oidc");
    await admin.getByRole("button", { name: "New client" }).click();
    await admin.getByLabel("Name", { exact: true }).fill(name);
    await admin.getByLabel("Redirect URIs (one per line)", { exact: true }).fill(REDIRECT);
    await admin.getByLabel("Post logout redirect URIs (one per line)", { exact: true }).fill(POST_LOGOUT);
    const toggle = admin.getByLabel("First party", { exact: true });
    if ((await toggle.isChecked()) !== firstParty) await toggle.click({ force: true });
    await admin.getByRole("button", { name: "Create", exact: true }).click();

    await expect(admin.getByText("shown only once")).toBeVisible();
    const id = await admin.getByLabel("Client ID").inputValue();
    const secret = await admin.getByLabel("Client secret").inputValue();
    expect(id).toBeTruthy();
    expect(secret.length).toBeGreaterThan(16);
    await admin.getByRole("button", { name: "Done" }).click();
    await expect(admin.getByRole("row", { name: new RegExp(name) })).toBeVisible();
    return { id, secret };
}

// The relying party: a server that only answers its redirect URIs, so the test
// can read the code from the URL the browser ends up on.
let rp: Server;
test.beforeAll(async () => {
    rp = createServer((_req, res) => res.writeHead(200, { "Content-Type": "text/plain" }).end("relying party"));
    await new Promise<void>((ok) => rp.listen(19001, "127.0.0.1", ok));
});
test.afterAll(async () => {
    await new Promise((ok) => rp.close(ok));
});

async function signInOnHostedPage(page: Page, email: string, password: string) {
    await page.getByLabel("Email or username").fill(email);
    await page.getByLabel("Password").fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
}

test.describe.configure({ mode: "serial" });

test("OIDC: register a first-party and a consent-requiring client in the admin UI", async ({ admin }) => {
    clients.first = await createClientInUi(admin, "E2E first party", true);
    clients.third = await createClientInUi(admin, "E2E third party", false);

    // the discovery URL shown on the page is the real one
    await admin.goto("/_/#/oidc");
    const shown = await admin.locator("code", { hasText: "openid-configuration" }).innerText();
    const disco = await (await fetch(shown)).json();
    expect(disco.issuer).toBe(`${state().base}/oidc`);
});

test("OIDC: authorization code + PKCE through the hosted login, tokens verified, SSO, consent, logout", async ({ browser, problems }) => {
    const { base, user } = state();
    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    const { watch } = await import("../fixtures");
    watch(page, problems);

    // ---- first-party client: login page, no consent
    const { verifier, challenge } = pkce();
    await page.goto(
        authorizeUrl(base, {
            client_id: clients.first.id,
            redirect_uri: REDIRECT,
            response_type: "code",
            scope: "openid profile email offline_access",
            state: "st-1",
            nonce: "nonce-1",
            code_challenge: challenge,
            code_challenge_method: "S256",
        }),
    );
    await expect(page).toHaveURL(/\/oidc\/login\?authRequestID=/);
    await expect(page.getByRole("heading", { name: /sign in/i })).toBeVisible();
    await expect(page.locator("body")).toContainText("E2E first party");
    await signInOnHostedPage(page, user.email, user.password);

    await page.waitForURL(/localhost:19001\/callback/);
    const cb = new URL(page.url());
    expect(cb.searchParams.get("state")).toBe("st-1");
    const code = cb.searchParams.get("code")!;
    expect(code).toBeTruthy();

    const tok = await exchangeCode(base, { clientId: clients.first.id, clientSecret: clients.first.secret, code, redirectUri: REDIRECT, verifier });
    expect(tok.status).toBe(200);
    expect(tok.body.refresh_token).toBeTruthy();
    const idc = await verifyJwt(base, tok.body.id_token);
    expect(idc.iss).toBe(`${base}/oidc`);
    expect(idc.aud).toContain(clients.first.id);
    expect(idc.nonce).toBe("nonce-1");
    expect(idc.email).toBe(user.email);
    const ui = await (await fetch(base + "/oidc/userinfo", { headers: { Authorization: `Bearer ${tok.body.access_token}` } })).json();
    expect(ui.sub).toBe(idc.sub);

    // a used code is dead
    const again = await exchangeCode(base, { clientId: clients.first.id, clientSecret: clients.first.secret, code, redirectUri: REDIRECT, verifier });
    expect(again.status).toBe(400);

    // ---- single sign-on: the same browser gets a code without a credentials form
    const second = pkce();
    await page.goto(
        authorizeUrl(base, {
            client_id: clients.first.id,
            redirect_uri: REDIRECT,
            response_type: "code",
            scope: "openid",
            state: "st-2",
            code_challenge: second.challenge,
            code_challenge_method: "S256",
        }),
    );
    await page.waitForURL(/localhost:19001\/callback\?.*state=st-2/);

    // ---- third-party client: consent page, Allow
    const third = pkce();
    await page.goto(
        authorizeUrl(base, {
            client_id: clients.third.id,
            redirect_uri: REDIRECT,
            response_type: "code",
            scope: "openid email",
            state: "st-3",
            code_challenge: third.challenge,
            code_challenge_method: "S256",
        }),
    );
    await expect(page).toHaveURL(/\/oidc\/consent/);
    await expect(page.locator("body")).toContainText("E2E third party");
    await page.getByRole("button", { name: "Allow" }).click();
    await page.waitForURL(/localhost:19001\/callback\?.*state=st-3/);

    // ---- third-party client again with prompt=consent, then Cancel -> access_denied
    const fourth = pkce();
    await page.goto(
        authorizeUrl(base, {
            client_id: clients.third.id,
            redirect_uri: REDIRECT,
            response_type: "code",
            scope: "openid email",
            state: "st-4",
            prompt: "consent",
            code_challenge: fourth.challenge,
            code_challenge_method: "S256",
        }),
    );
    await expect(page).toHaveURL(/\/oidc\/consent/);
    await page.getByRole("button", { name: "Cancel" }).click();
    await page.waitForURL(/localhost:19001\/callback\?.*state=st-4/);
    expect(new URL(page.url()).searchParams.get("error")).toBe("access_denied");

    // ---- logout: the hosted "signed out" page, and the SSO session is gone
    await page.goto(`${base}/oidc/end_session?id_token_hint=${tok.body.id_token}&post_logout_redirect_uri=${encodeURIComponent(POST_LOGOUT)}&state=bye`);
    await page.waitForURL(/localhost:19001\/loggedout/);
    await page.goto(`${base}/oidc/logged-out`);
    await expect(page.getByRole("heading", { name: "You are signed out" })).toBeVisible();

    const fifth = pkce();
    await page.goto(
        authorizeUrl(base, {
            client_id: clients.first.id,
            redirect_uri: REDIRECT,
            response_type: "code",
            scope: "openid",
            state: "st-5",
            prompt: "none",
            code_challenge: fifth.challenge,
            code_challenge_method: "S256",
        }),
    );
    await page.waitForURL(/localhost:19001\/callback\?.*state=st-5/);
    expect(new URL(page.url()).searchParams.get("error")).toBe("login_required");

    await ctx.close();
});

test("OIDC: the admin page lists the signing key and rotates it", async ({ admin }) => {
    await admin.goto("/_/#/oidc");
    const keys = admin.getByRole("row").filter({ has: admin.locator("td") });
    await expect(admin.locator("body")).toContainText("Signing keys");
    const before = (await api("GET", "/api/oidc/keys")).length;
    expect(before).toBeGreaterThan(0);
    await admin.getByRole("button", { name: "Rotate keys" }).click();
    const yes = admin.locator(".modal.popup.manual").getByRole("button", { name: "Yes" });
    if (await yes.isVisible().catch(() => false)) await yes.click();
    await expect.poll(async () => (await api("GET", "/api/oidc/keys")).length).toBeGreaterThan(before);
    await expect(keys.first()).toBeVisible();
});
