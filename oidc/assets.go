package oidc

// The CSS and scripts of the hosted pages are served as files, never inlined,
// so the Content-Security-Policy can forbid inline script and style.

const oidcCSS = `:root {
  color-scheme: light dark;
  --bg: #f4f5f7; --card: #ffffff; --fg: #1b1e23; --muted: #646b77; --line: #d5d9e0;
  --accent: #2f5fd0; --accent-fg: #ffffff; --err-bg: #fdecea; --err-fg: #5a1410; --ok-bg: #e6f4ea; --ok-fg: #14532d;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14161a; --card: #1d2026; --fg: #eceff4; --muted: #9aa3b2; --line: #343944;
    --accent: #6c93f0; --accent-fg: #0c1220; --err-bg: #3b1d1b; --err-fg: #ffd4cf; --ok-bg: #163322; --ok-fg: #c6f0d4;
  }
}
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 16px;
  background: var(--bg); color: var(--fg); font: 15px/1.5 -apple-system, system-ui, "Segoe UI", sans-serif; }
.card { width: 100%; max-width: 400px; background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 28px 24px; }
h1 { margin: 0 0 4px; font-size: 1.4rem; }
.sub, .muted { color: var(--muted); }
.sub { margin: 0 0 16px; }
label { display: block; margin-top: 14px; font-weight: 500; }
input { display: block; width: 100%; margin-top: 4px; padding: 10px; font: inherit; color: inherit;
  background: transparent; border: 1px solid var(--line); border-radius: 8px; }
input:focus-visible, button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
button { width: 100%; margin-top: 16px; padding: 10px 14px; font: inherit; font-weight: 600; cursor: pointer;
  color: var(--accent-fg); background: var(--accent); border: 1px solid var(--accent); border-radius: 8px; }
button.secondary { color: var(--fg); background: transparent; border-color: var(--line); }
button[disabled] { opacity: .55; cursor: not-allowed; }
.row { display: flex; gap: 8px; } .row button { margin-top: 20px; }
.or { display: flex; align-items: center; gap: 10px; margin-top: 18px; color: var(--muted); font-size: .85rem; }
.or::before, .or::after { content: ""; flex: 1; border-top: 1px solid var(--line); }
.msg { margin: 14px 0 0; padding: 10px 12px; border-radius: 8px; }
.msg.err { background: var(--err-bg); color: var(--err-fg); } .msg.ok { background: var(--ok-bg); color: var(--ok-fg); }
.scopes { list-style: none; margin: 14px 0 0; padding: 0; }
.scopes li { padding: 10px 0; border-top: 1px solid var(--line); } .scopes li span { display: block; color: var(--muted); font-size: .9rem; }
[hidden] { display: none !important; }
`

const loginJS = `import { PasskeysClient } from "/api/passkeys-client.js";

// the strings of the page, in the language the server negotiated (the JSON data block in the head)
let MESSAGES = {};
try { MESSAGES = JSON.parse(document.getElementById("i18n").textContent); } catch (e) { /* English keys show */ }
const t = (key) => (Object.prototype.hasOwnProperty.call(MESSAGES, key) ? MESSAGES[key] : key);

const root = document.getElementById("app");
const requestID = root.dataset.request;
const collection = root.dataset.collection;
const captchaBox = root.querySelector("#captchaBox");
const pw = root.querySelector("#pw");
const totp = root.querySelector("#totp");
const passkeyBox = root.querySelector("#passkeyBox");
const oauthBox = root.querySelector("#oauthBox");
const oauthList = root.querySelector("#oauthList");
const passkeyBtn = root.querySelector("#passkey");
const statusEl = root.querySelector("#status");

let mfaId = "";

if (root.dataset.loginHint) pw.elements.identity.value = root.dataset.loginHint;

// the message sits right under the forms and the fields point at it (aria-describedby in the page)
function fail(text) {
  statusEl.textContent = text;
  statusEl.hidden = !text;
  for (const f of root.querySelectorAll("#pw input, #totp input")) {
    if (text) f.setAttribute("aria-invalid", "true");
    else f.removeAttribute("aria-invalid");
  }
}

async function finish(token) {
  const res = await fetch(root.dataset.finish || "/oidc/login/finish", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Authorization: token },
    body: JSON.stringify({ authRequestID: requestID, ref: requestID }),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok || !body.redirect) {
    fail(body.message || t("js.oidc.finishFailed"));
    return;
  }
  window.location.assign(body.redirect);
}

async function post(path, payload, extraHeaders) {
  const res = await fetch("/api/collections/" + encodeURIComponent(collection) + path, {
    method: "POST",
    headers: Object.assign({ "Content-Type": "application/json" }, extraHeaders || {}),
    body: JSON.stringify(payload),
  });
  return { status: res.status, body: await res.json().catch(() => ({})) };
}

// ---- optional captcha (auth-methods "captcha") ----------------------------
// The provider script is loaded only when the collection requires a captcha
// for the action being performed, and never otherwise. The server extends the
// page policy with the provider's domains in exactly the same case.
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
      el.src = src;
      el.async = true;
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
    await new Promise((r) => window.grecaptcha.ready(r));
    return window.grecaptcha.execute(cfg.siteKey, { action });
  }
  const api = cfg.provider === "turnstile" ? window.turnstile : cfg.provider === "hcaptcha" ? window.hcaptcha : window.grecaptcha;
  if (cfg.provider === "recaptchaV2") await new Promise((r) => window.grecaptcha.ready(r));
  captchaBox.hidden = false;
  captchaBox.textContent = "";
  const holder = document.createElement("div");
  captchaBox.appendChild(holder);
  return new Promise((resolve, reject) => {
    const done = (fn, v) => { captchaBox.hidden = true; captchaBox.textContent = ""; fn(v); };
    api.render(holder, {
      sitekey: cfg.siteKey,
      action: cfg.provider === "turnstile" ? action : undefined,
      callback: (token) => done(resolve, token),
      "error-callback": () => done(reject, new Error(t("js.captcha.failed"))),
      "expired-callback": () => done(reject, new Error(t("js.captcha.expired"))),
    });
  });
}

function isCaptchaFailure(r) {
  return r.status === 400 && r.body && r.body.data && r.body.data.captchaToken;
}

function secondFactor(body) {
  if (body && body.mfaId && body.totp) {
    mfaId = body.mfaId;
    pw.hidden = true;
    passkeyBox.hidden = true;
    totp.hidden = false;
    totp.elements.code.focus();
    fail("");
    return true;
  }
  if (body && body.mfaId) {
    fail(t("js.signin.unsupportedFactor"));
    return true;
  }
  return false;
}

// ---- sign in with an OAuth2 provider (Microsoft, Google, ...) ---------------
// The same flow as the JavaScript SDK's authWithOAuth2: a realtime connection
// gives the page an id, the provider's sign-in window carries it as the state,
// /api/oauth2-redirect sends the code back over the connection, and the code is
// exchanged for the ordinary Claviger token at auth-with-oauth2. The window is
// opened in the click itself, so a pop-up blocker lets it through.
const OAUTH_TIMEOUT_MS = 5 * 60 * 1000;

function fmt(key, vars) {
  return t(key).replace(/\{(\w+)\}/g, (m, k) => (Object.prototype.hasOwnProperty.call(vars, k) ? vars[k] : m));
}

function renderOAuth(methods) {
  const o = methods && methods.oauth2;
  if (!o || !o.enabled || !Array.isArray(o.providers) || !o.providers.length) return false;
  for (const p of o.providers) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "secondary";
    b.dataset.provider = p.name;
    b.textContent = fmt("js.oidc.continueWith", { provider: p.displayName || p.name });
    b.addEventListener("click", () => oauthSignIn(p.name, p.displayName || p.name));
    oauthList.appendChild(b);
  }
  oauthBox.hidden = false;
  return true;
}

function oauthEvents() {
  const es = new EventSource("/api/realtime");
  const ready = new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("connect")), 15000);
    es.addEventListener("PB_CONNECT", (ev) => {
      clearTimeout(timer);
      try { resolve(JSON.parse(ev.data).clientId); } catch (e) { reject(e); }
    });
    es.onerror = () => { clearTimeout(timer); reject(new Error("connect")); };
  });
  return { es, ready };
}

async function oauthSignIn(name, label) {
  fail("");
  const popup = window.open("", "claviger-oauth", "popup,width=520,height=720");
  if (!popup) return fail(t("js.oidc.popupBlocked"));
  const buttons = oauthList.querySelectorAll("button");
  buttons.forEach((b) => { b.disabled = true; });
  let es = null;
  let watcher = 0;
  try {
    // a fresh list: its state and code verifier belong to this attempt
    const methods = await new PasskeysClient({ baseUrl: window.location.origin, collection }).methods();
    const provider = ((methods.oauth2 && methods.oauth2.providers) || []).find((p) => p.name === name);
    if (!provider) throw new Error("provider");

    const ev = oauthEvents();
    es = ev.es;
    const clientId = await ev.ready;
    const redirectURL = window.location.origin + "/api/oauth2-redirect";

    const result = new Promise((resolve, reject) => {
      es.addEventListener("@oauth2", (m) => {
        try { resolve(JSON.parse(m.data)); } catch (e) { reject(e); }
      });
      setTimeout(() => reject(new Error("timeout")), OAUTH_TIMEOUT_MS);
      // the person closed the window without finishing: give a late message a moment, then stop
      watcher = setInterval(() => {
        if (popup.closed) { clearInterval(watcher); setTimeout(() => reject(new Error("closed")), 1500); }
      }, 500);
    });

    const sub = await fetch("/api/realtime", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ clientId, subscriptions: ["@oauth2"] }),
    });
    if (!sub.ok) throw new Error("subscribe");

    popup.location.href = provider.authURL.replace(/([?&])state=[^&]*/, "$1state=" + encodeURIComponent(clientId)) + encodeURIComponent(redirectURL);

    const data = await result;
    if (data.state !== clientId || data.error || !data.code) throw new Error("denied");

    const r = await post("/auth-with-oauth2", {
      provider: provider.name,
      code: data.code,
      codeVerifier: provider.codeVerifier,
      redirectURL,
    });
    if (r.status === 200 && r.body.token) return await finish(r.body.token);
    if (r.status === 401 && secondFactor(r.body)) return;
    fail(r.status === 429 ? t("js.tooMany") : fmt("js.oidc.oauthFailed", { provider: label }));
  } catch (err) {
    fail(fmt("js.oidc.oauthFailed", { provider: label }));
  } finally {
    clearInterval(watcher);
    if (es) es.close();
    try { popup.close(); } catch (e) { /* already closed */ }
    buttons.forEach((b) => { b.disabled = false; });
  }
}

pw.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  fail("");
  const headers = {};
  try {
    const captcha = await getCaptchaToken("login");
    if (captcha) headers["X-Captcha-Token"] = captcha;
  } catch (err) {
    fail(err.message || t("js.captcha.incomplete"));
    return;
  }
  const r = await post("/auth-with-password", {
    identity: pw.elements.identity.value,
    password: pw.elements.password.value,
  }, headers);
  if (r.status === 200 && r.body.token) return finish(r.body.token);
  if (r.status === 401 && secondFactor(r.body)) return;
  if (isCaptchaFailure(r)) return fail(t("js.captcha.checkFailed"));
  fail(r.status === 429 ? t("js.tooMany") : t("js.signin.wrongCredentials"));
});

totp.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  fail("");
  const r = await post("/auth-with-totp", { mfaId, code: totp.elements.code.value.trim() });
  if (r.status === 200 && r.body.token) return finish(r.body.token);
  fail(r.status === 429 ? t("js.tooMany") : t("js.signin.codeRejected"));
});

passkeyBtn.addEventListener("click", async () => {
  fail("");
  const client = new PasskeysClient({ baseUrl: window.location.origin, collection, getCaptchaToken });
  try {
    const identity = pw.elements.identity.value.trim();
    if (!identity) {
      fail(t("js.oidc.enterIdentity"));
      return;
    }
    const res = await client.login(identity);
    if (res && res.token) return finish(res.token);
    if (!secondFactor(res)) fail(t("js.signin.passkeyIncomplete"));
  } catch (err) {
    if (err && err.status === 401 && secondFactor(err.response)) return;
    if (err && err.status === 400 && err.response && err.response.data && err.response.data.captchaToken) {
      fail(t("js.captcha.checkFailed"));
      return;
    }
    if (err && err.status === 429) {
      fail(t("js.tooMany"));
      return;
    }
    fail(err && /captcha/i.test(err.message || "") ? err.message : t("js.signin.passkeyIncomplete"));
  }
});

(async () => {
  try {
    const client = new PasskeysClient({ baseUrl: window.location.origin, collection });
    const methods = await client.methods();
    captchaCfg = methods.captcha || null;
    const passwordOn = !methods.password || methods.password.enabled !== false;
    pw.hidden = !passwordOn;
    const oauthOn = renderOAuth(methods);
    if (window.PublicKeyCredential && methods.webauthn && methods.webauthn.enabled) {
      passkeyBox.hidden = false;
      if (!passwordOn) {
        pw.hidden = false;
        pw.elements.password.closest("label").hidden = true;
        pw.elements.password.required = false;
        pw.querySelector("button").hidden = true;
      }
    }
    if (pw.hidden && passkeyBox.hidden && !oauthOn) fail(t("js.oidc.noMethod"));
  } catch (err) {
    pw.hidden = false;
  }
})();
`

const consentJS = `// the strings of the page, in the language the server negotiated (the JSON data block in the head)
let MESSAGES = {};
try { MESSAGES = JSON.parse(document.getElementById("i18n").textContent); } catch (e) { /* English keys show */ }
const t = (key) => (Object.prototype.hasOwnProperty.call(MESSAGES, key) ? MESSAGES[key] : key);

const root = document.getElementById("app");
const requestID = root.dataset.request;
const statusEl = root.querySelector("#status");
const buttons = root.querySelectorAll("button");

async function decide(allow) {
  buttons.forEach((b) => { b.disabled = true; });
  const res = await fetch("/oidc/consent", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ authRequestID: requestID, allow }),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok || !body.redirect) {
    statusEl.textContent = body.message || t("js.oidc.decisionFailed");
    statusEl.hidden = false;
    buttons.forEach((b) => { b.disabled = false; });
    return;
  }
  window.location.assign(body.redirect);
}

root.querySelector("#allow").addEventListener("click", () => decide(true));
root.querySelector("#deny").addEventListener("click", () => decide(false));
`
