// The consent purposes page of an org (#88): staff (owners, admins and
// registrars) list, create, edit, retire and delete the purposes of their own
// org. Everything goes through /api/orgs/{id}/consent-purposes; the server
// decides who may do what, the page only shows it. Global purposes (no org) are
// listed for reference and cannot be changed here.
import { session, el, api, createCaptcha, initSignIn, t } from "/orgs/assets/auth.js";

const root = document.getElementById("app");
const slug = root.dataset.slug;
const collection = root.dataset.collection;

const signinPanel = root.querySelector("#signin");
const mainPanel = root.querySelector("#main");
const statusEl = root.querySelector("#pstatus");
const loadingEl = root.querySelector("#loading");
const emptyEl = root.querySelector("#empty");
const form = root.querySelector("#purposeForm");
const captcha = createCaptcha(root.querySelector("#captchaBox"));

let org = null;
let editing = null; // the purpose being edited, or null for a new one

const BASES = [
  ["consent", "js.consentPurposes.basis.consent"],
  ["legitimate_interest", "js.consentPurposes.basis.legitimate_interest"],
  ["legal_obligation", "js.consentPurposes.basis.legal_obligation"],
  ["contract", "js.consentPurposes.basis.contract"],
  ["vital_interest", "js.consentPurposes.basis.vital_interest"],
  ["public_task", "js.consentPurposes.basis.public_task"],
];

function say(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = "msg " + (kind || "err");
  statusEl.setAttribute("role", kind === "ok" ? "status" : "alert");
  statusEl.hidden = !text;
  if (text && kind !== "ok") statusEl.focus();
}

const path = (suffix) => "/api/orgs/" + encodeURIComponent(org.id) + "/consent-purposes" + (suffix || "");
const call = (suffix, opts) => api(path(suffix), Object.assign({ token: session.get() }, opts || {}));

function signOut() {
  session.clear();
  location.reload();
}

function deny(text) {
  signinPanel.hidden = true;
  mainPanel.hidden = true;
  root.querySelector("#deniedText").textContent = text;
  root.querySelector("#denied").hidden = false;
  root.querySelector("#deniedOut").onclick = signOut;
}

// a refusal of the server, in the language of the page where the page knows it
function refusal(r, fallback) {
  if (r.status === 401) return signOut();
  if (r.status === 409) return say(t("js.consentPurposes.hasRecords"));
  say(r.body && r.body.message ? r.body.message : t(fallback));
}

// ---- the form ---------------------------------------------------------------
function resetForm(p) {
  editing = p || null;
  const f = form.elements;
  f.key.value = p ? p.key : "";
  f.key.disabled = !!p;
  f.name.value = p ? p.name : "";
  f.text.value = p ? p.text : "";
  f.lawfulBasis.value = p ? p.lawfulBasis : "consent";
  f.minAge.value = p && p.minAge ? String(p.minAge) : "";
  f.requiresGuardian.checked = !!(p && p.requiresGuardian);
  f.marketing.checked = !!(p && p.marketing);
  form.querySelector("#formTitle").textContent = p ? t("js.consentPurposes.formEdit", { name: p.name }) : t("js.consentPurposes.formNew");
  root.querySelector("#fSave").textContent = p ? t("js.consentPurposes.save") : t("js.consentPurposes.create");
  root.querySelector("#fCancel").hidden = !p;
}

function startEdit(p) {
  say("");
  resetForm(p);
  form.elements.name.focus();
}

async function submit(ev) {
  ev.preventDefault();
  say("");
  const f = form.elements;
  const body = {
    name: f.name.value,
    text: f.text.value,
    lawfulBasis: f.lawfulBasis.value,
    requiresGuardian: f.requiresGuardian.checked,
    marketing: f.marketing.checked,
  };
  if (f.minAge.value !== "") body.minAge = Number(f.minAge.value);
  else if (editing) body.minAge = 0;
  let r;
  if (editing) {
    r = await call("/" + encodeURIComponent(editing.id), { method: "PATCH", body });
  } else {
    body.key = f.key.value;
    r = await call("", { method: "POST", body });
  }
  if (!r.ok) return refusal(r, "js.admin.failed");
  const bumped = editing && r.body.version > editing.version;
  say(editing ? (bumped ? t("js.consentPurposes.savedVersion", { version: r.body.version }) : t("js.consentPurposes.saved")) : t("js.consentPurposes.created"), "ok");
  resetForm(null);
  await load();
}

// ---- the list ---------------------------------------------------------------
async function setStatus(p, status) {
  say("");
  const r = await call("/" + encodeURIComponent(p.id), { method: "PATCH", body: { status } });
  if (!r.ok) return refusal(r, "js.admin.failed");
  say(status === "archived" ? t("js.consentPurposes.retired") : t("js.consentPurposes.reactivated"), "ok");
  await load();
}

async function remove(p) {
  if (!confirm(t("js.consentPurposes.confirmDelete", { name: p.name }))) return;
  say("");
  const r = await call("/" + encodeURIComponent(p.id), { method: "DELETE" });
  if (!r.ok) return refusal(r, "js.admin.failed");
  if (editing && editing.id === p.id) resetForm(null);
  say(t("js.consentPurposes.deleted"), "ok");
  await load();
}

function row(p) {
  const own = p.org === org.id;
  const id = "p-" + p.id;
  const archived = p.status === "archived";
  const actions = own
    ? el("td", {},
        el("button", { type: "button", "aria-describedby": id, onclick: () => startEdit(p) }, t("js.consentPurposes.edit")),
        el("button", { type: "button", class: "secondary", "aria-describedby": id, onclick: () => setStatus(p, archived ? "active" : "archived") },
          archived ? t("js.consentPurposes.reactivate") : t("js.consentPurposes.retire")),
        el("button", { type: "button", class: "secondary", "aria-describedby": id, onclick: () => remove(p) }, t("js.consentPurposes.delete")))
    : el("td", {}, el("span", { class: "muted" }, t("js.consentPurposes.readOnly")));
  return el("tr", { "data-purpose-key": p.key, "data-scope": own ? "org" : "global" },
    el("td", { id }, p.name, p.marketing ? el("span", { class: "tag" }, t("js.consentPurposes.marketingTag")) : null),
    el("td", {}, el("code", {}, p.key)),
    el("td", {}, String(p.version)),
    el("td", {}, own ? t("js.consentPurposes.scopeOrg") : t("js.consentPurposes.scopeGlobal")),
    el("td", {}, archived ? t("js.consentPurposes.status.archived") : t("js.consentPurposes.status.active")),
    actions);
}

async function load() {
  loadingEl.hidden = false;
  const r = await call("");
  loadingEl.hidden = true;
  if (r.status === 401) return signOut();
  if (r.status === 403) return deny(t("js.consentPurposes.noAccess"));
  if (r.status === 404) return deny(t("js.consentPurposes.unavailable"));
  if (!r.ok) return say(t("js.consentPurposes.loadFailed"));
  const body = root.querySelector("#purposes tbody");
  body.textContent = "";
  const items = r.body.items || [];
  emptyEl.hidden = items.length > 0;
  for (const p of items) body.append(row(p));
}

// ---- boot --------------------------------------------------------------------
async function enter(token) {
  const refreshed = await api("/api/collections/" + encodeURIComponent(collection) + "/auth-refresh", { method: "POST", token });
  if (!refreshed.ok) {
    session.clear();
    return false;
  }
  session.set(refreshed.body.token);

  const r = await api("/api/orgs/" + encodeURIComponent(slug), { token: refreshed.body.token });
  if (!r.ok) {
    deny(t("js.consentPurposes.notMember"));
    return true;
  }
  org = r.body;
  root.querySelector("#orgName").textContent = org.name;
  document.title = org.name + " - " + document.title;

  signinPanel.hidden = true;
  mainPanel.hidden = false;
  resetForm(null);
  await load();
  return true;
}

const basisSelect = root.querySelector("#fBasis");
for (const [value, key] of BASES) basisSelect.append(el("option", { value }, t(key)));

root.querySelector("#signout").addEventListener("click", signOut);
root.querySelector("#fCancel").addEventListener("click", () => {
  say("");
  resetForm(null);
});
form.addEventListener("submit", submit);

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
