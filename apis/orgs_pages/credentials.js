// "My credentials" (#81): the safeguarding credentials of the signed-in person,
// their dates and why a list might look empty. Read-only. Everything comes from
// /api/me/access-status; nothing is cached or kept beyond this page.
import { session, el, api, t, createCaptcha, initSignIn } from "/orgs/assets/auth.js";

const root = document.getElementById("app");
const collection = root.dataset.collection;
const signinPanel = root.querySelector("#signin");
const mainPanel = root.querySelector("#main");
const statusEl = root.querySelector("#cstatus");
const listEl = root.querySelector("#credentials");
const loadingEl = root.querySelector("#loading");
const captcha = createCaptcha(root.querySelector("#captchaBox"));

function say(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = "msg " + (kind || "err");
  statusEl.hidden = !text;
  if (text && kind !== "ok") statusEl.focus();
}

function signOut() {
  session.clear();
  location.reload();
}

function day(v) {
  if (!v) return "";
  const d = new Date(String(v).replace(" ", "T"));
  const lang = document.documentElement.lang;
  return isNaN(d.getTime()) ? "" : d.toLocaleDateString(lang && lang !== "en" ? lang : undefined);
}

const STANDINGS = ["cleared", "expiring", "pending", "lapsed", "revoked", "missing"];

// the sentence under a credential: kind, and never a judgement
function detail(row) {
  const args = { date: day(row.expires), days: row.daysLeft, org: row.issuedByOrg || "" };
  let key = "js.credentials.detail." + row.standing;
  if (row.standing === "expiring") {
    if (row.daysLeft <= 0) key += "Today";
    else if (row.daysLeft === 1) key += "One";
  }
  return t(STANDINGS.includes(row.standing) ? key : "js.credentials.detail.missing", args);
}

function typePart(row) {
  const standing = STANDINGS.includes(row.standing) ? row.standing : "missing";
  const li = el("li", { class: "cred", "data-standing": standing },
    el("strong", {}, row.name),
    el("span", { class: "tag" }, t("js.credentials.standing." + standing)),
    el("p", { class: "muted" }, detail(row)));
  return li;
}

function orgPart(o, index) {
  const hid = "co-" + index;
  const sec = el("section", { class: "hh", "aria-labelledby": hid }, el("h2", { id: hid }, o.name));
  const types = o.types || [];
  if (!types.length) {
    sec.append(el("p", { class: "muted" }, t("js.credentials.noTypes")));
  } else {
    const ul = el("ul", { class: "creds" });
    for (const row of types) ul.append(typePart(row));
    sec.append(ul);
  }
  const names = (o.coordinators || []).map((c) => c.name).filter(Boolean);
  sec.append(el("p", { class: "muted" },
    names.length ? t("js.credentials.contact", { names: names.join(", ") }) : t("js.credentials.contactOffice")));
  return sec;
}

async function load() {
  say("");
  loadingEl.hidden = false;
  listEl.textContent = "";
  const r = await api("/api/me/access-status", { token: session.get() });
  loadingEl.hidden = true;
  if (r.status === 401) return signOut();
  if (r.status === 404) return say(t("js.credentials.unavailable"));
  if (!r.ok) return say(t("js.credentials.loadFailed"));
  const orgs = r.body.orgs || [];
  if (!orgs.length) {
    listEl.append(el("p", { class: "muted" }, t("js.credentials.none")));
    return;
  }
  orgs.forEach((o, i) => listEl.append(orgPart(o, i)));
  listEl.append(el("p", { class: "muted" }, t("js.credentials.emptyListNote")));
}

async function enter(token) {
  const refreshed = await api("/api/collections/" + encodeURIComponent(collection) + "/auth-refresh", { method: "POST", token });
  if (!refreshed.ok) {
    session.clear();
    return false;
  }
  session.set(refreshed.body.token);
  signinPanel.hidden = true;
  mainPanel.hidden = false;
  await load();
  return true;
}

root.querySelector("#signout").addEventListener("click", signOut);

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
