import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import type { World } from "./world";

const stores = new Map<string, () => Promise<void>>();

/** Chromium's conditional-UI autofill is answered at once by a virtual authenticator, which makes a
 *  sign-in page sign itself in and race the film's own clicks. Park those requests until the page
 *  aborts them, the way a real browser does with no gesture on the autofill UI. */
export async function parkConditionalMediation(page: Page): Promise<void> {
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

/**
 * A virtual WebAuthn authenticator (CDP), the stand-in for a phone's passkey. Its credentials are
 * saved after enrolment and put back in the next scene, so a person keeps one passkey for the run.
 */
export async function virtualAuthenticator(page: Page, world: World, name: string): Promise<{ count: () => Promise<number> }> {
    await parkConditionalMediation(page);
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("WebAuthn.enable");
    const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
        options: { protocol: "ctap2", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
    });
    const file = join(world.dir, "auth", `${name}-passkeys.json`);
    if (existsSync(file)) {
        const saved = JSON.parse(readFileSync(file, "utf8")) as Record<string, unknown>[];
        for (const credential of saved) await cdp.send("WebAuthn.addCredential", { authenticatorId, credential } as never);
    }
    stores.set(name, async () => {
        const { credentials } = await cdp.send("WebAuthn.getCredentials", { authenticatorId });
        writeFileSync(file, JSON.stringify(credentials));
    });
    return { count: async () => (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials.length };
}

/** Save the credentials the authenticator holds, after enrolling one. */
export async function savePasskeys(name: string): Promise<void> {
    await stores.get(name)?.();
}
