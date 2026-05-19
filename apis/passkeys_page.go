package apis

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
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
				e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
				e.Response.Header().Set("Cache-Control", "no-cache")
				e.Response.WriteHeader(http.StatusOK)
				_, err := e.Response.Write([]byte(passkeysPageHTML))
				return err
			})

			return se.Next()
		},
	})
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
  constructor({ baseUrl, collection, authToken } = {}) {
    if (!baseUrl) throw new Error("baseUrl is required");
    if (!collection) throw new Error("collection is required");
    this.baseUrl = baseUrl.replace(/\/+$/, "");
    this.collection = collection;
    this.authToken = authToken || null;
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
      body: JSON.stringify({ identity }),
    });
    const publicKey = decodePublicKeyOptions(begin.options || begin.publicKey || begin);
    const cred = await navigator.credentials.get({ publicKey });
    return this._fetch(this._path() + "/login-finish", {
      method: "POST",
      body: JSON.stringify(Object.assign(
        { identity, sessionToken: begin.sessionToken },
        encodeAssertion(cred),
      )),
    });
  }

  async register(name) {
    const begin = await this._fetch(this._path() + "/register-begin", { method: "POST" });
    const publicKey = decodePublicKeyOptions(begin.options || begin.publicKey || begin);
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

  remove(credentialId) {
    return this._fetch(this._path() + "/credentials/" + encodeURIComponent(credentialId), {
      method: "DELETE",
    });
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
    });

    service.authWithPasskey = async function (identity) {
      const result = await client().login(identity);
      if (result && result.token && result.record && pb.authStore && pb.authStore.save) {
        pb.authStore.save(result.token, result.record);
      }
      return result;
    };

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

const passkeysPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Passkeys</title>
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
</head>
<body>

<h1>Passkeys</h1>
<p class="muted">Sign in with — or register — a passkey on this device.</p>

<div id="msg"></div>

<section id="anon">
  <h2 style="font-size:1.1em;">Sign in with a passkey</h2>
  <label>Email or username
    <input type="text" id="identity" autocomplete="username webauthn" autofocus>
  </label>
  <button id="btnLogin">Sign in with passkey</button>
  <p class="muted" style="margin-top:1.5em;">
    No passkey yet? <a href="#" id="linkPasswordLogin">Sign in with password to register one.</a>
  </p>

  <div id="pwForm" hidden>
    <hr>
    <h2 style="font-size:1.1em;">Sign in with password</h2>
    <label>Email
      <input type="email" id="pwEmail" autocomplete="username">
    </label>
    <label>Password
      <input type="password" id="pwPass" autocomplete="current-password">
    </label>
    <button id="btnPwLogin">Sign in</button>
  </div>
</section>

<section id="authed" hidden>
  <p>Signed in as <strong id="me"></strong>. <a href="#" id="linkLogout">Sign out</a></p>
  <button id="btnRegister">Register a new passkey on this device</button>
  <h3 style="font-size:1em; margin-top:2em;">Your passkeys</h3>
  <ul class="creds" id="creds"></ul>
</section>

<script type="module">
import { PasskeysClient } from "/api/passkeys-client.js";

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
function client() {
  const auth = loadAuth();
  return new PasskeysClient({
    baseUrl: BASE_URL,
    collection: COLLECTION,
    authToken: auth ? auth.token : null,
  });
}

async function loginWithPasskey() {
  showMsg("");
  try {
    const result = await client().login($("identity").value);
    saveAuth({ token: result.token, record: result.record });
    render();
    showMsg("Signed in.", "ok");
  } catch (e) {
    showMsg("Login failed: " + e.message, "err");
  }
}

async function passwordLogin() {
  showMsg("");
  try {
    const res = await fetch(BASE_URL + "/api/collections/" + encodeURIComponent(COLLECTION) + "/auth-with-password", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ identity: $("pwEmail").value, password: $("pwPass").value }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.message || ("HTTP " + res.status));
    saveAuth({ token: body.token, record: body.record });
    render();
    showMsg("Signed in. You can now register a passkey.", "ok");
  } catch (e) {
    showMsg("Password login failed: " + e.message, "err");
  }
}

async function registerPasskey() {
  showMsg("");
  try {
    const friendly = prompt("Name this passkey (e.g. \"MacBook Touch ID\")") || "";
    await client().register(friendly);
    showMsg("Passkey registered.", "ok");
    loadCredentials();
  } catch (e) {
    showMsg("Registration failed: " + e.message, "err");
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
      li.textContent = "No registered passkeys yet.";
      ul.appendChild(li);
      return;
    }
    for (const c of items) {
      const li = document.createElement("li");
      const meta = document.createElement("div");
      meta.className = "meta";
      const name = document.createElement("div");
      name.textContent = c.name || "Unnamed passkey";
      const sub = document.createElement("small");
      sub.textContent = (c.created ? "Registered: " + c.created : "") +
        (c.signCount !== undefined ? " · Uses: " + c.signCount : "");
      meta.appendChild(name);
      meta.appendChild(sub);

      const renameBtn = document.createElement("button");
      renameBtn.className = "secondary";
      renameBtn.textContent = "Rename";
      renameBtn.style.marginTop = "0";
      renameBtn.onclick = async () => {
        const newName = prompt("New name:", c.name || "");
        if (!newName) return;
        try { await client().rename(c.id, newName); loadCredentials(); }
        catch (e) { showMsg("Rename failed: " + e.message, "err"); }
      };

      const delBtn = document.createElement("button");
      delBtn.className = "secondary";
      delBtn.textContent = "Delete";
      delBtn.style.marginTop = "0";
      delBtn.onclick = async () => {
        if (!confirm("Delete this passkey?")) return;
        try { await client().remove(c.id); loadCredentials(); }
        catch (e) { showMsg("Delete failed: " + e.message, "err"); }
      };

      li.appendChild(meta);
      li.appendChild(renameBtn);
      li.appendChild(delBtn);
      ul.appendChild(li);
    }
  } catch (e) {
    showMsg("Failed to load credentials: " + e.message, "err");
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
$("linkLogout").addEventListener("click", (e) => { e.preventDefault(); saveAuth(null); render(); });

if (!window.PublicKeyCredential) {
  showMsg("This browser does not support WebAuthn / passkeys.", "err");
}
render();
</script>
</body>
</html>
`
