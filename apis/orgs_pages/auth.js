// Shared by the hosted organization pages (#79b): a tiny DOM helper, the
// session of the page, the optional captcha and the sign-in forms. It signs in
// through the ordinary auth endpoints of the member collection (password,
// TOTP second factor, passkey, email code), so whatever the collection
// requires ends in the same token as everywhere else.
import { PasskeysClient } from "/api/passkeys-client.js";

const KEY = "claviger_orgs_token";

export const session = {
  get() {
    try { return sessionStorage.getItem(KEY) || ""; } catch (e) { return ""; }
  },
  set(v) {
    try { sessionStorage.setItem(KEY, v); } catch (e) { /* private mode */ }
  },
  clear() {
    try { sessionStorage.removeItem(KEY); } catch (e) { /* private mode */ }
  },
};

// el("div", {class: "x", onclick: fn}, "text", child) builds nodes with
// textContent only: nothing is ever parsed as HTML.
export function el(tag, props, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (k === "class") node.className = v;
    else if (k === "value") node.value = v;
    else if (v === true) node.setAttribute(k, "");
    else node.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

export async function api(path, opts) {
  const o = opts || {};
  const headers = Object.assign({}, o.headers || {});
  if (o.body !== undefined) headers["Content-Type"] = "application/json";
  if (o.token) headers.Authorization = o.token;
  const res = await fetch(path, {
    method: o.method || (o.body !== undefined ? "POST" : "GET"),
    headers,
    body: o.body !== undefined ? JSON.stringify(o.body) : undefined,
  });
  let body = {};
  if (res.status !== 204) body = await res.json().catch(() => ({}));
  return { ok: res.ok, status: res.status, body };
}

// the stable code of an invitation refusal ({"data":{"invitation":{"code":...}}})
export function errCode(r) {
  return (r.body && r.body.data && r.body.data.invitation && r.body.data.invitation.code) || "";
}

export function when(v) {
  if (!v) return "";
  const d = new Date(String(v).replace(" ", "T"));
  return isNaN(d.getTime()) ? "" : d.toLocaleString();
}

// ---- optional captcha ----------------------------------------------------
// The provider script is loaded only when the collection requires a captcha
// for the action being performed. The server extends the page policy with the
// provider's domains in exactly the same case.
const CAPTCHA_SRC = {
  turnstile: () => "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit",
  hcaptcha: () => "https://js.hcaptcha.com/1/api.js?render=explicit",
  recaptchaV2: () => "https://www.google.com/recaptcha/api.js?render=explicit",
  recaptchaV3: (cfg) => "https://www.google.com/recaptcha/api.js?render=" + encodeURIComponent(cfg.siteKey),
};

export function createCaptcha(box) {
  let cfg = null;
  const scripts = {};

  function load(c) {
    const src = CAPTCHA_SRC[c.provider] && CAPTCHA_SRC[c.provider](c);
    if (!src) return Promise.reject(new Error("Unsupported captcha provider."));
    if (!scripts[src]) {
      scripts[src] = new Promise((resolve, reject) => {
        const s = document.createElement("script");
        s.src = src;
        s.async = true;
        s.onload = resolve;
        s.onerror = () => reject(new Error("Could not load the captcha."));
        document.head.appendChild(s);
      });
    }
    return scripts[src];
  }

  return {
    configure(c) { cfg = c || null; },
    async token(action) {
      if (!cfg || !cfg.required || cfg.required.indexOf(action) < 0) return undefined;
      await load(cfg);
      if (cfg.provider === "recaptchaV3") {
        await new Promise((r) => window.grecaptcha.ready(r));
        return window.grecaptcha.execute(cfg.siteKey, { action });
      }
      const widget = cfg.provider === "turnstile" ? window.turnstile : cfg.provider === "hcaptcha" ? window.hcaptcha : window.grecaptcha;
      if (cfg.provider === "recaptchaV2") await new Promise((r) => window.grecaptcha.ready(r));
      box.hidden = false;
      box.textContent = "";
      const holder = document.createElement("div");
      box.appendChild(holder);
      return new Promise((resolve, reject) => {
        const done = (fn, v) => { box.hidden = true; box.textContent = ""; fn(v); };
        widget.render(holder, {
          sitekey: cfg.siteKey,
          action: cfg.provider === "turnstile" ? action : undefined,
          callback: (t) => done(resolve, t),
          "error-callback": () => done(reject, new Error("Captcha failed, please try again.")),
          "expired-callback": () => done(reject, new Error("Captcha expired, please try again.")),
        });
      });
    },
  };
}

function isCaptchaFailure(r) {
  return r.status === 400 && r.body && r.body.data && r.body.data.captchaToken;
}

// ---- sign-in ---------------------------------------------------------------
// root holds #pw, #totp, #otpBox (#otpSend, #otpForm), #passkeyBox (#passkey),
// #captchaBox and #status. onToken(token, record) is called once.
export async function initSignIn({ root, collection, captcha, onToken, identity }) {
  const pw = root.querySelector("#pw");
  const totp = root.querySelector("#totp");
  const otpBox = root.querySelector("#otpBox");
  const otpForm = root.querySelector("#otpForm");
  const passkeyBox = root.querySelector("#passkeyBox");
  const statusEl = root.querySelector("#status");
  const base = "/api/collections/" + encodeURIComponent(collection);
  let mfaId = "";
  let otpId = "";

  const fail = (text) => { statusEl.textContent = text; statusEl.hidden = !text; };
  const tooMany = (r) => r.status === 429;

  if (identity) pw.elements.identity.value = identity;

  function secondFactor(body) {
    if (body && body.mfaId && body.totp) {
      mfaId = body.mfaId;
      pw.hidden = true;
      otpBox.hidden = true;
      passkeyBox.hidden = true;
      totp.hidden = false;
      totp.elements.code.focus();
      fail("");
      return true;
    }
    if (body && body.mfaId) {
      fail("This account needs a second sign-in step that this page does not support yet.");
      return true;
    }
    return false;
  }

  async function withCaptcha(action) {
    const headers = {};
    const t = await captcha.token(action);
    if (t) headers["X-Captcha-Token"] = t;
    return headers;
  }

  pw.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fail("");
    let headers;
    try { headers = await withCaptcha("login"); } catch (err) { return fail(err.message || "The captcha could not be completed."); }
    const r = await api(base + "/auth-with-password", { body: { identity: pw.elements.identity.value, password: pw.elements.password.value }, headers });
    if (r.status === 200 && r.body.token) return onToken(r.body.token, r.body.record);
    if (r.status === 401 && secondFactor(r.body)) return;
    if (isCaptchaFailure(r)) return fail("The captcha check failed. Try again.");
    fail(tooMany(r) ? "Too many attempts. Try again later." : "Wrong email or password.");
  });

  totp.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fail("");
    const r = await api(base + "/auth-with-totp", { body: { mfaId, code: totp.elements.code.value.trim() } });
    if (r.status === 200 && r.body.token) return onToken(r.body.token, r.body.record);
    fail(tooMany(r) ? "Too many attempts. Try again later." : "That code was not accepted.");
  });

  root.querySelector("#otpSend").addEventListener("click", async () => {
    fail("");
    const who = pw.elements.identity.value.trim();
    if (!who) return fail("Enter your email first.");
    let headers;
    try { headers = await withCaptcha("otp_request"); } catch (err) { return fail(err.message || "The captcha could not be completed."); }
    const r = await api(base + "/request-otp", { body: { email: who }, headers });
    if (!r.ok || !r.body.otpId) return fail(tooMany(r) ? "Too many attempts. Try again later." : "The code could not be sent.");
    otpId = r.body.otpId;
    otpForm.hidden = false;
    otpForm.elements.code.focus();
  });

  otpForm.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fail("");
    const r = await api(base + "/auth-with-otp", { body: { otpId, password: otpForm.elements.code.value.trim() } });
    if (r.status === 200 && r.body.token) return onToken(r.body.token, r.body.record);
    if (r.status === 401 && secondFactor(r.body)) return;
    fail(tooMany(r) ? "Too many attempts. Try again later." : "That code was not accepted.");
  });

  root.querySelector("#passkey").addEventListener("click", async () => {
    fail("");
    const who = pw.elements.identity.value.trim();
    if (!who) return fail("Enter your email first.");
    const client = new PasskeysClient({ baseUrl: window.location.origin, collection, getCaptchaToken: (a) => captcha.token(a) });
    try {
      const res = await client.login(who);
      if (res && res.token) return onToken(res.token, res.record);
      if (!secondFactor(res)) fail("The passkey sign-in did not complete.");
    } catch (err) {
      if (err && err.status === 401 && secondFactor(err.response)) return;
      if (err && err.status === 429) return fail("Too many attempts. Try again later.");
      fail("The passkey sign-in did not complete.");
    }
  });

  try {
    const client = new PasskeysClient({ baseUrl: window.location.origin, collection });
    const methods = await client.methods();
    captcha.configure(methods.captcha);
    const passwordOn = !methods.password || methods.password.enabled !== false;
    const passkeyOn = !!(window.PublicKeyCredential && methods.webauthn && methods.webauthn.enabled);
    const otpOn = !!(methods.otp && methods.otp.enabled);
    pw.hidden = false;
    otpBox.hidden = !otpOn;
    passkeyBox.hidden = !passkeyOn;
    if (!passwordOn) {
      pw.elements.password.closest("label").hidden = true;
      pw.elements.password.required = false;
      pw.querySelector("button").hidden = true;
      if (!otpOn && !passkeyOn) fail("No sign-in method is available.");
    }
  } catch (err) {
    pw.hidden = false;
  }
}
