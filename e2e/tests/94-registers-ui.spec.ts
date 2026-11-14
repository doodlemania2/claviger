import { test, expect, state, api } from "../fixtures";
import type { BrowserContext, Page } from "@playwright/test";

// The screens of the permanent registers (#87, 87-5): the hosted page where registrars finalize
// entries and write marginal notes (with a passkey), and the superuser page of the admin UI that
// reads the registers, shows an entry with its margin, checks the chain and withdraws a request.
//
// Finalizing and annotating need a passkey assertion made moments ago, so each registrar gets a
// browser context with Chromium's virtual authenticator and registers a passkey on /passkeys first.
// Specs run in file order on one instance: this one makes its own org, users and registers.

test.describe.configure({ mode: "serial" });

const slug = "e2e-registers-ui";
const carol = { email: "ru-carol@example.com", password: "carol-pass-12345" };
const frank = { email: "ru-frank@example.com", password: "frank-pass-12345" };

let orgId = "";
const ids: Record<string, string> = {};

async function userId(u: { email: string; password: string }): Promise<string> {
    const res = await fetch(state().base + "/api/collections/users/auth-with-password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ identity: u.email, password: u.password }),
    });
    return (await res.json()).record.id;
}

async function draft(collection: string, surname: string): Promise<string> {
    const rec = await api("POST", `/api/collections/${collection}/records`, { org: orgId, sacrament: "baptism", surname, givenNames: "Ann" });
    return rec.id;
}

/** Parks the sign-in form's conditional passkey autofill, which a virtual authenticator answers at once. */
async function parkConditionalMediation(page: Page) {
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

/** A context with a virtual authenticator that already holds a passkey of this person. */
async function registrarPage(newGuardedPage: (ctx?: BrowserContext) => Promise<Page>, browser: any, who: { email: string; password: string }) {
    const ctx: BrowserContext = await browser.newContext({ baseURL: state().base });
    const page = await newGuardedPage(ctx);
    await parkConditionalMediation(page);
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("WebAuthn.enable");
    const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
        options: { protocol: "ctap2", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
    });

    await page.goto("/passkeys");
    await page.locator("#linkPasswordLogin").click();
    await page.locator("#pwEmail").fill(who.email);
    await page.locator("#pwPass").fill(who.password);
    await page.locator("#btnPwLogin").click();
    await expect(page.locator("#me")).toContainText(who.email);
    await page.locator("#btnRegister").click();
    await expect.poll(async () => (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials.length, { timeout: 15_000 }).toBe(1);
    await expect(page.getByText("Passkey added.")).toBeVisible();
    return page;
}

async function openRegisters(page: Page, who: { email: string; password: string }) {
    await page.goto(`/orgs/registers/${slug}`);
    const form = page.locator("form#pw");
    await form.getByLabel("Email or username").fill(who.email);
    await form.getByLabel("Password").fill(who.password);
    await form.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Registers", exact: true })).toBeVisible();
    await page.locator("#regSelect").selectOption("e2eui_baptisms"); // other specs made registers too
}

test.beforeAll(async () => {
    for (const u of [carol, frank]) {
        await api("POST", "/api/collections/users/records", { email: u.email, password: u.password, passwordConfirm: u.password, verified: true });
    }
    const org = await api("POST", "/api/orgs", { name: "E2E Registers UI", slug, owner: state().user.email });
    orgId = org.id;
    await api("POST", `/api/orgs/${orgId}/members`, { user: await userId(carol), role: "member", tags: ["registrar"] });
    await api("POST", `/api/orgs/${orgId}/members`, { user: await userId(frank), role: "member", tags: ["registrar"] });

    await api("POST", "/api/registers", { template: "sacramental", name: "e2eui_baptisms" });
    await api("POST", "/api/registers", { template: "sacramental", name: "e2eui_marriages", signers: 2 });
    ids.smith = await draft("e2eui_baptisms", "Smith");
    ids.jones = await draft("e2eui_marriages", "Jones");
    ids.brown = await draft("e2eui_marriages", "Brown");
});

test("Registers page: a shell with a strict policy, and it needs sign-in", async ({ guarded: page }) => {
    const res = await page.goto(`/orgs/registers/${slug}`);
    expect(res!.headers()["content-security-policy"] || "").not.toContain("unsafe-inline");
    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    await expect(page.locator("body")).not.toContainText("Smith");
});

test("Registers page: an owner reads the entries and cannot finalize", async ({ guarded: page, problems }) => {
    problems.ignore(/status of 40[34]/);
    await openRegisters(page, state().user);
    await expect(page.locator("#readonly")).toContainText("Only a registrar can finalize");
    const row = page.locator(`tr[data-entry='${ids.smith}']`);
    await expect(row).toContainText("Smith");
    await expect(row).toHaveAttribute("data-state", "draft");
    await row.getByRole("button", { name: /Open/ }).click();
    await expect(page.locator("#entryTitle")).toContainText(ids.smith);
    await expect(page.locator("#finalize")).toBeHidden();
    await expect(page.locator("#noteForm")).toBeHidden();
});

test("Registers page: a registrar finalizes with a passkey, writes a note, retracts it and opens the certificate", async ({ browser, newGuardedPage, problems }) => {
    // the console guard's own script is refused in the sandboxed certificate frame, which is the point of the sandbox
    problems.ignore(/Blocked script execution in 'about:(blank|srcdoc)'/);
    problems.ignore(/status of 403.*\/(finalize|annotations)/); // the step-up the page then answers with a passkey
    const page = await registrarPage(newGuardedPage, browser, carol);
    await openRegisters(page, carol);

    const row = page.locator(`tr[data-entry='${ids.smith}']`);
    await row.getByRole("button", { name: /Open/ }).click();
    page.once("dialog", (d) => d.accept());
    await page.locator("#finalize").click();
    await expect(page.locator("#pstatus")).toContainText("The entry is final.");
    await expect(page.locator("#facts")).toContainText("Final");
    await expect(page.locator("#facts")).toContainText("Register number");
    await expect(page.locator("#facts code")).toHaveText(/^[0-9a-f]{64}$/);
    await expect(page.locator("#finalize")).toBeHidden();
    await expect(page.locator(`tr[data-entry='${ids.smith}']`)).toHaveAttribute("data-state", "final");

    // a final entry takes notes, never edits
    const form = page.locator("#noteForm");
    await expect(form).toBeVisible();
    await form.getByLabel("Kind of note").selectOption("correction");
    await form.getByLabel("Note", { exact: true }).fill("Surname is Smyth");
    await form.getByLabel(/^Reason/).fill("Parish record seen");
    await form.getByRole("button", { name: "Add the note" }).click();
    await expect(page.locator("#pstatus")).toContainText("The note was added.");
    const first = page.locator("#notes li").first();
    await expect(first).toContainText("Surname is Smyth");
    await expect(first).toContainText("Reason: Parish record seen");
    await expect(first).toContainText("ru-carol@example.com");

    await form.getByLabel("Kind of note").selectOption("retraction");
    await form.getByLabel("Note to retract").selectOption({ index: 0 });
    await form.getByLabel("Note", { exact: true }).fill("Mistaken");
    await form.getByLabel(/^Reason/).fill("Wrong book");
    await form.getByRole("button", { name: "Add the note" }).click();
    await expect(page.locator("#notes li")).toHaveCount(2);
    await expect(page.locator("#notes li.retracted")).toHaveCount(1);
    await expect(page.locator("#notes li").nth(1)).toContainText("retracts the note");

    // the certificate is fetched with the token and shown in a sandboxed frame
    await page.locator("#certBtn").click();
    const frame = page.frameLocator("#certFrame");
    await expect(frame.locator("h1")).toHaveText("Register entry certificate");
    await expect(frame.locator("table.fields").first()).toContainText("Smith");
    await expect(frame.locator("ol.notes li.retracted")).toHaveCount(1);
    expect(await frame.locator("body").evaluate((b) => getComputedStyle(b).backgroundColor)).not.toBe("rgba(0, 0, 0, 0)"); // the print stylesheet loaded
    await page.locator("#certClose").click();
    await expect(page.locator("#certBox")).toBeHidden();
});

test("Registers page: with two signers a registrar puts an entry forward and a different one confirms", async ({ browser, newGuardedPage, problems }) => {
    problems.ignore(/status of 403.*\/finalize/);
    const carolPage = await registrarPage(newGuardedPage, browser, carol);
    await openRegisters(carolPage, carol);
    await carolPage.locator("#regSelect").selectOption("e2eui_marriages");
    await carolPage.locator(`tr[data-entry='${ids.jones}']`).getByRole("button", { name: /Open/ }).click();
    await expect(carolPage.locator("#finalize")).toHaveText("Put forward for a second signature");
    carolPage.once("dialog", (d) => d.accept());
    await carolPage.locator("#finalize").click();
    await expect(carolPage.locator("#pstatus")).toContainText("waiting for a second signer");
    await expect(carolPage.locator(`tr[data-entry='${ids.jones}']`)).toHaveAttribute("data-state", "pending");
    // the person who put it forward cannot confirm it, and can take the request back
    await expect(carolPage.locator("#finalize")).toBeHidden();
    await expect(carolPage.locator("#waiting")).toContainText("A different registrar has to confirm");
    await expect(carolPage.locator("#withdraw")).toBeVisible();

    const frankPage = await registrarPage(newGuardedPage, browser, frank);
    await openRegisters(frankPage, frank);
    await frankPage.locator("#regSelect").selectOption("e2eui_marriages");
    await frankPage.locator("#stateSelect").selectOption("pending");
    await frankPage.locator(`tr[data-entry='${ids.jones}']`).getByRole("button", { name: /Open/ }).click();
    await expect(frankPage.locator("#facts")).toContainText("ru-carol@example.com");
    await expect(frankPage.locator("#finalize")).toHaveText("Confirm and finalize");
    frankPage.once("dialog", (d) => d.accept());
    await frankPage.locator("#finalize").click();
    await expect(frankPage.locator("#pstatus")).toContainText("The entry is final.");
    await expect(frankPage.locator("#facts")).toContainText("Confirmed by");

    // a second request, left pending for the admin UI test
    await carolPage.locator("#stateSelect").selectOption("");
    await carolPage.locator(`tr[data-entry='${ids.brown}']`).getByRole("button", { name: /Open/ }).click();
    carolPage.once("dialog", (d) => d.accept());
    await carolPage.locator("#finalize").click();
    await expect(carolPage.locator(`tr[data-entry='${ids.brown}']`)).toHaveAttribute("data-state", "pending");
});

test("Registers admin: the registers, an entry with its margin, and the chain check", async ({ admin }) => {
    await admin.goto("/_/#/registers");
    await expect(admin.locator("#register_select")).toBeVisible();
    await admin.locator("#register_select").selectOption("e2eui_baptisms");
    const row = admin.locator(`tr[data-entry='${ids.smith}']`);
    await expect(row).toHaveAttribute("data-state", "final");
    await expect(row).toContainText("Smith");
    await row.getByRole("button", { name: `Open entry ${ids.smith}` }).click();

    const view = admin.locator("#register_entry");
    await expect(view).toContainText("Surname is Smyth");
    await expect(view.locator("li[data-kind='retraction']")).toContainText("Mistaken");
    await expect(view.locator("li[data-kind='correction'] div").nth(1)).toHaveCSS("text-decoration-line", "line-through");
    await expect(view).toContainText("ru-carol@example.com");
    // a superuser cannot sign
    await expect(view.getByRole("button", { name: "Withdraw the request" })).toHaveCount(0);

    await admin.locator("#register_verify_btn").click();
    const verify = admin.locator("#register_verify");
    await expect(verify).toContainText("Every chain is intact.");
    await expect(verify.locator("tr[data-register='e2eui_baptisms']")).toContainText("1");
});

test("Registers admin: a superuser withdraws a pending finalization", async ({ admin }) => {
    await admin.goto("/_/#/registers");
    await admin.locator("#register_select").selectOption("e2eui_marriages");
    await admin.locator("#register_state").selectOption("pending");
    const row = admin.locator(`tr[data-entry='${ids.brown}']`);
    await expect(row).toHaveAttribute("data-state", "pending");
    await row.getByRole("button", { name: `Open entry ${ids.brown}` }).click();
    await admin.locator("#register_withdraw").click();
    await admin.locator(".modal.popup.manual").getByRole("button", { name: "Yes" }).click();
    await expect(admin.locator("#registers_notice")).toContainText("withdrawn");
    await expect(admin.locator(`tr[data-entry='${ids.brown}']`)).toHaveCount(0);

    const entry = await api("GET", `/api/registers/e2eui_marriages/${ids.brown}`);
    expect(entry.state).toBe("draft");
});
