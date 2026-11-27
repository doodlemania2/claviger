import { test, expect, state, api, watch } from "../fixtures";
import { authorizeUrl, pkce } from "../oidc";
import { createServer, type Server } from "node:http";

// #140: the hosted /oidc/login page offers the auth collection's OAuth2 providers.
// The provider is a local stand-in that answers the three requests of the code flow.

const REDIRECT = "http://localhost:19001/callback";
const PROVIDER = "http://127.0.0.1:19002";

let rp: Server;
let idp: Server;
const seen: { authorize: URL[] } = { authorize: [] };

test.beforeAll(async () => {
    rp = createServer((_req, res) => res.writeHead(200, { "Content-Type": "text/plain" }).end("relying party"));
    await new Promise<void>((ok) => rp.listen(19001, "127.0.0.1", ok));

    const { user } = state();
    idp = createServer((req, res) => {
        const url = new URL(req.url ?? "/", PROVIDER);
        if (url.pathname === "/authorize") {
            seen.authorize.push(url);
            const to = new URL(url.searchParams.get("redirect_uri")!);
            to.searchParams.set("code", "mock-code");
            to.searchParams.set("state", url.searchParams.get("state")!);
            return res.writeHead(302, { Location: to.toString() }).end();
        }
        if (url.pathname === "/authorize-coop") {
            // like Microsoft's pages: Cross-Origin-Opener-Policy cuts the opener off from the window
            // (popup.closed reads true) and the person takes a few seconds to sign in
            const to = new URL(url.searchParams.get("redirect_uri")!);
            to.searchParams.set("code", "mock-code");
            to.searchParams.set("state", url.searchParams.get("state")!);
            return res
                .writeHead(200, { "Content-Type": "text/html", "Cross-Origin-Opener-Policy": "same-origin" })
                .end(`<!doctype html><title>Sign in</title><p>Signing in...</p><script>setTimeout(function(){location.href=${JSON.stringify(to.toString())}},4000)</script>`);
        }
        if (url.pathname === "/token") {
            return res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ access_token: "mock-token", token_type: "Bearer", expires_in: 3600 }));
        }
        if (url.pathname === "/userinfo") {
            return res
                .writeHead(200, { "Content-Type": "application/json" })
                .end(JSON.stringify({ sub: "mock-subject-1", email: user.email, email_verified: true, name: "Mock Person" }));
        }
        res.writeHead(404).end();
    });
    await new Promise<void>((ok) => idp.listen(19002, "127.0.0.1", ok));
});

test.afterAll(async () => {
    await new Promise((ok) => rp.close(ok));
    await new Promise((ok) => idp.close(ok));
    await api("PATCH", "/api/collections/users", { oauth2: { enabled: false, providers: [] } });
});

test("OIDC login page: sign in with an OAuth2 provider through the pop-up", async ({ browser, problems }) => {
    const { base } = state();

    // no provider configured: no button
    const client = await api("POST", "/api/oidc/clients", {
        name: "E2E OAuth app",
        authCollection: "users",
        redirectUris: [REDIRECT],
        firstParty: true,
    });

    const plain = await browser.newContext({ baseURL: base });
    const p0 = await plain.newPage();
    watch(p0, problems);
    const first = pkce();
    const authz = (st: string, c: string) =>
        authorizeUrl(base, { client_id: client.clientId, redirect_uri: REDIRECT, response_type: "code", scope: "openid profile email", state: st, code_challenge: c, code_challenge_method: "S256" });
    await p0.goto(authz("st-0", first.challenge));
    await expect(p0.getByLabel("Email or username")).toBeVisible();
    await expect(p0.locator("#oauthBox")).toBeHidden();
    await plain.close();

    // a provider is configured on the collection
    await api("PATCH", "/api/collections/users", {
        oauth2: {
            enabled: true,
            providers: [
                {
                    name: "oidc",
                    displayName: "Mock Microsoft",
                    clientId: "mock-client",
                    clientSecret: "mock-secret",
                    authURL: `${PROVIDER}/authorize`,
                    tokenURL: `${PROVIDER}/token`,
                    userInfoURL: `${PROVIDER}/userinfo`,
                    pkce: true,
                },
            ],
        },
    });

    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    watch(page, problems);
    const second = pkce();
    await page.goto(authz("st-1", second.challenge));
    await expect(page).toHaveURL(/\/oidc\/login\?authRequestID=/);

    const button = page.getByRole("button", { name: "Continue with Mock Microsoft" });
    await expect(button).toBeVisible();
    await expect(page.getByLabel("Email or username")).toBeVisible(); // password sign-in is still there

    await button.click();

    // the pop-up went to the provider with the realtime client id as the state and our redirect URI
    await page.waitForURL(/localhost:19001\/callback/, { timeout: 30_000 });
    const cb = new URL(page.url());
    expect(cb.searchParams.get("state")).toBe("st-1");
    expect(cb.searchParams.get("code")).toBeTruthy();

    expect(seen.authorize.length).toBe(1);
    const q = seen.authorize[0].searchParams;
    expect(q.get("redirect_uri")).toBe(`${base}/api/oauth2-redirect`);
    expect(q.get("client_id")).toBe("mock-client");
    expect(q.get("code_challenge_method")).toBe("S256");
    expect(q.get("state")!.length).toBeGreaterThan(10);

    await ctx.close();
});

test("OIDC login page: a provider window cut off by Cross-Origin-Opener-Policy still completes", async ({ browser, problems }) => {
    const { base } = state();

    await api("PATCH", "/api/collections/users", {
        oauth2: {
            enabled: true,
            providers: [
                {
                    name: "oidc",
                    displayName: "Mock Microsoft",
                    clientId: "mock-client",
                    clientSecret: "mock-secret",
                    authURL: `${PROVIDER}/authorize-coop`,
                    tokenURL: `${PROVIDER}/token`,
                    userInfoURL: `${PROVIDER}/userinfo`,
                    pkce: true,
                },
            ],
        },
    });

    const client = await api("POST", "/api/oidc/clients", {
        name: "E2E OAuth COOP app",
        authCollection: "users",
        redirectUris: [REDIRECT],
        firstParty: true,
    });

    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    watch(page, problems);
    const k = pkce();
    await page.goto(
        authorizeUrl(base, { client_id: client.clientId, redirect_uri: REDIRECT, response_type: "code", scope: "openid profile email", state: "st-coop", code_challenge: k.challenge, code_challenge_method: "S256" }),
    );
    await page.getByRole("button", { name: "Continue with Mock Microsoft" }).click();

    // the pop-up is cut off after a moment but the page keeps waiting and the sign-in completes
    await expect(page.locator("#oauthCancel")).toBeVisible();
    await page.waitForURL(/localhost:19001\/callback/, { timeout: 40_000 });
    expect(new URL(page.url()).searchParams.get("state")).toBe("st-coop");

    await ctx.close();
});

test("OIDC login page: cancel ends the wait", async ({ browser, problems }) => {
    const { base } = state();
    const client = await api("POST", "/api/oidc/clients", {
        name: "E2E OAuth cancel app",
        authCollection: "users",
        redirectUris: [REDIRECT],
        firstParty: true,
    });

    const ctx = await browser.newContext({ baseURL: base });
    const page = await ctx.newPage();
    watch(page, problems);
    const k = pkce();
    await page.goto(
        authorizeUrl(base, { client_id: client.clientId, redirect_uri: REDIRECT, response_type: "code", scope: "openid profile email", state: "st-cancel", code_challenge: k.challenge, code_challenge_method: "S256" }),
    );
    await page.getByRole("button", { name: "Continue with Mock Microsoft" }).click();
    await page.locator("#oauthCancel").click();
    await expect(page.locator("#status")).toContainText("did not complete");
    await expect(page.locator("#oauthCancel")).toBeHidden();
    await expect(page.getByRole("button", { name: "Continue with Mock Microsoft" })).toBeEnabled();

    await ctx.close();
});
