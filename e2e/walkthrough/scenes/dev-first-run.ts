import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, type Browser } from "@playwright/test";
import { SceneRun } from "../lib/scene";
import { configureInstance, installerUrl, stack } from "../lib/stack";
import { beat, open, press, typeIn } from "../lib/ui";
import type { World } from "../lib/world";

/** Scene 1: the first start prints an installer link; the first superuser is made on screen; a short tour of the admin UI. */
export async function firstRun(browser: Browser, world: World, index: number): Promise<void> {
    const scene = await SceneRun.begin(browser, world, index, "first-run", "First run and the admin tour", "The first superuser, then a look around the admin UI", [
        { name: "tomas", device: "desktop", label: "Tomas Alder (developer)" },
    ]);
    const a = scene.actor("tomas");
    const { page, say } = a;
    try {
        const base = world.base;
        const log = readFileSync(join(world.dir, "server.log"), "utf8").split("\n").filter(Boolean).slice(0, 6);
        await open(page, `${stack().rig.url}/blank`);
        await a.card("$ claviger serve --dir ./pb_data --encryptionEnv PB_ENCRYPTION_KEY", log.map((l) => l.replace(/(pbinstall\/)\S+/, "$1<one-time token>")));
        await say("Tomas starts Claviger for the first time. With no superuser yet, it prints a one-time installer link", 4500);
        await a.card(null);

        // Only the first run: later runs of a resumed story already have the superuser.
        if (!world.notes.superuserMade) {
            await open(page, installerUrl(world));
            await say("The link opens the installer: it creates the first superuser");
            await typeIn(page.locator("#superuser_email"), world.admin.email);
            await typeIn(page.locator("#superuser_password"), world.admin.password);
            await typeIn(page.locator("#superuser_password_confirm"), world.admin.password);
            await press(page.getByRole("button", { name: "Create superuser and login" }), page, 1500);
            world.notes.superuserMade = "yes";
        } else {
            await open(page, `${base}/_/#/login`);
        }
        await expect(page).toHaveURL(/#\/collections/);
        await configureInstance(world, "Alder and Finch Auth");

        const tour: [string, string][] = [
            ["collections", "Collections: the built-in users collection is already an auth collection with sign-in, sessions and tokens"],
            ["logs", "Logs: every request, with its status and timing"],
            ["audit", "The audit log: every change to a record, who made it and from where"],
            ["orgs", "Organizations: trees of orgs with roles, tags and SCIM provisioning"],
            ["oidc", "The OpenID Connect provider: register clients, rotate signing keys"],
            ["webhooks", "Signed webhooks with a delivery log"],
            ["settings", "Settings: application URL, mail, security, storage, backups, encryption keys"],
        ];
        for (const [route, text] of tour) {
            await open(page, `${base}/_/#/${route}`);
            await say(text);
        }
    } finally {
        await scene.end();
    }
}
