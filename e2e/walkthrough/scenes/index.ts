import type { Browser } from "@playwright/test";
import type { World } from "../lib/world";
import { firstRun } from "./dev-first-run";
import { apiKeyScene, totpScene } from "./dev-security";
import { oidcScene } from "./dev-oidc";
import { webhookScene } from "./dev-webhook";
import { auditScene, backupScene, fieldEncryptionScene, telemetryScene } from "./dev-ops";
import { orgsScene, staffScene } from "./parish-orgs";
import { householdsScene, safeguardingScene } from "./parish-care";
import { consentScene, recoveryScene } from "./parish-people";
import { dataProtectionScene, pastoralScene, registerScene, transferScene } from "./parish-records";
import { passkeySignin, usersPasskeys } from "./dev-users";

export type SceneFn = (browser: Browser, world: World, index: number) => Promise<void>;

export const SCENES: Record<string, SceneFn> = {
    "first-run": firstRun,
    "users-passkeys": usersPasskeys,
    "passkey-signin": passkeySignin,
    totp: totpScene,
    "api-key": apiKeyScene,
    oidc: oidcScene,
    webhook: webhookScene,
    audit: auditScene,
    "field-encryption": fieldEncryptionScene,
    backup: backupScene,
    telemetry: telemetryScene,
    orgs: orgsScene,
    staff: staffScene,
    households: householdsScene,
    safeguarding: safeguardingScene,
    recovery: recoveryScene,
    consent: consentScene,
    register: registerScene,
    pastoral: pastoralScene,
    transfer: transferScene,
    "data-protection": dataProtectionScene,
};
