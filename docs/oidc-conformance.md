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
   That script does not exist yet.

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
