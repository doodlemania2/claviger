// The staff page of recovery codes (#83): issue a code for a member who is in
// front of you, and confirm the request of a colleague for an owner or admin.
// Both need a fresh passkey assertion; the page asks for it when the server
// says so. The code is shown here once and kept nowhere.
import { session, el, api, when, createCaptcha, initSignIn, t } from "/orgs/assets/auth.js";
import { PasskeysClient } from "/api/passkeys-client.js";

const root = document.getElementById("app");
const slug = root.dataset.slug;
const collection = root.dataset.collection;

const signinPanel = root.querySelector("#signin");
const mainPanel = root.querySelector("#main");
const statusEl = root.querySelector("#pstatus");
const captcha = createCaptcha(root.querySelector("#captchaBox"));
const resultBox = root.querySelector("#result");

let org = null;
let me = null;

const STATE = {
  pending: "js.recovery.state.pending",
  active: "js.recovery.state.active",
};

function say(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = "msg " + (kind || "err");
  statusEl.hidden = !text;
}

const path = (suffix) => "/api/orgs/" + encodeURIComponent(org.id) + suffix;
const call = (suffix, opts) => api(path(suffix), Object.assign({ token: session.get() }, opts || {}));

function signOut() {
  session.clear();
  location.reload();
}

const needsStepUp = (r) => r.status === 403 && r.body && r.body.data && r.body.data.passkey && r.body.data.passkey.code === "passkey_step_up_required";

// stepped runs a request, and when the server wants a fresh passkey assertion
// asks for one and runs it again.
async function stepped(run) {
  let r = await run();
  if (!needsStepUp(r)) return r;
  try {
    const client = new PasskeysClient({ baseUrl: window.location.origin, collection, authToken: session.get(), onToken: (tok) => session.set(tok) });
    await client.stepUp();
  } catch (err) {
    return { ok: false, status: 0, body: { message: t("js.recovery.stepUpFailed") } };
  }
  r = await run();
  return r;
}

// drawQr puts the server-drawn SVG in the page as DOM nodes (never as a string
// of markup), so the strict Content-Security-Policy and the escaping of the
// page stay as they are. The link under it stays as the fallback.
function drawQr(box, svg, name) {
  if (!svg) return;
  const doc = new DOMParser().parseFromString(svg, "image/svg+xml");
  const node = doc.documentElement;
  if (!node || node.nodeName !== "svg" || doc.querySelector("parsererror")) return;
  const label = t("js.recovery.qrLabel", { name });
  node.setAttribute("aria-label", label);
  const title = node.querySelector("title");
  if (title) title.textContent = label;
  box.append(document.importNode(node, true));
  box.hidden = false;
}

function showResult(body, name) {
  const code = root.querySelector("#codeText");
  const note = root.querySelector("#resultNote");
  const extra = root.querySelector("#resultExtra");
  code.classList.remove("link");
  extra.textContent = "";
  const qrBox = root.querySelector("#qrBox");
  qrBox.textContent = "";
  qrBox.hidden = true;

  if (body.pending) {
    root.querySelector("#resultTitle").textContent = name;
    code.textContent = "";
    note.textContent = t("js.recovery.pending", { name });
  } else if (body.token) {
    root.querySelector("#resultTitle").textContent = t("js.recovery.linkFor", { name });
    code.classList.add("link");
    code.textContent = body.url;
    note.textContent = t("js.recovery.linkNote");
    drawQr(qrBox, body.qr, name);
  } else {
    root.querySelector("#resultTitle").textContent = t("js.recovery.codeFor", { name });
    code.textContent = body.code;
    note.textContent = t("js.recovery.readAloud");
  }
  if (!body.pending) {
    extra.textContent = t("js.recovery.validUntil", { time: when(body.expires) }) + " " +
      (body.emailSent ? t("js.recovery.emailSent") : t("js.recovery.emailNotSent"));
    if (body.channel === "sms") extra.textContent += " " + (body.smsSent ? t("js.recovery.smsSent") : t("js.recovery.smsNotSent"));
  }
  resultBox.hidden = false;
  resultBox.scrollIntoView({ block: "nearest" });
}

// ---- people and open requests ---------------------------------------------
let people = new Map();

async function loadPeople() {
  const r = await call("/members?perPage=200&status=active");
  const sel = root.querySelector("#person");
  sel.textContent = "";
  people = new Map();
  if (!r.ok) return;
  sel.append(el("option", { value: "" }, t("js.recovery.choose")));
  for (const m of r.body.items) {
    if (m.user === me.id) continue;
    const label = m.name || m.email || m.user;
    people.set(m.user, label);
    sel.append(el("option", { value: m.user }, label));
  }
}

async function loadOpen() {
  const r = await call("/recovery");
  const body = root.querySelector("#open tbody");
  body.textContent = "";
  if (r.status === 403) {
    root.querySelector("#issueForm").hidden = true;
    return say(t("js.recovery.notStaff"));
  }
  if (!r.ok) return;
  // no sender set up: do not offer the text message channel
  const smsOpt = root.querySelector('#issueForm option[value="sms"]');
  if (smsOpt) smsOpt.hidden = smsOpt.disabled = !r.body.sms;
  if (!r.body.items.length) {
    body.append(el("tr", {}, el("td", { colspan: 4, class: "muted" }, t("js.recovery.none"))));
    return;
  }
  for (const it of r.body.items) {
    const name = it.userName || people.get(it.user) || it.user;
    const action = it.state === "pending"
      ? (it.canConfirm ? el("button", { onclick: () => confirmRequest(it, name) }, t("js.recovery.confirm")) : el("span", { class: "muted" }, t("js.recovery.waitForOther")))
      : null;
    body.append(el("tr", {}, el("td", {}, name), el("td", {}, t(STATE[it.state] || STATE.active)), el("td", {}, when(it.expires)), el("td", {}, action)));
  }
}

async function confirmRequest(it, name) {
  say("");
  const r = await stepped(() => call("/recovery/" + encodeURIComponent(it.id) + "/confirm", { method: "POST" }));
  if (r.status === 401) return signOut();
  if (!r.ok) return say(r.body.message || t("js.admin.failed"));
  showResult(r.body, name);
  await loadOpen();
}

root.querySelector("#issueForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  say("");
  resultBox.hidden = true;
  const f = ev.target.elements;
  if (!f.user.value) return say(t("js.recovery.chooseFirst"));
  const body = { user: f.user.value, verification: f.verification.value, channel: f.channel.value, note: f.note.value };
  const r = await stepped(() => call("/recovery", { method: "POST", body }));
  if (r.status === 401) return signOut();
  if (!r.ok) return say(r.body.message || t("js.admin.failed"));
  f.note.value = "";
  showResult(r.body, people.get(f.user.value) || "");
  await loadOpen();
});

root.querySelector("#signout").addEventListener("click", signOut);

// ---- boot ------------------------------------------------------------------
async function enter(token) {
  const refreshed = await api("/api/collections/" + encodeURIComponent(collection) + "/auth-refresh", { method: "POST", token });
  if (!refreshed.ok) {
    session.clear();
    return false;
  }
  session.set(refreshed.body.token);
  me = refreshed.body.record;

  const r = await api("/api/orgs/" + encodeURIComponent(slug), { token: refreshed.body.token });
  if (!r.ok) {
    signinPanel.hidden = true;
    mainPanel.hidden = true;
    root.querySelector("#denied").hidden = false;
    root.querySelector("#deniedOut").onclick = signOut;
    return true;
  }
  org = r.body;
  root.querySelector("#orgName").textContent = org.name;
  document.title = org.name;
  signinPanel.hidden = true;
  mainPanel.hidden = false;
  await loadPeople();
  await loadOpen();
  return true;
}

(async () => {
  const tok = session.get();
  if (tok && (await enter(tok))) return;
  signinPanel.hidden = false;
  await initSignIn({
    root: signinPanel, collection, captcha,
    onToken: async (newTok) => {
      session.set(newTok);
      if (!(await enter(newTok))) say(t("js.admin.signInFailed"));
    },
  });
})();
