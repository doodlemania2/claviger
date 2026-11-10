import { test, expect, state, api, watch } from "../fixtures";
import { authorizeUrl, pkce } from "../oidc";
import { createServer, type Server } from "node:http";
import type { BrowserContext, CDPSession, Page } from "@playwright/test";

const REDIRECT = "http://localhost:19002/callback";
let rp: Server;
test.beforeAll(async () => {
    rp = createServer((_req, res) => res.writeHead(200, { "Content-Type": "text/plain" }).end("relying party"));
    await new Promise<void>((ok) => rp.listen(19002, "127.0.0.1", ok));
});
test.afterAll(async () => {
    await new Promise((ok) => rp.close(ok));
});

/** Chromium's virtual authenticator: a platform authenticator that approves every ceremony. */
async function addVirtualAuthenticator(ctx: BrowserContext, page: Page): Promise<{ cdp: CDPSession; authenticatorId: string }> {
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("WebAuthn.enable");
    const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
        options: {
            protocol: "ctap2",
            transport: "internal",
            hasResidentKey: true,
            hasUserVerification: true,
            isUserVerified: true,
            automaticPresenceSimulation: true,
        },
    });
    return { cdp, authenticatorId };
}

/**
 * The page starts conditional-UI autofill whenever it shows the sign-in form, and a virtual
 * authenticator with automatic presence answers that immediately, so right after a sign-out the
 * page signs itself in and races the test's own clicks. Tests of the manual button path park
 * conditional requests until the page aborts them, the way a real browser does with no user
 * gesture on the autofill UI.
 */
async function parkConditionalMediation(page: Page) {
    await page.addInitScript(() => {
        const get = navigator.credentials.get.bind(navigator.credentials);
        navigator.credentials.get = (options?: CredentialRequestOptions) => {
            if (options && (options as any).mediation === "conditional") {
                return new Promise<Credential | null>((_resolve, reject) => {
                    const abort = () => reject(new DOMException("aborted", "AbortError"));
                    if (options.signal?.aborted) return abort();
                    options.signal?.addEventListener("abort", abort);
                });
            }
            return get(options);
        };
    });
}

test.describe.configure({ mode: "serial" });

test("Passkeys page: register a passkey after a password sign-in, then sign in with it", async ({ browser, problems }) => {
    const { base, user } = state();
    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    watch(page, problems);
    await parkConditionalMediation(page);
    const { cdp, authenticatorId } = await addVirtualAuthenticator(ctx, page);

    await page.goto("/passkeys");
    await expect(page.getByRole("heading", { name: "Passkeys" })).toBeVisible();

    // password sign-in to get a session to register from
    await page.locator("#linkPasswordLogin").click();
    await page.locator("#pwEmail").fill(user.email);
    await page.locator("#pwPass").fill(user.password);
    await page.locator("#btnPwLogin").click();
    await expect(page.locator("#me")).toContainText(user.email);

    await page.locator("#btnRegister").click();
    await expect.poll(async () => (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials.length, { timeout: 15_000 }).toBe(1);
    // the authenticator holds one discoverable credential and the server one record
    const { credentials } = await cdp.send("WebAuthn.getCredentials", { authenticatorId });
    expect(credentials).toHaveLength(1);
    expect(credentials[0].isResidentCredential).toBe(true);
    // the authenticator holds the credential before the browser has sent the finish request,
    // so wait for the server to store it (on a loaded runner that takes a moment)
    await expect(page.getByText("Passkey registered.")).toBeVisible();
    await expect.poll(async () => (await api("GET", "/api/collections/_webauthnCredentials/records")).items.length, { timeout: 15_000 }).toBe(1);

    // sign out and sign in with the passkey alone
    await page.locator("#linkLogout").click();
    await expect(page.locator("#anon")).toBeVisible();
    await page.locator("#identity").fill(user.email);
    await page.locator("#btnLogin").click();
    await expect(page.locator("#me")).toContainText(user.email);
    await page.locator("#linkLogout").click();

    // ---- the same passkey on the OIDC hosted login (a client registered through the API)
    const client = await api("POST", "/api/oidc/clients", {
        name: "E2E passkey app",
        authCollection: "users",
        redirectUris: [REDIRECT],
        firstParty: true,
    });
    const { challenge } = pkce();
    await page.goto(
        authorizeUrl(base, {
            client_id: client.clientId,
            redirect_uri: REDIRECT,
            response_type: "code",
            scope: "openid",
            state: "pk-1",
            code_challenge: challenge,
            code_challenge_method: "S256",
        }),
    );
    await expect(page).toHaveURL(/\/oidc\/login/);
    await page.getByLabel("Email or username").fill(user.email);
    await page.getByRole("button", { name: "Sign in with a passkey" }).click();
    await page.waitForURL(/localhost:19002\/callback\?.*state=pk-1/);
    expect(new URL(page.url()).searchParams.get("code")).toBeTruthy();

    await ctx.close();
});

test("Passkeys page: a discoverable credential signs in without typing an identity", async ({ browser, problems }) => {
    const { base, user } = state();
    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    watch(page, problems);
    await parkConditionalMediation(page);
    const { cdp, authenticatorId } = await addVirtualAuthenticator(ctx, page);

    // seed: register through the page, as a user would
    await page.goto("/passkeys");
    await page.locator("#linkPasswordLogin").click();
    await page.locator("#pwEmail").fill(user.email);
    await page.locator("#pwPass").fill(user.password);
    await page.locator("#btnPwLogin").click();
    await expect(page.locator("#me")).toContainText(user.email);
    await page.locator("#btnRegister").click();
    await expect.poll(async () => (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials.length).toBeGreaterThan(0);
    await page.locator("#linkLogout").click();

    // no identity typed: the button uses the discoverable flow
    await page.locator("#btnLogin").click();
    await expect(page.locator("#me")).toContainText(user.email);
    await ctx.close();
});

test("Passkeys page: sign-in autofill signs in by itself after a sign-out", async ({ browser, problems }) => {
    const { base, user } = state();
    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    watch(page, problems);
    const { cdp, authenticatorId } = await addVirtualAuthenticator(ctx, page);

    await page.goto("/passkeys");
    await page.locator("#linkPasswordLogin").click();
    await page.locator("#pwEmail").fill(user.email);
    await page.locator("#pwPass").fill(user.password);
    await page.locator("#btnPwLogin").click();
    await expect(page.locator("#me")).toContainText(user.email);
    await page.locator("#btnRegister").click();
    await expect.poll(async () => (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials.length).toBeGreaterThan(0);

    // no click: the conditional request the page starts is answered by the authenticator
    await page.locator("#linkLogout").click();
    await expect(page.locator("#me")).toBeVisible();
    await expect(page.locator("#me")).toContainText(user.email);
    await ctx.close();
});
