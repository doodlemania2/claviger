# Accessible login pages

Claviger (#83, 83-3) makes its hosted pages usable with large text, high contrast, a keyboard and a screen reader: `/passkeys`, `/orgs/*` and the OpenID Connect pages (`/oidc/login`, the account chooser, consent, device code and approval, signed out, and the error pages). The superuser admin UI (`/_/`) is not covered.

The target is **WCAG 2.2 level AA**. Passkeys also meet the "accessible authentication" criterion (3.3.8), because they need no memorised or transcribed secret.

## Larger text and high contrast

Every hosted page has a button at the top, "Larger text and high contrast" (translated; a real `<button aria-pressed>`). It is hidden until the page script runs, so without script there is no dead button.

- The choice is saved in `localStorage` under `claviger_a11y` and applied by a small script in `<head>` before the first paint (no flash). Every read and write is inside `try`/`catch`: when storage is blocked the page still works and the mode lasts until the page is closed. Two open tabs stay in step.
- In large mode `<html data-a11y="large">` is set: base font 20px, headings larger, all controls at least **44px** tall and wide (24px otherwise), 4px focus outlines, 2px control borders, and a black on white palette (white on black when the system is in dark mode).
- `prefers-contrast: more` gives the same palette without the bigger text, and without any click. `prefers-reduced-motion: reduce` turns off animation and transitions (the pages have almost none).
- Focus is always visible (3px outline), and form controls have a 4.5:1 border (the pages used a 1.4:1 border before, which fails 1.4.11).
- There is a "Skip to the main content" link as the first tab stop, and the page has one `<main>`.

### How it is built

No inline script or style anywhere, so the Content-Security-Policy stays strict:

- `apis/hosted_assets/a11y-mode.css` and `a11y-mode.js`, served at `/a11y/a11y-mode.css` and `/a11y/a11y-mode.js`, are shared by all hosted pages. The CSS only uses the colour variables the pages already define, so one file serves all of them.
- `/passkeys` used `'unsafe-inline'` for script and style. Its script and style are now files (`/passkeys/assets/passkeys-page.js` and `.css`) and its policy no longer has `'unsafe-inline'`. A captcha provider still opens only its own domains, as before.
- Templates call `{{.Text.A11yHead}}` (stylesheet and script) and `{{.Text.A11yBar}}` (skip link and toggle). A new hosted page must call both and have an element with `id="app"` (its `<main>`).

## What was fixed in the WCAG pass

| Page | Fix |
|---|---|
| all | Visible focus; 24px minimum targets (44px in large mode); control borders at 3:1 or better; skip link; `lang` and `dir` were already set on `<html>` (kept) |
| `/passkeys` | One `<main>`; labels tied with `for`/`id` (they wrapped nothing before); heading order h1, h2 (was h1, h3); the three "links" that were `<a href="#">` acting as buttons are `<button>` with `aria-controls` and `aria-expanded`; the error appears next to the form it belongs to, in a live region (`role="alert"` for errors, `status` for confirmations), and the fields get `aria-invalid` and `aria-describedby`; focus moves into the revealed form and, after sign-in, to the register button; `autofocus` removed; the hard-coded English "Signed in." now comes from the catalog; the session no longer breaks when `localStorage` is blocked; hard-coded colours and `opacity` greys replaced by the shared palette |
| `/orgs/*` | Sign-in fields point at the message under them (`aria-describedby`) and get `aria-invalid` on failure; message role is `alert` for errors and `status` for confirmations; table headers have `scope="col"`; the empty actions header has a hidden name; in the member table the role, status and tags fields have names, and every row button is tied to its member, invitation or token (`aria-describedby`), so "Remove" is no longer ten identical buttons |
| `/oidc/*` | Same message and field wiring on the sign-in and device code pages; `role="status"` for the "signed out" message |

## Timeouts

None of these pages has a countdown or an automatic sign-out, so criterion 2.2.1 needs nothing. Recovery codes (83-1, 83-2) expire after 15 minutes. The page that shows or takes a code must say so in the page, and offer a new code instead of failing silently; add that when those pages land.

## Plain language

The English copy of the three page families was reviewed for about a grade 6 reading level. The changes were few, because most of it was already short: passkey wording ("add" instead of "register"), the unsupported-browser message, the authenticator prompt, the device code messages, and "Please sign in again". The Spanish strings were changed to match, and the Spanish pages now use *usted* and Latin American wording throughout (the device and account chooser pages had *tú* and Spain wording).

The other seven locales were **not** touched. Where an English message changed in meaning, those locales still carry the old wording, which is close in meaning. The convention of #97 applies: every non-English catalog is `"_status": "draft"` until a native speaker reviews it, so all of them are draft. The English keys that changed in 83-3 are listed here for the reviewer: `js.totpLabel`, `passkeys.intro`, `passkeys.noPasskeyLink`, `passkeys.lostPrompt`, `passkeys.register`, `js.passkeys.linkOnItsWay`, `js.passkeys.loginFailed`, `js.passkeys.signedInRegister`, `js.passkeys.passwordFailed`, `js.passkeys.namePrompt`, `js.passkeys.registered`, `js.passkeys.registerFailed`, `js.passkeys.none`, `js.passkeys.created`, `js.passkeys.renameFailed`, `js.passkeys.deleteFailed`, `js.passkeys.loadFailed`, `js.passkeys.unsupported`, `js.signin.unsupportedFactor`, `js.signin.passkeyIncomplete`, `oidc.device.badCode`, `oidc.device.inUse`, `oidc.device.warning`, `oidc.err.freshRequired`.

Three new messages were needed in every language (`a11y.toggle`, `a11y.skip`, `orgs.col.actions`). The tests require every key in every catalog, so the seven other languages have a short, plain translation of those three, and they are draft like the rest.

## Tests

- Go: `apis/a11y_pages_test.go` (assets, toggle and skip link on every page in three languages, no inline script or style, labels, wiring, org locale) and the updated golden files.
- Browser: `e2e/tests/95-accessible-mode.spec.ts` toggles the mode on `/passkeys`, an `/orgs` page and an `/oidc` page, checks the attribute, `aria-pressed`, the 20px and 44px sizes, the palette, persistence across a reload, blocked storage, `prefers-contrast` and reduced motion, labels, visible focus, the error wiring, the skip link, and the Spanish and Arabic pages. `@axe-core/playwright` is not a dependency of the suite, so these checks are by hand; adding axe is the obvious next step.

## Still needs a human

- **A screen reader pass** with NVDA or JAWS on Windows, VoiceOver on macOS and iOS, and TalkBack on Android: sign in with a password, a passkey and an emailed code; register and delete a passkey; accept an invitation. Nothing here was run with a real screen reader.
- **An axe or Lighthouse run**, and a check at 200% and 400% zoom (reflow), and in Windows forced-colors mode.
- **A review of the copy by an older user**, as the spec asks. The reading level was judged by eye, not by a tool.
- **A native review of the Spanish strings** (usted, Latin American wording) and of the seven other draft locales, including the three new messages.
- The org admin page (`/orgs/{slug}`) shows its status message at the top of the page, not next to each form, because one status line serves many forms. It is a live region, so it is announced, but it is not next to the field.
- The `confirm()` and `prompt()` dialogs on `/passkeys` and `/orgs/*` are the browser's own, and so are as accessible as the browser makes them. A custom dialog would give more control over wording and focus.
