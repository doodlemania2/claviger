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

// Where a message goes: next to the form it is about, and the fields of that form
// point at it (aria-describedby) and are marked invalid. No place given: the
// general message area at the top.
const PLACES = {
  login: { box: "loginMsg", fields: ["identity"] },
  password: { box: "pwMsg", fields: ["pwEmail", "pwPass"] },
  recovery: { box: "recoveryMsg", fields: ["recoveryEmail"] },
};
const ALL_FIELDS = ["identity", "pwEmail", "pwPass", "recoveryEmail"];

function showMsg(text, kind, place) {
  const boxes = ["msg"].concat(Object.values(PLACES).map((p) => p.box));
  for (const id of boxes) $(id).textContent = "";
  for (const id of ALL_FIELDS) {
    $(id).removeAttribute("aria-invalid");
    $(id).removeAttribute("aria-describedby");
  }
  if (!text) return;
  const where = PLACES[place];
  const box = $(where ? where.box : "msg");
  const div = document.createElement("div");
  div.className = "msg " + (kind || "ok");
  // an error interrupts the screen reader, a confirmation waits its turn
  div.setAttribute("role", kind === "err" ? "alert" : "status");
  div.textContent = text;
  box.appendChild(div);
  if (where && kind === "err") {
    for (const id of where.fields) {
      $(id).setAttribute("aria-invalid", "true");
      $(id).setAttribute("aria-describedby", where.box);
    }
  }
}
// The session lives in localStorage; when storage is blocked it lasts until the page closes.
let memoryAuth = null;
function loadAuth() {
  try { return JSON.parse(localStorage.getItem(STORAGE_KEY) || "null"); } catch (e) { return memoryAuth; }
}
function saveAuth(v) {
  memoryAuth = v || null;
  try {
    if (v) localStorage.setItem(STORAGE_KEY, JSON.stringify(v));
    else localStorage.removeItem(STORAGE_KEY);
  } catch (e) { /* storage blocked */ }
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
    $("btnRegister").focus();
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
    showMsg(t("js.passkeys.linkOnItsWay"), "ok", "recovery");
  } catch (e) {
    showMsg(t("js.passkeys.sendFailed", { error: e.message }), "err", "recovery");
  }
}

async function loginWithPasskey() {
  showMsg("");
  stopAutofill();
  try {
    const result = await client().login($("identity").value);
    saveAuth({ token: result.token, record: result.record });
    render();
    showMsg(t("js.passkeys.signedIn"), "ok");
    $("btnRegister").focus();
  } catch (e) {
    showMsg(t("js.passkeys.loginFailed", { error: e.message }), "err", "login");
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
    $("btnRegister").focus();
  } catch (e) {
    showMsg(t("js.passkeys.passwordFailed", { error: e.message }), "err", "password");
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
      renameBtn.onclick = async () => {
        const newName = prompt(t("js.passkeys.newName"), c.name || "");
        if (!newName) return;
        try { await client().rename(c.id, newName); loadCredentials(); }
        catch (e) { showMsg(t("js.passkeys.renameFailed", { error: e.message }), "err"); }
      };

      const delBtn = document.createElement("button");
      delBtn.className = "secondary";
      delBtn.textContent = t("js.passkeys.delete");
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
function reveal(buttonId, panelId, focusId) {
  $(buttonId).addEventListener("click", () => {
    $(panelId).hidden = false;
    $(buttonId).setAttribute("aria-expanded", "true");
    $(focusId).focus();
  });
}
reveal("linkPasswordLogin", "pwForm", "pwEmail");
reveal("linkRecovery", "recoveryForm", "recoveryEmail");
$("linkLogout").addEventListener("click", () => { saveAuth(null); showMsg(""); render(); setupMethods(); $("identity").focus(); });
$("btnRecovery").addEventListener("click", sendRecovery);

if (!window.PublicKeyCredential) {
  showMsg(t("js.passkeys.unsupported"), "err");
}
render();
if (!loadAuth()) setupMethods();
