# Running the OpenID Foundation conformance suite against Claviger

Decision (#71): run the **Basic OP** plan of the OpenID Foundation conformance suite first, and decide on
formal certification after milestone 1.

**Status: the suite has not been run.** What exists is the runbook below, a bootstrap script that builds and
starts a test instance and prints the suite's config (`scripts/oidc-conformance-instance.sh`, also `make
oidc-conformance`), and a manual-only CI job (`.github/workflows/oidc-conformance.yml`) that checks the bootstrap
still works. The suite itself is not in CI, for two reasons that need infrastructure, not code:

1. **The suite must reach the instance over https with a certificate its JVM trusts.** The suite is a Java
   application (a docker compose stack: server, MongoDB, an httpd front end) that fetches
   `https://<issuer>/.well-known/openid-configuration` itself. A runner-local `http://127.0.0.1` instance is not
   reachable from the suite's container, and a self-signed certificate is not trusted by it. A CI run therefore
   needs a throw-away public hostname with a real certificate (for example a temporary ingress on the cluster
   with cert-manager), per run.
2. **The sign-in page needs a browser driver.** The Basic OP tests sign a user in through the OP's pages. The
   suite's built-in automation (`browser` section of the config, HtmlUnit) does not run ES modules, and the hosted
   `/oidc/login` page is a module (`<script type="module">`, no inline script by design). Driving it needs a real
   browser: a small Playwright script that follows the URLs the suite asks a browser to visit and fills the form.
   The outline of that driver is under "A Playwright driver for the sign-in page" below; it has not been run against a suite yet.

The suite's own build is also heavy (a Maven build of the suite, roughly 10 to 20 minutes and a few GB of images),
which is why even once those two are solved it belongs behind `workflow_dispatch` on the `tdp-linux` runner, not on
the k3s pool or in the per-push workflow.

## What to run (manual)

You need a public https hostname that points at the instance, for example `https://oidc-test.example.org`.

1. **Start the instance.** On a machine that hosts the hostname (a reverse proxy such as Caddy or nginx terminates
   TLS and forwards to `127.0.0.1:8090`):

   ```sh
   ISSUER_BASE=https://oidc-test.example.org \
   SUITE_BASE=https://www.certification.openid.net \
   ALIAS=claviger \
   scripts/oidc-conformance-instance.sh
   ```

   It builds the binary, creates a superuser and a test user, sets the application URL, registers two confidential
   first-party clients (the Basic OP plan needs `client` and `client2`) whose redirect URIs are
   `<SUITE_BASE>/test/a/<ALIAS>/callback` and the same with `?dummy1=lorem&dummy2=ipsum`, checks discovery, an
   authorize request and the key set, and prints the suite's test plan config. The config is also written to
   `suite-config.json` in the scratch directory it names. Use `SUITE_BASE=https://localhost.emobix.co.uk:8443` for
   a locally hosted suite.

   Use a **fresh** instance for every run (the script makes one in a new temporary directory): the plan registers
   nothing but expects clean consent and session state.

2. **Pick the suite.**
   - Hosted: <https://www.certification.openid.net/> (sign in, create a plan). Easiest; nothing to build.
   - Local: clone `https://gitlab.com/openid/conformance-suite`, build it (`docker compose -f builder-compose.yml
     run builder`), then `docker compose up`. The suite is at `https://localhost.emobix.co.uk:8443`. The instance
     must be reachable from inside its container, so use a real hostname, not `127.0.0.1`.

3. **Create the plan.** Plan: *OpenID Connect Core: Basic Certification Profile Authorization server test*
   (`oidcc-basic-certification-test-plan`), variants `server_metadata = discovery` and `client_registration =
   static_client`. Paste the printed config (its `server.discoveryUrl`, `client` and `client2`). The `login` block
   in the printed config is for your browser driver; the suite ignores it.

4. **Run the tests and sign in.** For each test the suite shows a URL to visit. Open it in a browser and sign in as
   the test user from the config (`conformance-user@example.com`). The clients are first party, so there is no
   consent page. Several tests expect the OP to **skip** the sign-in on a second visit (single sign-on) or to
   **force** it (`prompt=login`, `max_age`); both are implemented (see `docs/oidc.md`, Single sign-on).

   From the suite repository the plan can also be run from the command line with `scripts/run-test-plan.py`
   (`CONFORMANCE_SERVER=<suite url>`; the plan name and variants as above; the config file as the second
   argument). The exact flags change between suite versions: read the `README`/`.gitlab-ci/run-tests.sh` of the
   version you checked out. This is where a Playwright driver for the sign-in page would plug in.

5. **Record the result.** Export the plan's result (the *Download logs* / *Publish* buttons on the plan page)
   and keep the summary in the issue. A passing run is the evidence for the decision on formal certification.

## Public https instance (a temporary hostname through a tunnel)

The suite's JVM must reach the issuer over https with a certificate it trusts. For a manual run the cheapest way is a
tunnel that terminates TLS with a real certificate and forwards to the instance on `127.0.0.1:8090`. Any of these work;
none of them is run by CI.

```sh
# Cloudflare quick tunnel: a random https://<random>.trycloudflare.com host name, no account
cloudflared tunnel --url http://127.0.0.1:8090
# or: ngrok http 8090            (prints https://<random>.ngrok-free.app)
# or: a temporary ingress / reverse proxy on a host you own, with a real certificate
```

Then start the instance **with that host name as the application URL**, because the issuer is
`<application URL>/oidc` and must match what the suite fetched:

```sh
ISSUER_BASE=https://<random>.trycloudflare.com \
SUITE_BASE=https://www.certification.openid.net \
ALIAS=claviger \
scripts/oidc-conformance-instance.sh
```

Checks before creating the plan: `curl -s https://<random>.trycloudflare.com/oidc/.well-known/openid-configuration`
returns `"issuer":"https://<random>.trycloudflare.com/oidc"`, and opening
`https://<random>.trycloudflare.com/oidc/authorize?...` in a browser lands on the hosted sign-in page. A quick tunnel's
host name changes on every start, so a run is one session: do not restart the tunnel mid-plan (the clients' redirect URIs
point at the suite, not at the tunnel, but the issuer does). The tunnel makes the instance public for the duration:
the test user's password is random and per run, and the instance holds no real data; stop both when done.

## Exact plan and config (suite release v5.3.4, checked 2026-11-09)

Read from the suite repository (`https://gitlab.com/openid/conformance-suite`, tag `release-v5.3.4`,
`.gitlab-ci/run-tests.sh`, `scripts/run-test-plan.py`). Re-check against the tag you deploy; names and variants change.

- Plan: `oidcc-basic-certification-test-plan`
- Variants: `server_metadata=discovery`, `client_registration=static_client`
- As the command line passes it: `oidcc-basic-certification-test-plan[server_metadata=discovery][client_registration=static_client]`
- Related plans that do **not** apply to Claviger by design: `oidcc-implicit-*` and `oidcc-hybrid-*` (no implicit or hybrid
  flow), `oidcc-formpost-*` (no `form_post`). `oidcc-config-certification-test-plan` (discovery document checks, no
  variants) is cheap and worth running too. Dynamic client registration (`client_registration=dynamic_client`) is not
  offered.

Config JSON for the plan (this is what `scripts/oidc-conformance-instance.sh` writes to `suite-config.json`; the values
are placeholders). There is **no `browser` block**, on purpose: the suite's built-in browser automation (HtmlUnit,
configured by that block) does not run ES modules, see below.

```json
{
  "alias": "claviger",
  "description": "Claviger Basic OP",
  "server": {
    "discoveryUrl": "https://<host>/oidc/.well-known/openid-configuration"
  },
  "client": { "client_id": "<client id 1>", "client_secret": "<client secret 1>" },
  "client2": { "client_id": "<client id 2>", "client_secret": "<client secret 2>" },
  "login": { "email": "conformance-user@example.com", "password": "<random per run>" }
}
```

The `login` block is not read by the suite; it carries the credentials for the Playwright driver below. Redirect URIs
registered on both clients: `https://<suite host>/test/a/claviger/callback` and the same with
`?dummy1=lorem&dummy2=ipsum`.

From the suite checkout, the plan can be scheduled with `scripts/run-test-plan.py` (it needs `CONFORMANCE_SERVER` and,
for the hosted suite, `CONFORMANCE_TOKEN`; `--export-dir` saves the results). Its only automation of the sign-in is the
`browser` config block, so a run from the script without a driver stops at the first test that needs a sign-in. Use the
suite's web UI plus the driver, or extend the script.

## A Playwright driver for the sign-in page

The suite shows, for every running test, the URLs a browser has to visit. The driver replaces the human: it visits them
in Chromium, signs in on `/oidc/login`, and lets the redirects return to the suite.

**Status: design, not run against a suite.** The Claviger half (the sign-in) is the same sequence the repository's own
e2e spec runs (`e2e/tests/70-oidc.spec.ts`). The suite half (how the driver learns the URLs) must be checked against the
suite version: the points marked *to verify* are from reading the API names, not from a run.

Outline:

1. Create the plan in the suite UI (or `POST /api/plan?planName=...&variant=...` with the config), and note its id.
2. For each module, start it (`POST /api/runner/{moduleId}` *to verify*) and poll `GET /api/info/{moduleId}` until the
   status is `WAITING` and the `browser` section lists URLs to visit (*to verify the field names*; the suite's own UI
   reads the same data).
3. For each URL: new browser context (so the module starts with no single sign-on cookie, except where the module
   needs one: `oidcc-prompt-none-*`, `oidcc-max-age-*`, `oidcc-ensure-registered-*` re-use the first visit's context, see
   the test descriptions), `page.goto(url)`.
4. If the page becomes `/oidc/login?authRequestID=...`: fill and submit. The form is revealed by a module script, so
   wait for the field, do not assume it is in the HTML:

   ```ts
   await page.getByLabel("Email or username").fill(login.email);
   await page.getByLabel("Password").fill(login.password);
   await page.getByRole("button", { name: "Sign in", exact: true }).click();
   ```

5. Wait for the browser to arrive on the suite's callback (`/test/a/<alias>/callback...`); the suite page that loads
   there reports the result back (for the implicit-style fragment pages it runs a script, which is why a real browser is
   the point of this driver). A test that expects the OP to show no page (`prompt=none`) ends on the callback with an
   `error`; one that expects an error page (a refused `request` object) ends on Claviger's error page: both are fine, the
   suite judges the outcome, the driver only has to finish navigating and capture a screenshot.
6. Poll `GET /api/info/{moduleId}` until the status is `FINISHED`; collect `result` (`PASSED`, `WARNING`, `FAILED`,
   `REVIEW`, `SKIPPED`), screenshot the failures, go to the next module. Put the results in an
   `expected-failures` file (see the CI section) as they are triaged.

Some modules need a human or a different shape of visit (the "Review" ones ask for a screenshot to be uploaded to the
suite: the driver's screenshot is what to upload, by hand). A module that never reaches `WAITING` within the suite's
time-out is a failure of the instance to answer, not of the driver.

## Results template

```
OpenID conformance run
Date:                       Tester:
Claviger version/commit:
Suite: hosted [ ]  local [ ]   version/tag:
Issuer (redacted):          Tunnel/host kind:
Plan: oidcc-basic-certification-test-plan[server_metadata=discovery][client_registration=static_client]
Also run: oidcc-config-certification-test-plan [ ]
Sign-in driven by: human [ ]  Playwright driver [ ]  (driver commit:)

Totals:   passed __   warning __   failed __   review __   skipped __
Failures and warnings, one per line (module, suite message, triage: Claviger bug / test expectation / known gap):
  
Known gaps hit (request objects, claims parameter, acr_values, form_post, ...):
Modules needing a manual screenshot (uploaded: yes/no):
Bugs filed (issue links):
Plan result exported (Download logs / Publish): yes [ ]  location:
Decision input for formal certification:
Clean-up: tunnel stopped [ ]  instance and data dir removed [ ]  suite plan kept/deleted [ ]
```

## What to expect

Claviger follows the Basic OP profile (authorization code flow, `client_secret_basic` and `client_secret_post`,
discovery, JWKS, UserInfo, RS256 ID tokens, `nonce`, `prompt`, `max_age`, `id_token_hint`, `auth_time`, `acr`).
Known areas where the first run will probably report warnings or failures, to triage rather than to fix blindly
(none of this has been observed; it is from reading the plan against the implementation):

- **Request objects and the `claims` parameter** are not supported (discovery says
  `request_parameter_supported`/`request_uri_parameter_supported`/`claims_parameter_supported` are false). Tests
  that require them belong to other plans or are optional; a test that sends `request=` should be refused with
  `request_not_supported`, which is the correct answer.
- **`acr_values` and `ui_locales`** are ignored. `acr` is always asserted (`urn:claviger:acr:*`).
- **`response_mode`** other than `query` is not offered.
- **Consent.** The registered clients are first party. A plan run against a client that is not first party would
  stop at the consent page.
- **Refresh tokens** are rotated on every use and a reused one revokes the chain; the suite's refresh test should
  pass, a test that replays an old refresh token expects exactly that failure.
- **Rate limits.** The protocol endpoints are rate limited per IP (see `docs/oidc.md`). A suite run from one
  address stays far below the limits; if a run is refused with `429`, `temporarily_unavailable`, look for a test
  loop before suspecting the limiter.

## CI

`.github/workflows/oidc-conformance.yml` is `workflow_dispatch` only. It runs `scripts/oidc-conformance-instance.sh
--check`: build, start, register the two clients, verify discovery, one authorize request and the JWKS against the
real binary, stop. It does **not** run the suite. When the two blockers above are solved (a throw-away https
hostname per run, and a browser driver), the next step is to extend that job to start the suite with docker compose,
run `run-test-plan.py`, and fail on anything outside an `expected-failures` file kept in the repository.
