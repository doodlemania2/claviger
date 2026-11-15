// The parish story's stage: who belongs where. Everything here is idempotent, so a scene can be run
// on its own (WALK_ONLY) and the steps a scene films itself are only filled in when they are missing.
import { expect } from "@playwright/test";
import type { Actor } from "./scene";
import { api, configureInstance, ensureUser, rawCall, stack } from "./stack";
import { DIOCESE, PARISH_ORG, SECOND_PARISH } from "./story";
import { open, press, typeIn } from "./ui";
import { saveWorld, type World } from "./world";

export const ORG_ROLES = {
    basil: { role: "owner", tags: ["registrar", "clergy", "breakglass"] },
    maren: { role: "admin", tags: ["registrar", "greeter"] },
    dora: { role: "member", tags: ["safeguarding"] },
    ignatius: { role: "member", tags: ["clergy", "breakglass"] },
    ellis: { role: "member", tags: [] as string[] },
    rosa: { role: "member", tags: [] as string[] },
    agnes: { role: "member", tags: [] as string[] },
    tobias: { role: "member", tags: [] as string[] },
} as const;

const TAGS = ["registrar", "greeter", "safeguarding", "clergy", "breakglass", "care-team"];

export async function orgBySlug(slug: string): Promise<any | null> {
    const r = await rawCall("GET", `/api/orgs/${slug}`, undefined, await (await import("./stack")).adminToken());
    return r.status === 200 ? r.json : null;
}

export async function ensureOrg(slug: string, body: Record<string, unknown>): Promise<any> {
    return (await orgBySlug(slug)) ?? (await api("POST", "/api/orgs", { slug, ...body }));
}

export async function ensureMember(orgId: string, email: string, role: string, tags: string[]): Promise<void> {
    const token = await (await import("./stack")).adminToken();
    const r = await rawCall("POST", `/api/orgs/${orgId}/members`, { user: email, role, tags }, token);
    if (r.status < 300) return;
    if (r.status !== 409) throw new Error(`member ${email}: ${r.status} ${r.text}`);
    // already a member (made by an earlier scene): bring the role and tags to what the story says
    const list = await api("GET", `/api/orgs/${orgId}/members?perPage=100`);
    const mine = list.items.find((m: any) => m.email === email);
    if (!mine) throw new Error(`member ${email} not found`);
    const same = mine.role === role && [...(mine.tags ?? [])].sort().join() === [...tags].sort().join();
    if (!same) await api("PATCH", `/api/orgs/${orgId}/members/${mine.id}`, { role, tags });
}

/** The users every parish scene needs, the users collection with passkeys on, and the mail settings. */
export async function prepParish(world: World, skip: string[] = []): Promise<void> {
    for (const key of Object.keys(world.people)) if (!skip.includes(key)) await ensureUser(world, key);
    if (!world.notes.parishPrep) {
        await api("PATCH", "/api/collections/users", { webauthn: { enabled: true, discoverable: true } });
        world.notes.parishPrep = "yes";
        saveWorld(world);
    }
    await configureInstance(world, "Holy Wayfarer Online");
}

/** Diocese, parish and second parish, with the people in them. `staff: false` leaves out what scenes 1 and 2 film. */
export async function ensureParishBase(world: World, opts: { orgs?: boolean; staff?: boolean } = {}): Promise<void> {
    const { orgs = true, staff = true } = opts;
    const p = world.people;
    if (orgs) {
        const diocese = await ensureOrg(DIOCESE.slug, { name: DIOCESE.name, kind: "diocese", owner: p.hugo.email });
        const parish = await ensureOrg(PARISH_ORG.slug, { name: PARISH_ORG.name, kind: "parish", parent: diocese.id, owner: p.basil.email });
        await ensureOrg(SECOND_PARISH.slug, { name: SECOND_PARISH.name, kind: "parish", parent: diocese.id, owner: p.hugo.email });
        world.ids.diocese = diocese.id;
        world.ids.parish = parish.id;
        world.ids.wayside = (await orgBySlug(SECOND_PARISH.slug)).id;
    }
    if (staff && world.ids.parish) {
        for (const t of TAGS) await rawCall("POST", `/api/orgs/${world.ids.parish}/tags`, { name: t }, await (await import("./stack")).adminToken());
        // the owner keeps their tags too
        for (const [key, who] of Object.entries(ORG_ROLES)) {
            if (key === "basil") continue;
            await ensureMember(world.ids.parish, p[key].email, who.role, [...who.tags]);
        }
        const owner = await api("GET", `/api/orgs/${world.ids.parish}/members?perPage=50`);
        const mine = owner.items.find((m: any) => m.email === p.basil.email || m.user === p.basil.id);
        if (mine) await api("PATCH", `/api/orgs/${world.ids.parish}/members/${mine.id}`, { tags: [...ORG_ROLES.basil.tags] });
    }
    saveWorld(world);
}

/** A passkey for this person on this actor's virtual authenticator, registered on /passkeys unless it already holds one. */
export async function ensurePasskey(actor: Actor, who: { email: string; password: string }, collection = "users"): Promise<void> {
    if (!actor.passkeys) throw new Error(`${actor.name} has no authenticator`);
    if ((await actor.passkeys.count()) > 0) return;
    const { page } = actor;
    const q = collection === "users" ? "" : `?collection=${collection}`;
    await open(page, `${stack().world.base}/passkeys${q}`);
    await actor.say("A passkey is needed for the sensitive steps that follow. Added here once, on this device", 3500);
    await page.locator("#linkPasswordLogin").click();
    await typeIn(page.locator("#pwEmail"), who.email);
    await typeIn(page.locator("#pwPass"), who.password);
    await press(page.locator("#btnPwLogin"), page, 1000);
    await expect(page.locator("#me")).toContainText(who.email);
    await press(page.locator("#btnRegister"), page, 800);
    await expect(page.getByText("Passkey added.")).toBeVisible();
    await actor.say("");
}

/** The Vasquez-Hale household with Rosa (guardian, linked to her login) and Teo (minor). Found, or made without a camera. */
export async function ensureHousehold(world: World): Promise<{ id: string; teo: string; rosaMember: string }> {
    const find = async () => {
        const list = await api("GET", `/api/households?org=${world.ids.parish}`);
        return list.items.find((h: any) => h.name === "Vasquez-Hale family");
    };
    let h = await find();
    if (!h) {
        await api("POST", "/api/households", {
            org: world.ids.parish,
            name: "Vasquez-Hale family",
            address: { street: "14 Willow Lane", city: "Marlowe Vale" },
            first: { name: world.people.rosa.name, role: "head", guardian: true, user: world.people.rosa.id },
        });
        h = await find();
        await api("POST", `/api/households/${h.id}/members`, { name: "Teo Vasquez-Hale", role: "child", isMinor: true, birthDate: "2016-04-02" });
    }
    const full = await api("GET", `/api/households/${h.id}`);
    const teo = full.members.find((m: any) => m.name === "Teo Vasquez-Hale");
    const rosa = full.members.find((m: any) => m.name === world.people.rosa.name);
    return { id: h.id, teo: teo.id, rosaMember: rosa.id };
}
