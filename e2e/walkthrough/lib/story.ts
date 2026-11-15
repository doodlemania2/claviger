// Everything a viewer reads that differs between the two stories lives here: the invented people, their
// addresses (all example.org: a name reserved for documentation) and the scene list.
// No real person, no real parish, no real domain appears anywhere in this rig.
import type { Person, StoryId, World } from "./world";

export interface Story {
    id: StoryId;
    title: string;
    /** The superuser of the throwaway instance. */
    adminEmail: string;
    /** The scene ids this story runs, in order. */
    scenes: string[];
    cast: Record<string, Person>;
}

const person = (key: string, name: string): [string, Person] => [
    key,
    { name, email: `${key}@example.org`, password: `walk-${key}-pass-2026` },
];

const DEVELOPER: Story = {
    id: "developer",
    title: "Claviger for developers: set up, sign in, ship",
    adminEmail: "tomas@example.org",
    scenes: ["first-run", "users-passkeys", "passkey-signin", "totp", "api-key", "oidc", "webhook", "audit", "field-encryption", "backup", "telemetry"],
    cast: Object.fromEntries([person("priya", "Priya Raman"), person("jonas", "Jonas Weld")]),
};

const PARISH: Story = {
    id: "parish",
    title: "Claviger for a parish: people, care and records",
    adminEmail: "diocesan-it@example.org",
    scenes: ["orgs", "staff", "households", "safeguarding", "recovery", "consent", "register", "pastoral", "transfer", "data-protection"],
    cast: Object.fromEntries([
        person("hugo", "Hugo Marsh"),
        person("basil", "Fr. Basil Thorne"),
        person("ignatius", "Fr. Ignatius Wren"),
        person("maren", "Maren Castellano"),
        person("dora", "Dora Quill"),
        person("ellis", "Ellis Fenner"),
        person("rosa", "Rosa Vasquez-Hale"),
        person("agnes", "Agnes Pembrook"),
        person("tobias", "Tobias Reed"),
    ]),
};

export const STORIES: Record<StoryId, Story> = { developer: DEVELOPER, parish: PARISH };

export const storyOf = (world: World): Story => STORIES[world.story];

/** Names used by the parish scenes. */
export const DIOCESE = { name: "Diocese of Marlowe Vale", slug: "marlowe-vale" };
export const PARISH_ORG = { name: "Holy Wayfarer Parish", slug: "holy-wayfarer" };
export const SECOND_PARISH = { name: "Wayside Chapel Parish", slug: "wayside-chapel" };
