import { expect, type Browser } from "@playwright/test";
import { SceneRun } from "../lib/scene";
import { stack } from "../lib/stack";
import { beat, ensureAdmin, open, press, typeIn } from "../lib/ui";
import type { World } from "../lib/world";

/** Scene 6: an OIDC client in the admin UI, then a sample app signs people in: passkey, consent, account chooser. */
export async function oidcScene(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(
        browser,
        world,
        index,
        "oidc",
        "Sign in to an app with Claviger",
        "An OIDC client, then a sample app: passkey, consent and the account chooser",
        [
            { name: "tomas", device: "desktop", label: "Tomas Alder (developer)" },
            { name: "priya", device: "desktop", label: "Priya Raman (user)", passkey: true },
        ],
        "row",
    );
    const tomas = scene.actor("tomas");
    const priya = scene.actor("priya");
    try {
        const { rig } = stack();
        await open(priya.page, `${rig.url}/`);
        await priya.say("A sample app, Alder Notes, wants to sign people in with Claviger");

        await ensureAdmin(tomas.page, world.admin);
        await open(tomas.page, `${world.base}/_/#/oidc`);
        await tomas.say("Tomas registers Alder Notes as an OIDC client. It is a third party app, so people will be asked for consent");
        await press(tomas.page.getByRole("button", { name: "New client" }), tomas.page, 800);
        await typeIn(tomas.page.getByLabel("Name", { exact: true }), "Alder Notes");
        await typeIn(tomas.page.getByLabel("Redirect URIs (one per line)", { exact: true }), rig.redirectUri());
        const toggle = tomas.page.getByLabel("First party", { exact: true });
        if (await toggle.isChecked()) await toggle.click({ force: true });
        await beat(tomas.page, 1000);
        await press(tomas.page.getByRole("button", { name: "Create", exact: true }), tomas.page, 1200);
        await expect(tomas.page.getByText("shown only once")).toBeVisible();
        rig.oidc.clientId = await tomas.page.getByLabel("Client ID").inputValue();
        rig.oidc.clientSecret = await tomas.page.getByLabel("Client secret").inputValue();
        await tomas.say("The client id and secret go into the app's configuration. The secret is shown once", 3500);
        await press(tomas.page.getByRole("button", { name: "Done" }), tomas.page, 800);
        await expect(tomas.page.getByRole("row", { name: /Alder Notes/ })).toBeVisible();
        await tomas.say("Discovery, keys and the hosted sign-in pages are served by Claviger itself");

        // the app side
        const p = priya.page;
        // the short OIDC clip (clip.mjs ... oidc oidc-start oidc-done): the app sends her to Claviger, a passkey, consent, signed in
        priya.mark("oidc-start");
        await press(p.getByRole("link", { name: "Sign in with Claviger" }), p, 1000);
        await expect(p).toHaveURL(/\/oidc\/login/);
        await priya.say("Alder Notes sends Priya to Claviger's hosted sign-in. The app never sees her password");
        await typeIn(p.getByLabel("Email or username"), world.people.priya.email);
        await priya.say("She uses the passkey she registered earlier");
        await press(p.getByRole("button", { name: "Sign in with a passkey" }), p, 1200);
        await expect(p).toHaveURL(/\/oidc\/consent/);
        await priya.say("Alder Notes is a third party app, so Claviger asks what she agrees to share");
        await beat(p, 1500);
        await press(p.getByRole("button", { name: "Allow" }), p, 1500);
        await expect(p.getByText("You are signed in")).toBeVisible();
        await priya.say("Back in the app, signed in. It received signed claims, no password");
        await beat(p, 1200);
        priya.mark("oidc-done");

        // the account chooser: a second account in the same browser
        await press(p.getByRole("link", { name: "Switch account" }), p, 1200);
        await priya.say("Switch account opens the account chooser. Priya adds a second account");
        await press(p.getByRole("button", { name: /Use another account/ }).or(p.getByRole("link", { name: /Use another account/ })), p, 1000);
        await typeIn(p.getByLabel("Email or username"), world.people.jonas.email);
        await typeIn(p.getByLabel("Password"), world.people.jonas.password);
        await press(p.getByRole("button", { name: "Sign in", exact: true }), p, 1200);
        await p.waitForURL((u) => u.pathname.includes("/oidc/consent") || u.origin === rig.url);
        if (p.url().includes("/oidc/consent")) await press(p.getByRole("button", { name: "Allow" }), p, 1200);
        await expect(p.getByText("You are signed in")).toBeVisible();
        await priya.say("The browser now holds two Claviger sessions; the app shows the second one");
        await press(p.getByRole("link", { name: "Switch account" }), p, 1200);
        await priya.say("With two accounts the chooser lists both, and she picks one without typing anything", 3500);
        await beat(p, 800);
        await press(p.getByRole("button", { name: new RegExp(world.people.priya.email) }).first(), p, 1500);
        await expect(p.getByText("You are signed in")).toBeVisible();
        await priya.say("Signed in again as Priya. Single sign-on, per account, per browser");
    } finally {
        await scene.end();
    }
}
