import { session, el, api, errCode, createCaptcha, initSignIn, t, word } from "/orgs/assets/auth.js";

const root = document.getElementById("app");
const d = root.dataset;
const collection = d.collection;
const token = d.token;
const base = "/api/collections/" + encodeURIComponent(collection);

const panels = {
  signedIn: root.querySelector("#signedIn"),
  signIn: root.querySelector("#signin"),
  signUp: root.querySelector("#signup"),
  done: root.querySelector("#done"),
};
const statusEl = root.querySelector("#istatus");
const captcha = createCaptcha(root.querySelector("#captchaBox"));
let record = null;

function show(name) {
  for (const [k, p] of Object.entries(panels)) p.hidden = k !== name;
}

function say(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = "msg " + (kind || "err");
  statusEl.hidden = !text;
}

function finished(res) {
  const link = root.querySelector("#doneLink");
  link.href = "/orgs/" + encodeURIComponent(res.org.slug);
  root.querySelector("#doneText").textContent = t("js.invite.joined", { org: res.org.name, role: word("role", res.role) });
  say("");
  show("done");
}

function signedIn(rec) {
  record = rec;
  root.querySelector("#who").textContent = rec.email || t("js.invite.yourAccount");
  root.querySelector("#verifyBtn").hidden = true;
  show("signedIn");
}

function onToken(tok, rec) {
  session.set(tok);
  say("");
  signedIn(rec || {});
}

root.querySelector("#accept").addEventListener("click", async () => {
  say("");
  const r = await api("/api/invitations/accept", { token: session.get(), body: { token } });
  if (r.ok) return finished(r.body);
  say(r.body.message || t("js.invite.acceptFailed"));
  if (errCode(r) === "email_not_verified") root.querySelector("#verifyBtn").hidden = false;
});

root.querySelector("#verifyBtn").addEventListener("click", async () => {
  const headers = {};
  try {
    const t = await captcha.token("verification");
    if (t) headers["X-Captcha-Token"] = t;
  } catch (err) {
    return say(err.message || t("js.captcha.incomplete"));
  }
  const r = await api(base + "/request-verification", { body: { email: record.email }, headers });
  if (r.ok || r.status === 204) say(t("js.invite.checkInbox"), "ok");
  else say(r.status === 429 ? t("js.tooMany") : t("js.invite.emailNotSent"));
});

root.querySelector("#signout").addEventListener("click", () => {
  session.clear();
  record = null;
  say("");
  show(d.hasAccount === "true" ? "signIn" : "signUp");
});

const toSignUp = root.querySelector("#toSignUp");
if (toSignUp) toSignUp.addEventListener("click", () => { say(""); show("signUp"); });
root.querySelector("#toSignIn").addEventListener("click", () => { say(""); show("signIn"); });

const signup = root.querySelector("#signupForm");
signup.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  say("");
  const headers = {};
  try {
    const t = await captcha.token("signup");
    if (t) headers["X-Captcha-Token"] = t;
  } catch (err) {
    return say(err.message || t("js.captcha.incomplete"));
  }
  const f = signup.elements;
  const r = await api("/api/invitations/register", {
    headers,
    body: { token, password: f.password.value, passwordConfirm: f.passwordConfirm.value, name: f.name.value },
  });
  if (!r.ok) {
    const first = r.body.data && Object.values(r.body.data)[0];
    if (errCode(r) === "account_exists") {
      show("signIn");
    }
    return say((first && first.message) || r.body.message || t("js.invite.createFailed"));
  }
  // the account exists and the invitation is accepted: sign in with the ordinary endpoint
  const login = await api(base + "/auth-with-password", { body: { identity: d.email, password: f.password.value } });
  if (login.ok && login.body.token) session.set(login.body.token);
  f.password.value = "";
  f.passwordConfirm.value = "";
  finished(r.body);
});

(async () => {
  await initSignIn({ root: panels.signIn, collection, captcha, onToken, identity: d.email });
  const t = session.get();
  if (t) {
    const r = await api(base + "/auth-refresh", { method: "POST", token: t });
    if (r.ok && r.body.token) {
      session.set(r.body.token);
      return signedIn(r.body.record);
    }
    session.clear();
  }
  show(d.hasAccount === "true" ? "signIn" : "signUp");
})();
