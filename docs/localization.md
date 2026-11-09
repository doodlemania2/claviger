# Localization

Claviger (#97) shows its hosted pages and built-in emails in the language of the person looking at them. Out of the box it ships English and eight more languages. The superuser admin UI (`/_/`) stays English.

With nothing configured, an English install renders exactly the text and markup it did before localization. The only change to the HTML is one inline `application/json` data block in the head of pages that run a script (see "How the strings reach the page scripts").

## What is localized

| Surface | Where |
|---|---|
| `/passkeys` self-service page | `apis/passkeys_page.go` |
| OpenID Connect sign-in, consent, account chooser, device-flow, signed-out and error pages | `oidc/` |
| Organization pages: invitation accept page, `/orgs/<slug>` admin page | `apis/orgs_pages*` |
| Auth emails: verification, password reset, email change, one-time password, new-location login alert, passkey reset | `mails/` |
| The organization invitation email | `mails/invitation.go` |

Not localized: the superuser admin UI, API error messages (`{"message": ...}` of the REST API), OAuth protocol errors sent to client applications, log lines, and the names of roles in the data (`owner`, `admin`, ...; their display names are localized, the stored values are not).

## Languages and translation status

| Tag | Language | Direction | Status |
|---|---|---|---|
| `en` | English | ltr | source, reviewed |
| `es` | Español | ltr | **draft** (machine translation) |
| `pt-BR` | Português (Brasil) | ltr | **draft** |
| `fr` | Français | ltr | **draft** |
| `tl` | Tagalog | ltr | **draft** |
| `vi` | Tiếng Việt | ltr | **draft** |
| `ko` | 한국어 | ltr | **draft** |
| `zh-Hans` | 简体中文 | ltr | **draft** |
| `ar` | العربية | **rtl** | **draft** |

Every non-English file carries `"_status": "draft"`. Drafts are complete (a test fails on a missing key, a lost placeholder or changed markup) but were written without a native reviewer, so wording may be stiff or off, particularly short UI labels, role names and the terms for "passkey", "token" and "tenant". Treat them as a starting point and ask a native speaker to review before you rely on them for a production church or organization. Change the status to `"reviewed"` when someone has. Chinese is Simplified; any `zh-*` request (including Traditional, `zh-TW`) is served `zh-Hans` until a Traditional catalog exists.

## How the language is chosen

For a hosted page, the first of these that names an available language wins:

1. `?lang=xx` in the URL. A valid choice is remembered in a `lang` cookie (HttpOnly, one year, path `/`) so the next page keeps it.
2. the `lang` cookie.
3. the preferred language of the account the page is about, when the page knows it (the OIDC consent and device approval pages, the invitation page for an existing account). It is read from a field of the auth collection, named `lang` by default.
4. the `Accept-Language` header, in quality order.
5. `meta.defaultLanguage` (default `en`).

An unsupported value is skipped, not an error: `?lang=xx` falls through to the next source. A region falls back to its language (`pt-PT` is served by `pt-BR`, `es-MX` by `es`), and message lookups fall back along `pt-BR`, `pt`, `en`, so a catalog may be incomplete.

For an email there is no browser, so it is the account's preferred language, then `meta.defaultLanguage`. The invitation email goes to the invited address, so it uses that address's account when one exists.

### Settings

`meta.defaultLanguage` (a tag such as `en` or `pt-BR`) and `meta.languageField` (the name of the auth collection field with the preferred language, default `lang`) are part of the settings (`PATCH /api/settings`, `{"meta": {...}}`). To let members choose, add a text (or select) field named `lang` to the auth collection, with values such as `es` or `pt-BR`. The REST API already lets a user update their own record, so a profile page can offer a language picker.

## How to add a language

1. Copy `tools/i18n/locales/en.json` to `tools/i18n/locales/<tag>.json` (for example `de.json`, `pt-PT.json`).
2. Add `"_name"` (the language's own name) and `"_status": "draft"`, and translate every value. Keep:
   - `{placeholders}` exactly (`{client}`, `{org}`, `{APP_NAME}`, `{TOKEN}`, ...). Move them where your grammar needs them, never rename them.
   - HTML tags and attributes exactly. Keys ending in `Html` and all `email.*` keys are HTML fragments; everything else is plain text and is escaped for you.
   - Keys starting with `js.` are the strings page scripts need; they are all sent to the browser.
3. Add the tag to the `shipped` list in `tools/i18n/catalog_test.go`. `go test ./tools/i18n/` then checks that no key is missing or extra, the placeholder set of every message matches English, and the markup matches.
4. Text direction comes from the language code (`ar`, `he`, `fa`, `ur`, `ps`, `sd`, `yi`, `dv`, `ug` are right-to-left); nothing else to configure.

Plural forms use CLDR categories: add `key.one`, `key.other` (and `zero`, `two`, `few`, `many` where the language has them) and read them with `Bundle.Plural(tag, "key", n)`. The catalog has no plural message yet; the engine and its rules (en, es, pt, fr, tl, ar, and "other" only for vi, ko, zh) are tested for when one is needed.

## How to override strings without rebuilding

Put JSON files in `<pb_data>/i18n/`, one per tag, in the same format. They are read once at the first use and merged over the shipped catalogs key by key, so a file may hold only the messages you want to change:

```json
// pb_data/i18n/es.json
{ "js.signIn": "Entrar", "email.team": "El equipo de {APP_NAME}" }
```

A file for a tag that is not shipped adds that language (`pb_data/i18n/de.json` with `"_name": "Deutsch"`; it falls back to English for missing keys). Restart to pick up a change. Embedding Go code can also call `core.I18n(app).Override(tag, map[string]string{...})`.

## Emails

The collection's email templates (Collections, Auth options, in the admin UI) stay the admin's override. A template that is still exactly the English default that new collections are created with is replaced, at send time, by the catalog version in the recipient's language. Editing a template, even by a character, turns localization off for that template: your text is sent as written, in every language. An organization's own invitation template (`settings.invitationEmail`) works the same way, per part (subject and body).

Placeholders (`{APP_NAME}`, `{TOKEN}`, `{RECORD:name}`, `{ORG_NAME}`, ...) work in the translated messages as in the English ones. For right-to-left languages the body is wrapped in `<div lang="ar" dir="rtl">`; other non-English bodies in `<div lang="xx">`. Emails are HTML only, as before (no plain-text part).

The expiry date in the invitation email is "2 January 2006" in English and `2006-01-02` in other languages: month names are not in the catalog and a numeric date is unambiguous.

## How the strings reach the page scripts

The hosted pages run under a strict Content-Security-Policy (no inline script). The text the page scripts need (`js.*` keys: error messages, button labels they create, confirmations) is sent in the HTML as

```html
<script type="application/json" id="i18n">{"js.tooMany":"..."}</script>
```

A JSON block is data, not script: browsers never execute it and `script-src 'self'` does not apply to it, so the policy is unchanged in every language (a test asserts this). The page scripts read it with `JSON.parse(document.getElementById("i18n").textContent)`. The `/passkeys` page, which already allows inline script by design, does the same.

## Right-to-left

Pages set `<html lang="ar" dir="rtl">` (nothing extra for left-to-right languages). The page styles use logical properties only (`margin-inline-end`, `text-align: start`, flex layout), so they mirror by themselves; a test rejects physical `left`/`right` in `apis/orgs_pages/hosted.css`. If you add CSS, use logical properties. Email addresses, URLs and app names inside a right-to-left sentence are Latin-script values: the translations place them so the sentence reads correctly, and `<bdi>` can be added around a placeholder in a message if a particular value renders badly.

## Notes for developers

- Messages: `core.I18n(app)` returns the app's `*i18n.Bundle` (`tools/i18n`). `apis.NewPageText(e, recordLang)` negotiates the language for a request and renders messages (`T`, `HTML`, `DataBlock`).
- Keep English identical: the golden files in `apis/testdata/i18n_golden/`, `oidc/testdata/i18n_golden/` and `mails/testdata/i18n_golden/` hold the pre-localization output; the tests compare English to them (minus the data block). Do not regenerate them to make a test pass.
- New strings go to `en.json` first; reference them by key in templates and scripts. A key used in code but missing in English, or in English but unused, fails `TestKeysInUseAreInTheCatalog`.
