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
				// audit M6: tight CSP. Inline script/style are required because
				// the helper module is consumed at runtime and the page applies
				// minimal inline styles. img-src includes data: for the favicon.
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

	// audit M6: tight CSP. Inline script/style are required because the
	// helper module is consumed at runtime and the page applies minimal inline
	// styles. img-src includes data: for the favicon.
	csp := "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'" + src.script + "; " +
		"style-src 'self' 'unsafe-inline'" + src.style + "; " +
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

// passkeysPageHTML is the page template: "@{key}" is replaced by the catalog message in the
// language of the request (see renderPasskeysPage).
const passkeysPageHTML = `<!DOCTYPE html>
<html@{lang}>
<head>
<meta charset="utf-8">
<title>@{passkeys.title}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: light dark; }
  body { font: 15px/1.5 -apple-system, system-ui, sans-serif; max-width: 480px; margin: 4em auto; padding: 0 1em; }
  h1 { font-size: 1.5em; margin-bottom: 0.2em; }
  .muted { opacity: 0.65; font-size: 0.9em; }
  label { display: block; margin-top: 1em; }
  input[type=text], input[type=email], input[type=password] {
    width: 100%; box-sizing: border-box; padding: 0.5em; font-size: 1em;
    border: 1px solid #888; border-radius: 4px; background: transparent; color: inherit;
  }
  button { padding: 0.55em 1em; font-size: 1em; border-radius: 4px; border: 1px solid #888;
           background: #4a8cff; color: white; cursor: pointer; margin-top: 1em; }
  button.secondary { background: transparent; color: inherit; }
  button[disabled] { opacity: 0.5; cursor: not-allowed; }
  .msg { margin-top: 1em; padding: 0.6em; border-radius: 4px; }
  .msg.err { background: #fdecea; color: #5a1410; }
  .msg.ok  { background: #e6f4ea; color: #14532d; }
  ul.creds { list-style: none; padding: 0; }
  ul.creds li { display: flex; align-items: center; gap: 0.5em; padding: 0.5em 0; border-bottom: 1px solid #ddd3; }
  ul.creds li .meta { flex: 1; }
  ul.creds li .meta small { display: block; opacity: 0.6; }
  hr { border: none; border-top: 1px solid #8884; margin: 2em 0; }
</style>
@{datablock}</head>
<body>

<h1>@{passkeys.title}</h1>
<p class="muted">@{passkeys.intro}</p>

<div id="msg"></div>

<section id="anon">
  <h2 style="font-size:1.1em;">@{js.passkeySignIn}</h2>
  <label>@{js.identityLabel}
    <input type="text" id="identity" autocomplete="username webauthn" autofocus>
  </label>
  <div id="captchaBox" hidden></div>
  <button id="btnLogin">@{passkeys.signInButton}</button>
  <p class="muted" id="pwHint" style="margin-top:1.5em;">
    @{passkeys.noPasskey} <a href="#" id="linkPasswordLogin">@{passkeys.noPasskeyLink}</a>
  </p>

  <div id="recovery" hidden>
    <p class="muted" style="margin-top:1.5em;">
      @{passkeys.lostPrompt}
      <a href="#" id="linkRecovery">@{passkeys.lostLink}</a>
    </p>
    <div id="recoveryForm" hidden>
      <label>@{orgs.email}
        <input type="email" id="recoveryEmail" autocomplete="email">
      </label>
      <button id="btnRecovery">@{passkeys.sendLink}</button>
    </div>
  </div>

  <div id="pwForm" hidden>
    <hr>
    <h2 style="font-size:1.1em;">@{passkeys.passwordHeading}</h2>
    <label>@{orgs.email}
      <input type="email" id="pwEmail" autocomplete="username">
    </label>
    <label>@{js.passwordLabel}
      <input type="password" id="pwPass" autocomplete="current-password">
    </label>
    <button id="btnPwLogin">@{js.signIn}</button>
  </div>
</section>

<section id="authed" hidden>
  <p>@{passkeys.signedInAsHtml} <a href="#" id="linkLogout">@{orgs.signOut}</a></p>
  <button id="btnRegister">@{passkeys.register}</button>
  <h3 style="font-size:1em; margin-top:2em;">@{passkeys.yourPasskeys}</h3>
  <ul class="creds" id="creds"></ul>
</section>

<script type="module">
import { PasskeysClient } from "/api/passkeys-client.js";

// the strings of the page, in the language the server negotiated (see the JSON data block in the head)
let MESSAGES = {};
try { MESSAGES = JSON.parse(document.getElementById("i18n").textContent); } catch (e) { /* English keys show */ }
const t = (key, args) => {
  const msg = Object.prototype.hasOwnProperty.call(MESSAGES, key) ? MESSAGES[key] : key;
  return msg.replace(/\{(\w+)\}/g, (m, k) => (args && k in args ? args[k] : m));
};

const COLLECTION = new URLSearchParams(location.search).get("collection") || "users";
const STORAGE_KEY = "pb_passkeys_auth_" + COLLECTION;
const BASE_URL = location.origin;

const $ = (id) => document.getElementById(id);

function showMsg(text, kind) {
  const el = $("msg");
  if (!text) { el.innerHTML = ""; return; }
  el.innerHTML = '<div class="msg ' + (kind || "ok") + '"></div>';
  el.firstChild.textContent = text;
}
function loadAuth() {
  try { return JSON.parse(localStorage.getItem(STORAGE_KEY) || "null"); } catch (e) { return null; }
}
function saveAuth(v) {
  if (v) localStorage.setItem(STORAGE_KEY, JSON.stringify(v));
  else localStorage.removeItem(STORAGE_KEY);
}
// ---- optional captcha (auth-methods "captcha") --------------------------
// The provider script is loaded ONLY when the collection requires a captcha
// for the action being performed, and never otherwise.
let captchaCfg = null;
const captchaScripts = {};
const CAPTCHA_SRC = {
  turnstile: () => "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit",
  hcaptcha: () => "https://js.hcaptcha.com/1/api.js?render=explicit",
  recaptchaV2: () => "https://www.google.com/recaptcha/api.js?render=explicit",
  recaptchaV3: (cfg) => "https://www.google.com/recaptcha/api.js?render=" + encodeURIComponent(cfg.siteKey),
};
function loadCaptchaScript(cfg) {
  const src = CAPTCHA_SRC[cfg.provider] && CAPTCHA_SRC[cfg.provider](cfg);
  if (!src) return Promise.reject(new Error(t("js.captcha.unsupported")));
  if (!captchaScripts[src]) {
    captchaScripts[src] = new Promise((resolve, reject) => {
      const el = document.createElement("script");
      el.src = src; el.async = true;
      el.onload = resolve;
      el.onerror = () => reject(new Error(t("js.captcha.loadFailed")));
      document.head.appendChild(el);
    });
  }
  return captchaScripts[src];
}
async function getCaptchaToken(action) {
  const cfg = captchaCfg;
  if (!cfg || !cfg.required || cfg.required.indexOf(action) < 0) return undefined;
  await loadCaptchaScript(cfg);
  if (cfg.provider === "recaptchaV3") {
    await new Promise((r) => grecaptcha.ready(r));
    return grecaptcha.execute(cfg.siteKey, { action });
  }
  const api = cfg.provider === "turnstile" ? window.turnstile : cfg.provider === "hcaptcha" ? window.hcaptcha : window.grecaptcha;
  if (cfg.provider === "recaptchaV2") await new Promise((r) => grecaptcha.ready(r));
  const box = $("captchaBox");
  box.hidden = false;
  box.textContent = "";
  const holder = document.createElement("div");
  box.appendChild(holder);
  return new Promise((resolve, reject) => {
    const done = (fn, v) => { box.hidden = true; box.textContent = ""; fn(v); };
    api.render(holder, {
      sitekey: cfg.siteKey,
      action: cfg.provider === "turnstile" ? action : undefined,
      callback: (token) => done(resolve, token),
      "error-callback": () => done(reject, new Error(t("js.captcha.failed"))),
      "expired-callback": () => done(reject, new Error(t("js.captcha.expired"))),
    });
  });
}

function client() {
  const auth = loadAuth();
  return new PasskeysClient({
    getCaptchaToken,
    baseUrl: BASE_URL,
    collection: COLLECTION,
    authToken: auth ? auth.token : null,
    // a step-up replaces the token; keep the stored session in step
    onToken: (token) => {
      const cur = loadAuth();
      if (cur) saveAuth({ token, record: cur.record });
    },
  });
}

let autofillAbort = null;

// Conditional UI: the identity input carries autocomplete="username webauthn",
// so when the collection allows usernameless sign-in the browser offers saved
// passkeys right in the field. Picking one signs in with no typing.
async function startAutofill() {
  if (autofillAbort) return;
  try {
    if (!window.PublicKeyCredential || !PublicKeyCredential.isConditionalMediationAvailable) return;
    if (!(await PublicKeyCredential.isConditionalMediationAvailable())) return;
    autofillAbort = new AbortController();
    const result = await client().autofillLogin(autofillAbort.signal);
    autofillAbort = null;
    saveAuth({ token: result.token, record: result.record });
    render();
    showMsg(t("js.passkeys.signedIn"), "ok");
  } catch (e) {
    autofillAbort = null;
    if (e && e.name === "AbortError") return;
    // autofill is best effort: stay quiet and leave the manual button working
  }
}

function stopAutofill() {
  if (autofillAbort) { autofillAbort.abort(); autofillAbort = null; }
}

async function setupMethods() {
  try {
    const methods = await client().methods();
    captchaCfg = (methods && methods.captcha) || null;
    const wa = (methods && methods.webauthn) || {};
    if (wa.passkeyOnly) {
      // no password to fall back on: offer the emailed link instead
      $("pwHint").hidden = true;
      $("pwForm").hidden = true;
      $("recovery").hidden = false;
    }
    if (wa.discoverable) startAutofill();
  } catch (e) {
    // leave the defaults in place
  }
}

async function sendRecovery() {
  showMsg("");
  try {
    await client().requestReset($("recoveryEmail").value);
    showMsg(t("js.passkeys.linkOnItsWay"), "ok");
  } catch (e) {
    showMsg(t("js.passkeys.sendFailed", { error: e.message }), "err");
  }
}

async function loginWithPasskey() {
  showMsg("");
  stopAutofill();
  try {
    const result = await client().login($("identity").value);
    saveAuth({ token: result.token, record: result.record });
    render();
    showMsg("Signed in.", "ok");
  } catch (e) {
    showMsg(t("js.passkeys.loginFailed", { error: e.message }), "err");
  }
}

async function passwordLogin() {
  showMsg("");
  try {
    const headers = { "Content-Type": "application/json" };
    const captcha = await getCaptchaToken("login");
    if (captcha) headers["X-Captcha-Token"] = captcha;
    const res = await fetch(BASE_URL + "/api/collections/" + encodeURIComponent(COLLECTION) + "/auth-with-password", {
      method: "POST",
      headers,
      body: JSON.stringify({ identity: $("pwEmail").value, password: $("pwPass").value }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.message || ("HTTP " + res.status));
    saveAuth({ token: body.token, record: body.record });
    render();
    showMsg(t("js.passkeys.signedInRegister"), "ok");
  } catch (e) {
    showMsg(t("js.passkeys.passwordFailed", { error: e.message }), "err");
  }
}

async function registerPasskey() {
  showMsg("");
  try {
    const friendly = prompt(t("js.passkeys.namePrompt")) || "";
    await client().register(friendly);
    showMsg(t("js.passkeys.registered"), "ok");
    loadCredentials();
  } catch (e) {
    showMsg(t("js.passkeys.registerFailed", { error: e.message }), "err");
  }
}

async function loadCredentials() {
  const ul = $("creds");
  ul.textContent = "";
  try {
    const list = await client().list();
    const items = Array.isArray(list) ? list : (list.items || []);
    if (!items.length) {
      const li = document.createElement("li");
      li.className = "muted";
      li.textContent = t("js.passkeys.none");
      ul.appendChild(li);
      return;
    }
    for (const c of items) {
      const li = document.createElement("li");
      const meta = document.createElement("div");
      meta.className = "meta";
      const name = document.createElement("div");
      name.textContent = c.name || t("js.passkeys.unnamed");
      const sub = document.createElement("small");
      sub.textContent = (c.created ? t("js.passkeys.created", { date: c.created }) : "") +
        (c.lastUsed ? " · " + t("js.passkeys.lastUsed", { date: c.lastUsed }) : " · " + t("js.passkeys.neverUsed")) +
        (c.signCount !== undefined ? " · " + t("js.passkeys.uses", { count: c.signCount }) : "");
      meta.appendChild(name);
      meta.appendChild(sub);

      const renameBtn = document.createElement("button");
      renameBtn.className = "secondary";
      renameBtn.textContent = t("js.passkeys.rename");
      renameBtn.style.marginTop = "0";
      renameBtn.onclick = async () => {
        const newName = prompt(t("js.passkeys.newName"), c.name || "");
        if (!newName) return;
        try { await client().rename(c.id, newName); loadCredentials(); }
        catch (e) { showMsg(t("js.passkeys.renameFailed", { error: e.message }), "err"); }
      };

      const delBtn = document.createElement("button");
      delBtn.className = "secondary";
      delBtn.textContent = t("js.passkeys.delete");
      delBtn.style.marginTop = "0";
      delBtn.onclick = async () => {
        if (!confirm(t("js.passkeys.confirmDelete"))) return;
        try { await client().remove(c.id); loadCredentials(); }
        catch (e) { showMsg(t("js.passkeys.deleteFailed", { error: e.message }), "err"); }
      };

      li.appendChild(meta);
      li.appendChild(renameBtn);
      li.appendChild(delBtn);
      ul.appendChild(li);
    }
  } catch (e) {
    showMsg(t("js.passkeys.loadFailed", { error: e.message }), "err");
  }
}

function render() {
  const auth = loadAuth();
  if (auth && auth.record) {
    $("anon").hidden = true;
    $("authed").hidden = false;
    $("me").textContent = auth.record.email || auth.record.id;
    loadCredentials();
  } else {
    $("anon").hidden = false;
    $("authed").hidden = true;
  }
}

$("btnLogin").addEventListener("click", loginWithPasskey);
$("btnPwLogin").addEventListener("click", passwordLogin);
$("btnRegister").addEventListener("click", registerPasskey);
$("linkPasswordLogin").addEventListener("click", (e) => { e.preventDefault(); $("pwForm").hidden = false; });
$("linkLogout").addEventListener("click", (e) => { e.preventDefault(); saveAuth(null); render(); setupMethods(); });
$("linkRecovery").addEventListener("click", (e) => { e.preventDefault(); $("recoveryForm").hidden = false; });
$("btnRecovery").addEventListener("click", sendRecovery);

if (!window.PublicKeyCredential) {
  showMsg(t("js.passkeys.unsupported"), "err");
}
render();
if (!loadAuth()) setupMethods();
</script>
</body>
</html>
`
