# Claviger brand guide

Claviger is an open-source backend with passkeys built in. It is built on PocketBase, runs as one
file, and keeps identity at its centre. This guide covers the name, the logo, colour, type, voice and
the copy other sites should use.

Everything here is hand-drawn SVG or plain text. The logo generator (geometry for the keys and the
wordmark) lives outside the repo; the SVGs in `logo/` are the source of truth.

## Name

- Write **Claviger** in prose, with a capital C. The wordmark is lowercase: **claviger**.
- Say **KLAV-ih-jer**. Church Latin speakers say **KLAH-vee-jer**, and that is fine too.
- It is Latin for **key-bearer**: the one who holds the keys and decides who comes in.
- Don't write CLAVIGER, ClaviGer or Claviger.app in prose. Use the domain only as a link.
- When you mention the code it came from, say "built on PocketBase". Never use the PocketBase logo.

## The story, in three sentences

A key-bearer holds the keys and opens the door to the people who belong. The crossed keys, one gold
and one silver, echo the keys of St. Peter, a sign of trust that many Christian traditions share.
Claviger does the same job in software: it holds your members' keys and opens the right doors.

Use the keys as a sign of trust and access. Do not pair them with sacred vessels, the Eucharist, a
tiara or the papal arms. The mark is for congregations of any tradition and for developers with no
religious connection at all.

## Logo

| File | Use |
|---|---|
| `logo/mark.svg`, `logo/mark-dark.svg` | The crossed keys, 32 px and up. `-dark` is for dark backgrounds. |
| `logo/mark-small.svg`, `logo/mark-small-dark.svg` | A bolder cut for 20–32 px. |
| `logo/favicon.svg`, `logo/app-icon.svg` | The bold keys on a vesper tile. Browser tabs, app icons, 16 px and up. |
| `logo/mark-mono.svg` | One colour (vesper), for stamps, embossing and single-colour print. |
| `logo/wordmark.svg`, `logo/wordmark-dark.svg` | The name alone. |
| `logo/lockup-horizontal.svg`, `logo/lockup-horizontal-dark.svg` | Mark plus wordmark. The default for headers. |
| `social-card.svg`, `social-card.png` | 1200 × 630 share image (Open Graph / Twitter card). |
| `previews/` | PNG renders of all of the above, for reviewing without a browser. |

The gold key lies in front, leaning right. The silver key lies behind it, mirrored. A thin gap cuts the
silver key where they cross, so the overlap reads at every size. The wordmark is drawn as a single
round-ended line on a geometric skeleton; the dot of the **i** is gold, the only colour in it.

**Clear space.** Keep empty space around the logo equal to the diameter of a key's bow (about a
quarter of the mark's height) on every side.

**Minimum size.** Full mark: 32 px tall. Below that use `mark-small` (down to 20 px) or the favicon
(16 px). Wordmark: 72 px wide. Lockup: 120 px wide.

**Don't**

- recolour the keys, swap gold and silver, or make both the same metal (use `mark-mono` instead);
- rotate, skew, outline or add shadows or gradients to the mark;
- set the wordmark in a font: always use the SVG;
- put the colour mark on a busy photo or on a mid-tone background where gold falls below 3:1;
- add a cross, tiara, chalice or any other religious symbol to the mark.

## Colour

Light and dark are equal citizens. The ratios below are WCAG 2.x contrast against the page background
(**bg**) and card **surface**; AA needs 4.5:1 for body text and 3:1 for large text and graphics.

### Light

| Token | Hex | Use | vs bg | vs surface |
|---|---|---|---|---|
| `bg` | `#FAF7F0` | Parchment page | | |
| `band` | `#F2EEE3` | Alternate sections | | |
| `surface` | `#FFFFFF` | Cards, inputs | | |
| `line` | `#E3DED0` | Borders | | |
| `ink` | `#1B2540` | Vesper. Text, headings, buttons | 14.17 | 15.16 |
| `muted` | `#4A556B` | Secondary text | 7.00 | 7.49 |
| `link` | `#2F4F8F` | Lapis. Links | 7.45 | 7.97 |
| `gold-text` | `#86621A` | Gold for text, eyebrows, labels | 5.20 | 5.56 |
| `gold` | `#B8862B` | Graphics only (logo, rules, icons) | 3.03 | 3.24 |
| `silver` | `#7A8595` | Graphics only | 3.49 | 3.74 |
| button | `#FAF7F0` on `#1B2540` | Primary button | 14.17 | |

### Dark

| Token | Hex | Use | vs bg | vs surface |
|---|---|---|---|---|
| `bg` | `#0F1522` | Night vesper | | |
| `band` | `#131B2B` | Alternate sections | | |
| `surface` | `#172033` | Cards | | |
| `line` | `#263048` | Borders | | |
| `ink` | `#ECEEF3` | Text | 15.72 | 14.01 |
| `muted` | `#A9B1C2` | Secondary text | 8.48 | 7.56 |
| `link` | `#9DB6F0` | Links | 9.02 | 8.04 |
| `gold` / `gold-text` | `#E3B85C` | Logo and gold text | 9.80 | 8.74 |
| `silver` | `#C2C9D3` | Logo | 10.94 | 9.75 |
| button | `#1B2540` on `#E3B85C` | Primary button | 8.14 | |

Gold `#B8862B` is a graphic colour on light backgrounds. Never set text in it; use `gold-text`. It
drops to 2.79:1 on `band`, so put the colour mark on `bg` or `surface`, not on `band`.

Machine-readable tokens: `tokens.json` (with ratios) and `tokens.css` (custom properties, light,
dark by `prefers-color-scheme`, and a `data-theme` override).

## Type

| Role | Typeface | Fallback |
|---|---|---|
| Display (headings, pull quotes) | **Newsreader** 400–600, optical sizes on | Georgia, "Times New Roman", serif |
| Body and UI | **Public Sans** 400–700 | system-ui, -apple-system, "Segoe UI", Roboto, sans-serif |
| Code | **JetBrains Mono** 400/600 | ui-monospace, SFMono-Regular, Menlo, Consolas, monospace |

```html
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Newsreader:opsz,wght@6..72,400..700&family=Public+Sans:wght@400..700&family=JetBrains+Mono:wght@400;600&display=swap">
```

Newsreader gives headings a calm, bookish warmth; Public Sans keeps body text plain and legible, and
matches The Doodle Project's body face. Headings use weight 500, slight negative tracking, and
sentence case. Eyebrow labels are Public Sans 600, uppercase, letter-spaced 0.12em, in `gold-text`.

## Voice and tone

Calm, plain and warm. We sound like a careful parish administrator who also writes good code: we say
what the software does, what it does not do yet, and how to get help. Short sentences. Active voice.

| Do | Don't |
|---|---|
| "Members sign in with their phone's fingerprint or face check." | "Revolutionary biometric authentication experience." |
| "Households are on the roadmap. They are not built yet." | "Coming soon: next-gen family management!" |
| "Your data stays on your server." | "Enterprise-grade, military-grade security." |
| "Built on PocketBase by Gani Georgiev." | (Leaving out where the code came from.) |
| "For any church, synagogue, mosque or faith community." | "The Catholic auth platform." |

Words we don't use: seamless, powerful, robust, revolutionary, blazing, effortless, next-gen,
military-grade, game-changing. (The Doodle Project site's check fails on several of these too.)

Label anything unbuilt as **Roadmap**, every time it appears. Label anything built but broken as
**Being fixed**.

## Taglines

1. **Open the right doors.** *(recommended)*
2. Keys for the people you serve.
3. Your members' keys, kept close.
4. Sign-in your congregation can trust.
5. Hold the keys yourself.

**Open the right doors** says what an identity system does, works for developers and congregations
alike, and carries the key-bearer image without leaning on religious language. (Avoid "Hold the
keys." on its own: Ostiary uses it.)

## Elevator pitches

**10 words.** An open-source backend with passkeys, for apps and congregations.

**30 words.** Claviger is an open-source backend with passkeys built in. It runs as one file on your
own server, stores your data there, and is growing the identity features congregations need.

**100 words.** Claviger is a free, open-source backend built on PocketBase. One file gives you a
realtime database, file storage, an admin screen and an API, with passkey sign-in next to password,
OAuth2 and one-time codes. It runs on your own server, so members' data stays where you put it, and
there are no per-user fees. Next on the roadmap are the identity features congregations keep asking
for: households with guardians, children's check-in, access that ends when safeguarding training
lapses, recovery the parish office can vouch for, and one sign-in across a diocese. The PocketBase
SDKs keep working.

## Positioning

| | Claviger | Auth0 | Ostiary | Planning Center |
|---|---|---|---|---|
| What it is | Backend + identity, one binary | Hosted identity service | Self-hosted identity server (Better Auth, Postgres) | Hosted church management suite |
| Who it is for | Developers and congregations | Companies | Developers | Churches |
| Where data lives | Your server | Their cloud | Your Postgres | Their cloud |
| Price | Free (MIT) | Per monthly active user | Free (MIT) | Subscription per product |
| Our angle | Identity that understands households, safeguarding and pastoral confidentiality — on infrastructure a parish can afford | | | |

- **Against Auth0:** you own it, there are no per-user fees, and it ships the database too.
- **Against Ostiary:** one binary with the database built in, no Postgres to run, and a roadmap
  shaped by how congregations actually work. Ostiary is ahead on OIDC provider features today; say
  so if asked.
- **Against Planning Center:** we don't replace a church management system. We are the identity and
  data layer underneath one, and we will sign members in to Planning Center and others (roadmap).

Status as of October 2026: passkeys, OpenTelemetry log export and the container handover ship today.
Passkey reset is built but has a known bug being fixed. OIDC, households, check-in,
safeguarding-based access, staff-vouched recovery, diocese-wide identity, audit log and encryption are
roadmap.

## For the TDP public site

Claviger replaces the "PocketBase fork" entry in the open-source list (`OPEN` in
`src/lib/products.mjs`). The site states only what a product does today, so this entry leaves the
roadmap out.

```js
  {
    id: 'claviger',
    name: 'Claviger',
    upstream: 'PocketBase',
    license: 'MIT',
    spot: 'keys',
    href: 'https://claviger.app',
    tagline: 'Open the right doors.',
    text: 'Claviger is our open source backend, built on PocketBase. It adds passkey sign-in next to password, OAuth2 and one-time codes. It sends warnings and errors to your own OpenTelemetry collector, and during a rolling deploy the new copy waits for the old one to let go of the database.',
    gives: 'Everything is public under the MIT licence. We still follow each PocketBase release and keep its API, so the PocketBase SDKs keep working.',
  },
```

https://claviger.app is live (2026-10-31), so link there; the source is
`https://github.com/doodlemania2/claviger`. GitHub redirects the old `doodlemania2/pocketbase`
address.

A `keys` spot for `src/lib/art.mjs`, drawn in the site's own classes (200 × 160) so it takes the TDP
palette rather than Claviger's:

```js
  keys: `
<g transform="translate(100 84) scale(1.25)"><g transform="translate(0 -7)">
<path class="l-fill" transform="rotate(-45) scale(-1 1) translate(0 9)" d="M-4 -55H4V22H-4ZM-8.5 14h17v4.5h-17ZM3.5 -55H21V-50H16V-45H21V-42H16V-37H21V-32H3.5ZM0 21a16 16 0 1 1 0 32a16 16 0 1 1 0 -32ZM0 30.4a6.6 6.6 0 1 0 0 13.2a6.6 6.6 0 1 0 0 -13.2Z"/>
<path class="g-fill" transform="rotate(45) translate(0 9)" d="M-4 -55H4V22H-4ZM-8.5 14h17v4.5h-17ZM3.5 -55H21V-50H16V-45H21V-42H16V-37H21V-32H3.5ZM0 21a16 16 0 1 1 0 32a16 16 0 1 1 0 -32ZM0 30.4a6.6 6.6 0 1 0 0 13.2a6.6 6.6 0 1 0 0 -13.2Z"/>
</g></g>
${sparkle(172, 30, 10)}`,
```

If the site later wants Claviger's own colours (for example on a product page), use
`logo/mark.svg` / `logo/mark-dark.svg` and the `social-card.png` as the share image. The TDP
Content-Security-Policy forbids inline styles and outside files, so inline the SVG paths and colour
them with classes, as above.

## Website

The public site, https://claviger.app, lives in its own private repository,
`doodlemania2/claviger-site` (static pages behind nginx on doodlessd). It uses the tokens above and
self-hosts the three typefaces, because its Content-Security-Policy allows no outside files. Change
the site there, and keep its palette in step with `tokens.css`.

## Credit

Claviger is built on [PocketBase](https://pocketbase.io) by Gani Georgiev, under the MIT licence. Keep
his copyright notice in the licence file and say "built on PocketBase" wherever the product is
described at length.
