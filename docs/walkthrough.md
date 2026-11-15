# Recorded walkthroughs

Two recorded videos of Claviger, made by a Playwright rig against a **local** instance: one for a developer who sets Claviger up, one for a parish that runs its people, care and records on it. The rig lives in `e2e/walkthrough/`. It is not part of CI and not part of the default `npx playwright test` run.

It is modeled on the bidjoy walkthrough rig, with these differences: it films a throwaway binary on `localhost` (no staging, no ssh, no proxy), and it can never be pointed at a deployment, because the base URL is always `http://localhost:<port>`.

## Run it

```sh
e2e/walkthrough/run.sh                      # the developer story
WALK_STORY=parish e2e/walkthrough/run.sh    # the parish story
```

`run.sh`:

1. Builds `e2e/.bin/claviger` (skipped when `E2E_SKIP_BUILD` is set and the binary exists, the same rule as `e2e/global-setup.ts`).
2. Runs `playwright.walkthrough.config.ts`. The test process starts the binary with a temp data dir (`<run>/pb_data`), `PB_ENCRYPTION_KEY` (32 random characters, which also gives the field-encryption key by HKDF, so encrypted fields work), `CLAVIGER_WEBHOOKS_ALLOW_LOCALHOST=true` and a metrics token, on `localhost:18190`. It also starts an SMTP sink and the rig's small servers (below).
3. Stitches the scenes with `stitch.mjs` (ffmpeg at `/opt/homebrew/bin/ffmpeg`).

Output is in `e2e/walkthrough/.out/<timestamp>/` (git-ignored):

| Path | What |
| --- | --- |
| `video/walkthrough-<story>.mp4` and `.vtt` | all scenes, with a title card before each, and the captions shifted to match |
| `video/NN-<scene>.mp4` and `.vtt` | one per scene |
| `video/manifest.json` | story, scene number, id, title, duration, size and file names of every file, plus the skipped steps |
| `scenes/NN-<scene>/<actor>.webm` | the raw Playwright recording of each actor |
| `scenes/NN-<scene>/last-<actor>.png` | the last screen of each actor (the first thing to look at when a scene fails) |
| `world.json`, `server.log` | state carried between scenes, and the server's log |

The delivered files are H.264, 1280x720, yuv420p, `+faststart`, no audio. They are composed at 1920x1080 and scaled down. The `.vtt` files carry the same caption text and timings that were drawn on screen (the recorder logs each caption's start and end); with two actors on screen each cue carries a voice tag with the actor's name.

Settings (environment):

| Variable | Meaning |
| --- | --- |
| `WALK_STORY` | `developer` (default) or `parish` |
| `WALK_ONLY` | scene numbers or ids, like `1,3` or `oidc`. A partial run is **not stitched** (set `WALK_STITCH=1` to stitch anyway). `WALK_ONLY=1` is the quick smoke check |
| `WALK_FAST` | `1` turns off the slow motion and the reading pauses: a fast check that every scene works (the videos are far too quick to watch) |
| `WALK_SLOWMO` | milliseconds between browser actions (default 200) |
| `WALK_PACE` | multiplier for the reading pauses after captions (default 1) |
| `WALK_RUN_DIR` | resume an earlier run: same data dir, same people, same passkeys. Scenes that need an earlier scene's data make it themselves when it is missing |
| `WALK_PORT` | base port (default 18190; the SMTP sink uses +400 and the rig +900) |

A single scene can be re-run on its own: `WALK_STORY=parish WALK_ONLY=7 e2e/walkthrough/run.sh`. Each scene prepares what it needs without a camera (the people, the orgs, a register) when an earlier scene did not.

The overlay (caption bar, name badge, click ripple, cursor dot, card) is checked by `e2e/tests/96-walkthrough-overlay.spec.ts`, which runs in the normal e2e suite: it installs the same init script on the admin UI and on a hosted page and fails on a CSP violation or a console error.

## How it works

- **One Playwright video per actor per scene.** A scene opens a browser context per actor (`lib/scene.ts`). Two actors are stitched side by side (`row` layout); one actor plays alone. `lib/overlay.ts` draws the on-screen helpers with DOM and CSSOM only (`el.style`), never an injected `<style>` or `<script>`, because Claviger's Content-Security-Policy refuses both.
- **Cards.** Where a step has no screen, the actor shows a *card*: the API call and its answer in monospace, taken from the real response.
- **Passkeys.** Each actor can have a Chromium virtual authenticator (CDP). Its credentials are saved after a scene and put back in the next, so a person keeps one passkey for the run. Steps that need a fresh passkey (recovery codes, finalizing a register entry, break-glass, legal holds) are real ceremonies against the virtual authenticator.
- **Email.** The app sends to an SMTP sink (`e2e/smtp.ts`, the one the e2e suite uses). A small mailbox page on the rig renders the captured message, and the actor clicks the link in it, so invitation, notice and reset links are followed as a person would.
- **The rig's local servers** (`lib/rig.ts`, localhost only): the mailbox; *Alder Notes*, a sample relying party that signs in with OpenID Connect (authorization code with PKCE); and a CRM receiver that checks the Standard Webhooks signature.
- **Skipped steps.** A step the product cannot show is recorded in `world.json` and in `manifest.json` (`skipped`) and said on screen with a caption starting "Not shown here".

## Stories

All people are invented and use `example.org`. Names and addresses are in `lib/story.ts`.

### `developer`: a self-hoster sets up Claviger

| # | Scene | What it shows |
| --- | --- | --- |
| 1 | first-run | The server prints a one-time installer link; the first superuser is made in the installer; a tour of the admin UI |
| 2 | users-passkeys | WebAuthn switched on for the users collection in the collection editor; the first user made in the record editor |
| 3 | passkey-signin | The user registers a passkey on `/passkeys`, signs out, signs in with it (with and without typing the email) |
| 4 | totp | The superuser enrols an authenticator app, signs in with a code, then with a backup code |
| 5 | api-key | A read-only key scoped to one collection, shown once, used (allowed, and refused for writes and other collections), revoked |
| 6 | oidc | An OIDC client in the admin UI, then the sample app signs in with a passkey, shows consent, and the account chooser with two accounts. Two actors side by side |
| 7 | webhook | A signed webhook to the local CRM receiver, a test event, a real `record.created`, the delivery log. Two actors side by side |
| 8 | audit | The audit log: filter and open an entry |
| 9 | field-encryption | An encrypted text field, its ciphertext in the SQL console, the encryption keys page and a key rotation |
| 10 | backup | An encrypted backup, and the built-in restore verification |
| 11 | telemetry | Skipped with a caption (see below); the metrics endpoint is shown |

### `parish`: Diocese of Marlowe Vale and Holy Wayfarer Parish

| # | Scene | What it shows |
| --- | --- | --- |
| 1 | orgs | The superuser creates the diocese and the parish under it |
| 2 | staff | The pastor invites the parish secretary by email (she registers from the link), creates tags, sets a role and a tag. Two actors |
| 3 | households | Staff create a family with a guardian and a child; the guardian edits the child's allergies and medical notes on `/households/mine`. Two actors |
| 4 | safeguarding | A credential type and a credential recorded in the admin UI; the volunteer's `/credentials/mine`; a roster collection whose rules open only while the credential is current. Two actors |
| 5 | recovery | A greeter vouches for an older member, who sets up a new passkey on her phone with a short code, and signs in with it. Two actors, one on a phone |
| 6 | consent | A registrar adds a consent purpose on the hosted page; a guardian consents for her child; the office reads the history. Two actors |
| 7 | register | A baptism register with two signers: put forward, confirmed with a different registrar's passkey, a marginal correction, the certificate. Two actors |
| 8 | pastoral | A sealed pastoral note the owner cannot read; break-glass with two clergy, passkeys, the notice email, the read, the audit trail. Two actors |
| 9 | transfer | A member moves to another parish with his own consent. Two actors |
| 10 | data-protection | The data map, an export, a legal hold (passkey step-up), a retention policy preview, an erasure and its result |

## What cannot be shown, and why

Each of these is recorded as a skipped step and says so on screen.

| Story | Step | Why |
| --- | --- | --- |
| developer | Telemetry settings screen | There is none. OTLP logs, traces and metrics are configured by environment variables. The scene shows the Prometheus endpoint (`/api/metrics`, behind `CLAVIGER_METRICS_TOKEN`) instead |
| parish | A break-glass screen | Break-glass has routes, an email and an audit trail but no page. The request, approval and read are shown as API calls with their real answers, next to the mail and the audit trail |
| parish | Keying a new register entry | No screen creates entries yet; the hosted page and the admin page work on entries that exist. The draft is entered beforehand |
| parish | A guardian's consent screen | Granting consent for a child has routes (`/api/me/consents`) but no hosted page. Shown as API calls |
| parish | Transfer screens | An organization transfer has routes and an email but no page. Shown as API calls; the person's new org page is shown |
| parish | Requesting and downloading an export | The Data requests screen lists exports but does not start or download one. The request is made through the route, and the screen lists it |

Other limits worth knowing:

- The authenticator app in the TOTP scene is the rig: it computes the six digit code from the setup key.
- The webhook target is `http://localhost`, which Claviger accepts only with `CLAVIGER_WEBHOOKS_ALLOW_LOCALHOST=true` (set by the rig). Production endpoints must be https.
- The backup scene shows the built-in restore verification, which opens the archive in a scratch directory. It does not restore over the running instance.
- The OIDC device flow is not filmed: the sample app uses the authorization code flow.
- Text-message recovery is not filmed: no SMS sender is configured on a local instance.
- Passkeys are real WebAuthn ceremonies, but against Chromium's virtual authenticator, not a phone.

## Troubleshooting

- A scene fails: open `scenes/NN-<scene>/last-<actor>.png` in the run directory, and `server.log`.
- Port in use: set `WALK_PORT`.
- To record one scene again after fixing it, resume the run: `WALK_RUN_DIR=<run dir> WALK_ONLY=<n> e2e/walkthrough/run.sh`, then stitch with `WALK_STITCH=1`.
