# Screenshot kit for the Claviger documentation site

This kit takes every screenshot of the public docs (the Docusaurus site in the `claviger-site`
repository). It is modelled on Antiphona's guide kit. It has three parts:

| File | What it is |
|---|---|
| `world.ts` | Starts the real binary and seeds an **invented** congregation through the API. |
| `screens.json` | The manifest: every admin UI page and hosted page, with its named states. |
| `capture.mjs` | Runs the world, visits each screen and state, writes PNGs. |
| `guides.spec.ts` | Playwright project `guides`: the manifest validates, and a 3-screen capture works. |

It needs Node 22.18 or newer (the files are TypeScript and ESM run by type stripping; the project
asks for Node 24) and the Playwright browser of `e2e/` (`cd e2e && npm ci && npx playwright install chromium`).
The binary is `e2e/.bin/claviger`; it is built when missing (`go build ./examples/base`, after `npm run build` in `ui/` if you changed the UI).

## Run it

```bash
cd e2e
alias capture='node --disable-warning=ExperimentalWarning --disable-warning=MODULE_TYPELESS_PACKAGE_JSON guides/capture.mjs'

capture --list                      # every id and state, no browser
capture --check                     # validate the manifest (ids, audiences, routes against ui/src/router.js)
capture --check --live              # also start the world and prove every route answers or opens in the SPA
capture --only admin-webhooks       # one screen (all its states), or one state: --only admin-webhooks--new
capture --out /tmp/shots            # everything, about 5 minutes
capture --specs ../../claviger-site/docs-site/guides   # capture what the docs-site specs declare in `shots:`
```

Flags: `--only <id[--state]>` (repeatable), `--out <dir>`, `--specs <dir>`, `--jobs <n>` (pages at once, default 4),
`--no-dark`, `--no-mobile`, `--attach <world.json>` (reuse a world that `node world.ts --keep` left running; a dev aid).

**Where it writes.** `--out <dir>`, else `$CLAVIGER_GUIDES_DIR/guides`, else `claviger-site/docs-site/guides` next to the
primary checkout (`../../claviger-site/docs-site/guides` counted from the primary's `e2e/`). The last one only exists
when you mean to write into the docs site: with no `claviger-site/docs-site` the runner stops. Tests and experiments always pass `--out`.

**Output names.** `<out>/<area>/img/<id>[--<state>].png`, with `-dark` before the extension for the dark theme and `-mobile`
for the 390x844 capture of a hosted page. Desktop is 1440x900 in light and dark (`prefers-color-scheme`). Hosted pages are
captured full page; admin pages are one viewport, and states that scroll to a section crop to it.

**Explore the world** in a browser: `node --disable-warning=ExperimentalWarning --disable-warning=MODULE_TYPELESS_PACKAGE_JSON guides/world.ts --keep`
prints the URL and the operator sign-in (`operator@example.org`, every demo password is `demo-pass-1234`).

The runner picks free ports, so it never meets the e2e suite (port 18090) or another session.

**A prebuilt binary.** `CLAVIGER_GUIDES_BIN=<path>` runs that binary and skips the build (a CI job that already built one).

## The safety scan

Every shot is scanned before it is written (`scan.mjs`, the fleet presence standard's requirement 2). The kit reads the
visible text of the page and the values of its inputs (never a password field), and a shot fails when it holds:

| Rule | Fails on |
|---|---|
| `email` | an address whose domain is not `example.org`, `example.com` or `example.net` (or a subdomain of one) |
| `phone` | a phone number outside 555-0100 to 555-0199 (any country or area code, any separators) |
| `hostname` | a production or internal name: `claviger.app` and its subdomains, `thedoodleproject.com`, `stfoa`, `doodlessd`, `doodlemanage`, `*.ts.net`, `tailscale`, `derekmartin` |
| `deny` | a substring of `deny.json` (`{"substrings": [...]}`, case-insensitive; `--deny <file>` reads another) |
| `error` | an error state: "Something went wrong", "Try again", "unexpected error" or "failed to load" in the page's prose (code samples are not prose), or any visible error panel (`[role=alert]`, `.msg.err`, `.alert.danger`, an error toast), empty or not |

A screen or state that shows an error on purpose (the sign-in error page, an expired invitation) says why with
`"showsError": "<reason>"` in `screens.json`, and the `error` rule skips it; `--check` refuses an empty reason. Every
other broken screen fails, so a page that errored while it was captured never ships as a picture.

A shot that fails is **not written**. The run lists each failure and exits 1. Every capture run writes
`<out>/scan-report.json`:

```json
{ "checked": 350, "failures": [{ "screen": "admin-webhooks", "variant": "light", "rule": "deny", "match": "Webhooks" }] }
```

`checked` counts the shots that reached the scan. The docs site uses recaptured pictures only when that file exists,
`failures` is empty and `checked` is above 0. So the world's data stays invented: emails on the `example.*` domains and
phone numbers in 555-01xx.

## How a docs-site spec references a screen

A spec lists its shots in the front matter (Antiphona's `shots:` block, with no click steps):

```yaml
shots:
  - out: webhooks-new.png
    screen: admin-webhooks--new      # an id, or id--state
    persona: superuser
    path: /_/#/webhooks              # optional: the manifest route is the default
```

**Flow screens.** An entry marked `"flow": true` in `screens.json` needs something the world generates for each run: an
authorize request (`hosted-oidc-login`, `-consent`, `-chooser`, `-error`), a device code (`hosted-oidc-device`,
`-device-approve`), an invitation token (`hosted-orgs-invite`) or an erasure token (`hosted-erasure-confirm`, `-cancel`).
For these the kit runs the manifest's own recipe and **ignores the spec's `path`**: it is informational only, and
`--check --specs` prints a warning when a spec gives one. A persona with no account (`invitee`) means "not signed in".

`capture --specs <docs-site/guides>` reads `<area>/*.md`, parses the front matter, and writes `<area>/img/webhooks-new.png`,
`webhooks-new-dark.png` (and `-mobile.png` for a hosted page). The kit reaches the state by itself, from the manifest
(route plus the steps of that state) and the persona; a spec never says how. `capture --check --specs <dir>` fails on an unknown screen or persona.

## Ids, audiences and personas

- Admin UI pages: `admin-<route>` (`admin-collections`, `admin-orgs`, `admin-settings-mail`); data protection: `admin-gdpr-<page>`;
  hosted pages: `hosted-<page>`. A dialog or variant is a **state** of its screen (`admin-orgs--invite`).
- `audience` is one of `developers`, `superusers`, `staff`, `members`.
- `area` is the folder under `guides/` the images go to (`collections`, `settings`, `organizations`, `congregation`,
  `identity`, `integrations`, `data-protection`, `hosted`, `audit`, `logs`, `admin`).
- Personas (the world seeds them; the sign-in is injected, so no screen shows a login unless it is the point):

| Persona | Who | Role |
|---|---|---|
| `superuser` | operator@example.org | the instance operator (admin UI). Holds a passkey. |
| `org-owner` | Maeve Okafor | owner of St. Brendan's Parish, tag `safeguarding` |
| `coordinator` | Brigid Halloran | org admin, credentials coordinator |
| `registrar` | Tomas Brennan | tag `registrar`. Holds a passkey, so finalizing works. |
| `org-admin` | Brigid Halloran | alias of `coordinator` |
| `invitee` | Ciaran Doyle | has a pending invitation to St. Brendan's, no account yet (the invite page) |
| `greeter` | Aoife Rourke | tag `greeter`, no household |
| `clergy` | Fr. Anselm Whitcombe | tags `clergy`, `breakglass`. Holds a passkey. |
| `care-team` | Priya Ellington | tags `care-team`, `breakglass`; holds credentials |
| `guardian` | Dara Calloway | guardian of the Calloway household (child: Wren) |
| `member` | Hal Fenwick | adult member, own household |

The world also holds people for particular screens: Quillon Marsh (an erasure waiting for confirmation), Elowen Tate (an
erasure in its grace period), Lowell Pennington (erased by staff, under a legal hold), Corwin Ashby (banned) and Seren Doyle (a viewer).

## Add a screen

1. Find the route (`ui/src/router.js` for the admin UI; the `se.Router.GET` calls in `apis/` and `oidc/` for hosted pages).
2. Add an entry to `screens.json`: `id`, `area`, `route`, `audience`, `title`, `states`. A route may hold placeholders
   (`{orgSlug}`, `{inviteToken}`, `{oidcLogin}`): the world defines them in `vars` or `lazy` (`world.ts`). Hosted pages
   take a `persona` (or `"anon": true`).
3. A state is `{ "name": "...", "steps": [...] }`. Steps: `click`, `fill [selector, value]`, `select [selector, value]`,
   `check`, `press`, `hover`, `wait` (a selector), `scroll`, `top` (scroll a section to the top of the viewport),
   `pause` (ms), `goto` (a route). Selectors are Playwright selectors. A state must **open** things, never save them. The
   exception is a state that shows a once-only secret: flag it `"mutates": true` and it runs last, one at a time, and `mask` the secret.
4. State options: `mask` (selectors painted over), `fullPage`, `dark: false`, `mobile: false`, `persona`, `anon`, `passkey`, `allowStatus`.
5. `capture --check`, then `capture --only <id> --out <scratch dir>` and look at the images.

The admin UI's Content-Security-Policy forbids inline script and style, so the runner never injects a `<style>`. It
changes text and input values only (`normalize()` in `capture.mjs`) and uses Playwright's `mask` for what must not show.

## Stable pictures

The world uses fixed names, and `capture.mjs` makes the volatile bits stable: a date or time made by the run (today up to a
year ahead: stamps, the end of a grace period) becomes the same moment (14 March 2026, 09:30), record ids become
`demo001xxxxxxxx`, `localhost:<port>` becomes `claviger.example.org`, and a device code is replaced. Browser time zone is UTC, locale en-GB, motion off.
Requests to any other host are aborted: nothing leaves the machine, and a redirect URI or webhook URL on `example.org` is never visited.

## The invented-data rule

**Nothing in the world is real.** Every person, parish and diocese is made up (St. Brendan's Parish in the Diocese of
Harrowmere), every address is on `example.org` or `example.com`, and there is no real domain, tenant or congregation: no STFoA,
nothing from a deployment. Do not paste real data into `world.ts` and do not point the kit at a production instance. The
data directory is a temp directory that is deleted when the run ends. The settings key and the KEK in `world.ts` are
public demo values that protect nothing.
