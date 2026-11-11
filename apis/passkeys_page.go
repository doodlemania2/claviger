package apis

import (
	"html/template"
	"net/http"
	"regexp"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

// bindPasskeysPage registers the self-service WebAuthn / passkey routes:
//
//   - GET /api/passkeys-client.js  → reusable ES module helper for other apps.
//   - GET /passkeys                → minimal self-service HTML page powered by the helper.
//
// The page lets end-users sign in with a registered passkey and, once signed in,
// register or remove their own passkeys. The collection name defaults to "users"
// and can be overridden via the "?collection=" query string.
//
// Other apps embed the helper directly:
//
//	import PocketBase from 'pocketbase'
//	import { withPasskeys } from 'https://auth.example.com/api/passkeys-client.js'
//	const pb = withPasskeys(new PocketBase('https://auth.example.com'))
//	await pb.collection('users').authWithPasskey('alice@example.com')
//	await pb.collection('users').authWithPasskeyAutofill(abortSignal) // conditional UI, no identity
//	await pb.collection('users').stepUpPasskey() // fresh passkey assertion for gated actions
//	await pb.collection('users').registerPasskey('My MacBook')
//	await pb.collection('users').listPasskeys()
//	await pb.collection('users').renamePasskey(id, 'New name')
//	await pb.collection('users').deletePasskey(id)
func bindPasskeysPage(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Priority: 9998,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET("/api/passkeys-client.js", func(e *core.RequestEvent) error {
				e.Response.Header().Set("Content-Type", "application/javascript; charset=utf-8")
				e.Response.Header().Set("Cache-Control", "public, max-age=300")
				e.Response.Header().Set("Access-Control-Allow-Origin", "*")
				e.Response.WriteHeader(http.StatusOK)
				_, err := e.Response.Write([]byte(passkeysClientJS))
				return err
			})

			se.Router.GET("/passkeys", func(e *core.RequestEvent) error {
				header := e.Response.Header()
				header.Set("Content-Type", "text/html; charset=utf-8")
				header.Set("Cache-Control", "no-cache")
				header.Set("Content-Security-Policy", passkeysPageCSP(e.App.Settings().Captcha))
				e.Response.WriteHeader(http.StatusOK)
				_, err := e.Response.Write([]byte(renderPasskeysPage(NewPageText(e, ""))))
				return err
			})

			return se.Next()
		},
	})
}

// passkeysPageCSP returns the page Content-Security-Policy. It is tight by
// default; only when a captcha is configured AND at least one action requires
// it does it open the chosen provider's domains (and only those) for scripts,
// frames, XHR and styles, so an install without captcha is unchanged.
func passkeysPageCSP(cfg core.CaptchaConfig) string {
	var src captchaCSPSources
	if len(cfg.Required()) > 0 {
		src = captchaSourcesFor(cfg.Provider)
	}

	// audit M6, tightened for #83: the page script and style are files
	// (/passkeys/assets/), so neither 'unsafe-inline' is needed. The data
	// block with the page messages is type application/json, not script.
	// img-src includes data: for the favicon.
	csp := "default-src 'self'; " +
		"script-src 'self'" + src.script + "; " +
		"style-src 'self'" + src.style + "; " +
		"img-src 'self' data:; " +
		"connect-src 'self'" + src.connect + "; "

	if src.frame != "" {
		csp += "frame-src 'self'" + src.frame + "; "
	}

	return csp + "frame-ancestors 'self'; " +
		"base-uri 'self'; " +
		"form-action 'self'"
}

// passkeysClientJS is a self-contained ES module that wraps the
// /api/collections/{c}/auth-with-webauthn/* endpoints into a small
// helper attachable to a PocketBase JS SDK instance.
const passkeysClientJS = `// PocketBase Passkeys helper — augments the pocketbase JS SDK with WebAuthn support.
// Drop-in: import { withPasskeys } from '<this-server>/api/passkeys-client.js'

const b64uToBuf = (s) => {
  s = String(s).replace(/-/g, "+").replace(/_/g, "/");
  while (s.length % 4) s += "=";
  const bin = atob(s);
  const buf = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
  return buf.buffer;
};

const bufToB64u = (buf) => {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.byteLength; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
};

const decodePublicKeyOptions = (opts) => {
  if (!opts) return opts;
  if (opts.challenge) opts.challenge = b64uToBuf(opts.challenge);
  if (opts.user && opts.user.id) opts.user.id = b64uToBuf(opts.user.id);
  if (Array.isArray(opts.allowCredentials)) {
    opts.allowCredentials = opts.allowCredentials.map((c) => ({ ...c, id: b64uToBuf(c.id) }));
  }
  if (Array.isArray(opts.excludeCredentials)) {
    opts.excludeCredentials = opts.excludeCredentials.map((c) => ({ ...c, id: b64uToBuf(c.id) }));
  }
  return opts;
};

const encodeAttestation = (cred) => ({
  id: cred.id,
  rawId: bufToB64u(cred.rawId),
  type: cred.type,
  clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
  response: {
    attestationObject: bufToB64u(cred.response.attestationObject),
    clientDataJSON: bufToB64u(cred.response.clientDataJSON),
  },
});

const encodeAssertion = (cred) => ({
  id: cred.id,
  rawId: bufToB64u(cred.rawId),
  type: cred.type,
  clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
  response: {
    authenticatorData: bufToB64u(cred.response.authenticatorData),
    clientDataJSON: bufToB64u(cred.response.clientDataJSON),
    signature: bufToB64u(cred.response.signature),
    userHandle: cred.response.userHandle ? bufToB64u(cred.response.userHandle) : null,
  },
});

export const helpers = { b64uToBuf, bufToB64u, decodePublicKeyOptions, encodeAttestation, encodeAssertion };

export class PasskeysClient {
  constructor({ baseUrl, collection, authToken, onToken, getCaptchaToken } = {}) {
    if (!baseUrl) throw new Error("baseUrl is required");
    if (!collection) throw new Error("collection is required");
    this.baseUrl = baseUrl.replace(/\/+$/, "");
    this.collection = collection;
    this.authToken = authToken || null;
    // called with the replacement token after a successful step-up
    this.onToken = onToken || null;
    // optional async (action) => token, called before the unauthenticated
    // requests that may need a captcha (see auth-methods "captcha"). The token
    // is sent as the X-Captcha-Token header.
    this.getCaptchaToken = getCaptchaToken || null;
  }

  async _captchaHeaders(action) {
    if (!this.getCaptchaToken) return {};
    const token = await this.getCaptchaToken(action);
    return token ? { "X-Captcha-Token": token } : {};
  }

  _collectionPath() {
    return this.baseUrl + "/api/collections/" + encodeURIComponent(this.collection);
  }

  // methods() returns the collection's auth methods (includes webauthn.discoverable
  // and webauthn.passkeyOnly).
  methods() {
    return this._fetch(this._collectionPath() + "/auth-methods", { method: "GET" });
  }

  // requestReset(email) asks for the emailed passkey recovery link. The server
  // answers 204 whether or not the address is registered.
  async requestReset(email) {
    return this._fetch(this._collectionPath() + "/request-passkey-reset", {
      method: "POST",
      headers: await this._captchaHeaders("reset_request"),
      body: JSON.stringify({ email }),
    });
  }

  setAuthToken(token) { this.authToken = token || null; }

  _path() {
    return this.baseUrl + "/api/collections/" + encodeURIComponent(this.collection) + "/auth-with-webauthn";
  }

  async _fetch(path, opts) {
    opts = opts || {};
    opts.headers = Object.assign({}, opts.headers || {});
    if (opts.body !== undefined && !opts.headers["Content-Type"]) {
      opts.headers["Content-Type"] = "application/json";
    }
    if (this.authToken) opts.headers["Authorization"] = this.authToken;
    const res = await fetch(path, opts);
    const text = await res.text();
    let body = null;
    if (text) { try { body = JSON.parse(text); } catch (e) { body = text; } }
    if (!res.ok) {
      const err = new Error((body && body.message) || ("HTTP " + res.status));
      err.status = res.status;
      err.response = body;
      throw err;
    }
    return body;
  }

  async login(identity) {
    if (!identity) throw new Error("identity is required");
    const begin = await this._fetch(this._path() + "/login-begin", {
      method: "POST",
      headers: await this._captchaHeaders("passkey_login"),
      body: JSON.stringify({ identity }),
    });
    let publicKey = begin.options || begin.publicKey || begin;
    // go-webauthn wraps the options in { publicKey: {...} } — unwrap it.
    if (publicKey && publicKey.publicKey) publicKey = publicKey.publicKey;
    publicKey = decodePublicKeyOptions(publicKey);
    const cred = await navigator.credentials.get({ publicKey });
    return this._fetch(this._path() + "/login-finish", {
      method: "POST",
      body: JSON.stringify(Object.assign(
        { identity, sessionToken: begin.sessionToken },
        encodeAssertion(cred),
      )),
    });
  }

  // autofillLogin() runs a usernameless sign-in through the browser's
  // conditional UI (an <input autocomplete="username webauthn"> offers the
  // saved passkeys). It resolves once the user picks one, and rejects with an
  // AbortError if the signal is aborted. Needs webauthn.discoverable on the
  // collection and a browser with conditional mediation.
  async autofillLogin(signal) {
    const begin = await this._fetch(this._path() + "/login-begin-discoverable", {
      method: "POST",
      body: JSON.stringify({}),
    });
    let publicKey = begin.options || begin.publicKey || begin;
    if (publicKey && publicKey.publicKey) publicKey = publicKey.publicKey;
    publicKey = decodePublicKeyOptions(publicKey);
    const cred = await navigator.credentials.get({ publicKey, mediation: "conditional", signal });
    return this._fetch(this._path() + "/login-finish-discoverable", {
      method: "POST",
      body: JSON.stringify(Object.assign(
        { sessionToken: begin.sessionToken },
        encodeAssertion(cred),
      )),
    });
  }

  // stepUp() proves a fresh passkey assertion for the signed-in user and
  // switches this client (and the onToken callback) to the replacement token.
  // Gated actions, such as deleting a passkey, require one made in the last
  // few minutes.
  async stepUp() {
    const begin = await this._fetch(this._path() + "/step-up-begin", { method: "POST" });
    let publicKey = begin.options || begin.publicKey || begin;
    if (publicKey && publicKey.publicKey) publicKey = publicKey.publicKey;
    publicKey = decodePublicKeyOptions(publicKey);
    const cred = await navigator.credentials.get({ publicKey });
    const res = await this._fetch(this._path() + "/step-up-finish", {
      method: "POST",
      body: JSON.stringify(Object.assign(
        { sessionToken: begin.sessionToken },
        encodeAssertion(cred),
      )),
    });
    if (res && res.token) {
      this.authToken = res.token;
      if (this.onToken) this.onToken(res.token);
    }
    return res;
  }

  async register(name) {
    const begin = await this._fetch(this._path() + "/register-begin", { method: "POST" });
    let publicKey = begin.options || begin.publicKey || begin;
    if (publicKey && publicKey.publicKey) publicKey = publicKey.publicKey;
    publicKey = decodePublicKeyOptions(publicKey);
    const cred = await navigator.credentials.create({ publicKey });
    return this._fetch(this._path() + "/register-finish", {
      method: "POST",
      body: JSON.stringify(Object.assign(
        { sessionToken: begin.sessionToken, name: name || "" },
        encodeAttestation(cred),
      )),
    });
  }

  list() {
    return this._fetch(this._path() + "/credentials", { method: "GET" });
  }

  rename(credentialId, newName) {
    return this._fetch(this._path() + "/credentials/" + encodeURIComponent(credentialId), {
      method: "PATCH",
      body: JSON.stringify({ name: newName }),
    });
  }

  // remove() deletes a passkey. The server wants a passkey assertion from the
  // last few minutes first; when it asks (403 passkey_step_up_required) this
  // runs the step-up prompt once and retries.
  async remove(credentialId) {
    const url = this._path() + "/credentials/" + encodeURIComponent(credentialId);
    try {
      return await this._fetch(url, { method: "DELETE" });
    } catch (err) {
      const code = err && err.response && err.response.data && err.response.data.passkey
        && err.response.data.passkey.code;
      if (err.status !== 403 || code !== "passkey_step_up_required") throw err;
      await this.stepUp();
      return this._fetch(url, { method: "DELETE" });
    }
  }

  static isSupported() {
    return typeof window !== "undefined" && !!window.PublicKeyCredential;
  }
}

// withPasskeys(pb) augments a PocketBase JS SDK instance: every
// pb.collection(name) gains authWithPasskey / registerPasskey / listPasskeys /
// renamePasskey / deletePasskey. authWithPasskey saves into pb.authStore the
// same way authWithPassword does.
export function withPasskeys(pb) {
  if (!pb || typeof pb.collection !== "function") {
    throw new Error("withPasskeys: expected a PocketBase SDK instance");
  }

  const origCollection = pb.collection.bind(pb);
  pb.collection = function (idOrName) {
    const service = origCollection(idOrName);
    const collectionName = idOrName;
    const client = () => new PasskeysClient({
      baseUrl: pb.baseUrl || pb.baseURL || "",
      collection: collectionName,
      authToken: pb.authStore && pb.authStore.token ? pb.authStore.token : null,
      onToken: (token) => {
        if (pb.authStore && pb.authStore.save) pb.authStore.save(token, pb.authStore.record || pb.authStore.model);
      },
    });

    service.authWithPasskey = async function (identity) {
      const result = await client().login(identity);
      if (result && result.token && result.record && pb.authStore && pb.authStore.save) {
        pb.authStore.save(result.token, result.record);
      }
      return result;
    };

    service.authWithPasskeyAutofill = async function (signal) {
      const result = await client().autofillLogin(signal);
      if (result && result.token && result.record && pb.authStore && pb.authStore.save) {
        pb.authStore.save(result.token, result.record);
      }
      return result;
    };

    service.stepUpPasskey   = function ()    { return client().stepUp(); };
    service.registerPasskey = function (name) { return client().register(name); };
    service.listPasskeys    = function ()    { return client().list(); };
    service.renamePasskey   = function (id, n) { return client().rename(id, n); };
    service.deletePasskey   = function (id)  { return client().remove(id); };

    return service;
  };

  return pb;
}

export default { withPasskeys, PasskeysClient, helpers };
`

var passkeysMarkerRe = regexp.MustCompile(`@\{([A-Za-z0-9_.]+)\}`)

// renderPasskeysPage fills the page template with the messages of the language.
// Messages are HTML-escaped; the English result is byte-identical to the
// page before localization (apart from the JSON data block with the script strings).
func renderPasskeysPage(text *PageText) string {
	return passkeysMarkerRe.ReplaceAllStringFunc(passkeysPageHTML, func(m string) string {
		key := m[2 : len(m)-1]

		switch key {
		case "lang":
			return text.LangAttr()
		case "datablock":
			return string(text.DataBlock())
		case "passkeys.signedInAsHtml":
			return string(text.HTML(key, text.Args("who", template.HTML(`<strong id="me"></strong>`))))
		}

		return template.HTMLEscapeString(text.T(key))
	})
}

// passkeysPageHTML is the page template: a marker is replaced by the catalog message in the
// language of the request (see renderPasskeysPage).
const passkeysPageHTML = `<!DOCTYPE html>
<html@{lang}>
<head>
<meta charset="utf-8">
<title>@{passkeys.title}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<link rel="icon" href="data:,">
<link rel="stylesheet" href="/passkeys/assets/passkeys-page.css">
<link rel="stylesheet" href="/a11y/a11y-mode.css">
<script src="/a11y/a11y-mode.js"></script>
@{datablock}</head>
<body>
<a class="skip" href="#app">@{a11y.skip}</a>
<header class="a11y-bar"><button type="button" id="a11yToggle" class="a11y-toggle" aria-pressed="false" hidden>@{a11y.toggle}</button></header>

<main class="card" id="app" tabindex="-1">
<h1>@{passkeys.title}</h1>
<p class="muted">@{passkeys.intro}</p>

<div id="msg"></div>

<section id="anon" aria-labelledby="anonHeading">
  <h2 id="anonHeading">@{js.passkeySignIn}</h2>
  <label for="identity">@{js.identityLabel}</label>
  <input type="text" id="identity" autocomplete="username webauthn">
  <div id="captchaBox" hidden></div>
  <button type="button" id="btnLogin">@{passkeys.signInButton}</button>
  <div id="loginMsg"></div>
  <p class="muted hint" id="pwHint">
    @{passkeys.noPasskey} <button type="button" class="linklike" id="linkPasswordLogin" aria-controls="pwForm" aria-expanded="false">@{passkeys.noPasskeyLink}</button>
  </p>

  <div id="recovery" hidden>
    <p class="muted hint">
      @{passkeys.lostPrompt}
      <button type="button" class="linklike" id="linkRecovery" aria-controls="recoveryForm" aria-expanded="false">@{passkeys.lostLink}</button>
    </p>
    <div id="recoveryForm" hidden>
      <label for="recoveryEmail">@{orgs.email}</label>
      <input type="email" id="recoveryEmail" autocomplete="email">
      <button type="button" id="btnRecovery">@{passkeys.sendLink}</button>
      <div id="recoveryMsg"></div>
    </div>
  </div>

  <div id="pwForm" hidden>
    <hr>
    <h2>@{passkeys.passwordHeading}</h2>
    <label for="pwEmail">@{orgs.email}</label>
    <input type="email" id="pwEmail" autocomplete="username">
    <label for="pwPass">@{js.passwordLabel}</label>
    <input type="password" id="pwPass" autocomplete="current-password">
    <button type="button" id="btnPwLogin">@{js.signIn}</button>
    <div id="pwMsg"></div>
  </div>
</section>

<section id="authed" aria-labelledby="authedHeading" hidden>
  <h2 id="authedHeading">@{passkeys.yourPasskeys}</h2>
  <p>@{passkeys.signedInAsHtml} <button type="button" class="linklike" id="linkLogout">@{orgs.signOut}</button></p>
  <button type="button" id="btnRegister">@{passkeys.register}</button>
  <ul class="creds" id="creds"></ul>
</section>
</main>

<script type="module" src="/passkeys/assets/passkeys-page.js"></script>
</body>
</html>
`
