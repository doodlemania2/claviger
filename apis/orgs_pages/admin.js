import { session, el, api, when, createCaptcha, initSignIn, t as tx, word } from "/orgs/assets/auth.js";

const root = document.getElementById("app");
const slug = root.dataset.slug;
const collection = root.dataset.collection;
const RANK = { owner: 4, admin: 3, member: 2, viewer: 1 };
const ROLES = ["owner", "admin", "member", "viewer"];

const signinPanel = root.querySelector("#signin");
const mainPanel = root.querySelector("#main");
const statusEl = root.querySelector("#pstatus");
const captcha = createCaptcha(root.querySelector("#captchaBox"));

let org = null;
let me = null; // the signed-in record
let canManage = false;
let myRank = 0;
let tags = [];
let recoveryLinked = false;
let consentsLinked = false;

function say(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = "msg " + (kind || "err");
  statusEl.setAttribute("role", kind === "ok" ? "status" : "alert");
  statusEl.hidden = !text;
}

const path = (suffix) => "/api/orgs/" + encodeURIComponent(org.id) + suffix;
const call = (suffix, opts) => api(path(suffix), Object.assign({ token: session.get() }, opts || {}));

function signOut() {
  session.clear();
  org = null;
  location.reload();
}

function splitTags(text) {
  return text.split(",").map((s) => s.trim()).filter(Boolean);
}

function roleSelect(current, max) {
  const sel = el("select", {}, ROLES.filter((r) => RANK[r] <= max || r === current).map((r) => el("option", { value: r }, word("role", r))));
  sel.value = current;
  return sel;
}

async function guard(r, okText) {
  if (r.ok) {
    if (okText) say(okText, "ok");
    return true;
  }
  if (r.status === 401) return signOut(), false;
  say(r.body.message || tx("js.admin.failed"));
  return false;
}

// ---- members ---------------------------------------------------------------
async function loadMembers() {
  const r = await call("/members?perPage=200");
  const body = root.querySelector("#members tbody");
  body.textContent = "";
  if (!r.ok) return guard(r);
  const mine = r.body.items.find((m) => me && m.user === me.id);
  if (mine && !recoveryLinked && (canManage || (mine.tags || []).includes("greeter"))) {
    // staff of the recovery codes (#83) get a way to the page
    recoveryLinked = true;
    root.querySelector("#myRole").after(el("a", { class: "recoverylink", href: "/orgs/recovery/" + encodeURIComponent(slug) }, tx("js.admin.recoveryLink")));
  }
  if (mine && !consentsLinked && (canManage || (mine.tags || []).includes("registrar"))) {
    // owners, admins and registrars manage the consent purposes of the org (#88)
    consentsLinked = true;
    root.querySelector("#myRole").after(el("a", { class: "recoverylink", href: "/orgs/consents/" + encodeURIComponent(slug) }, tx("js.admin.consentsLink")));
  }
  for (const m of r.body.items) {
    const isSelf = me && m.user === me.id;
    const editable = canManage && RANK[m.role] <= myRank;
    const who = el("td", { id: "m-" + m.id }, m.name || m.email || m.user, m.name && m.email ? el("div", { class: "muted" }, m.email) : null);
    if (!editable) {
      body.append(el("tr", {}, who, el("td", {}, word("role", m.role)), el("td", {}, word("status", m.status)), el("td", {}, (m.tags || []).join(", ")),
        el("td", {}, canManage && isSelf ? el("button", { class: "secondary", "aria-describedby": "m-" + m.id, onclick: () => removeMember(m, true) }, tx("js.admin.leave")) : null)));
      continue;
    }
    const role = roleSelect(m.role, myRank);
    role.setAttribute("aria-label", tx("orgs.col.role"));
    role.setAttribute("aria-describedby", "m-" + m.id);
    const status = el("select", { disabled: isSelf, "aria-label": tx("orgs.col.status"), "aria-describedby": "m-" + m.id }, el("option", { value: "active" }, word("status", "active")), el("option", { value: "suspended" }, word("status", "suspended")));
    status.value = m.status;
    const tagInput = el("input", { value: (m.tags || []).join(", "), "aria-label": tx("js.admin.tags"), "aria-describedby": "m-" + m.id });
    body.append(el("tr", {}, who, el("td", {}, role), el("td", {}, status), el("td", {}, tagInput),
      el("td", {},
        el("button", { "aria-describedby": "m-" + m.id, onclick: () => saveMember(m, role.value, status.value, splitTags(tagInput.value)) }, tx("js.admin.save")),
        el("button", { class: "secondary", "aria-describedby": "m-" + m.id, onclick: () => removeMember(m, isSelf) }, isSelf ? tx("js.admin.leave") : tx("js.admin.remove")))));
  }
}

async function saveMember(m, role, status, tagNames) {
  say("");
  const patch = { tags: tagNames };
  if (role !== m.role) patch.role = role;
  if (status !== m.status) patch.status = status;
  if (await guard(await call("/members/" + encodeURIComponent(m.id), { method: "PATCH", body: patch }), tx("js.admin.saved"))) {
    await Promise.all([loadMembers(), loadTags()]);
  }
}

async function removeMember(m, self) {
  if (!confirm(self ? tx("js.admin.confirmLeave") : tx("js.admin.confirmRemove"))) return;
  say("");
  if (await guard(await call("/members/" + encodeURIComponent(m.id), { method: "DELETE" }), self ? "" : tx("js.admin.removed"))) {
    if (self) return signOut();
    await loadMembers();
  }
}

// ---- invitations -----------------------------------------------------------
async function loadInvitations() {
  const r = await call("/invitations?perPage=200");
  const body = root.querySelector("#invitations tbody");
  body.textContent = "";
  if (!r.ok) return guard(r);
  for (const inv of r.body.items) {
    const live = inv.status === "pending" || inv.status === "expired";
    const mine = RANK[inv.role] <= myRank;
    body.append(el("tr", {}, el("td", { id: "i-" + inv.id }, inv.email), el("td", {}, word("role", inv.role)), el("td", {}, (inv.tags || []).join(", ")),
      el("td", {}, word("status", inv.status)), el("td", {}, inv.status === "accepted" ? when(inv.acceptedAt) : when(inv.expires)), el("td", {}, String(inv.sendCount)),
      el("td", {},
        live && mine ? el("button", { "aria-describedby": "i-" + inv.id, onclick: () => resend(inv) }, tx("js.admin.resend")) : null,
        inv.status === "pending" && mine ? el("button", { class: "secondary", "aria-describedby": "i-" + inv.id, onclick: () => revoke(inv) }, tx("js.admin.revoke")) : null)));
  }
}

async function resend(inv) {
  say("");
  const r = await call("/invitations/" + encodeURIComponent(inv.id) + "/resend", { method: "POST" });
  if (await guard(r, r.body.emailSent === false ? tx("js.admin.renewedNoEmail") : tx("js.admin.sentAgain"))) await loadInvitations();
}

async function revoke(inv) {
  if (!confirm(tx("js.admin.confirmRevokeInvitation", { email: inv.email }))) return;
  say("");
  if (await guard(await call("/invitations/" + encodeURIComponent(inv.id), { method: "DELETE" }), tx("js.admin.revoked"))) await loadInvitations();
}

async function invite(ev) {
  ev.preventDefault();
  say("");
  const f = ev.target.elements;
  const r = await call("/invitations", { method: "POST", body: { email: f.email.value, role: f.role.value, tags: splitTags(f.tags.value) } });
  if (await guard(r, r.body.emailSent === false ? tx("js.admin.invitedNoEmail") : tx("js.admin.invited"))) {
    f.email.value = "";
    f.tags.value = "";
    await Promise.all([loadInvitations(), loadTags()]);
  }
}

// ---- SCIM --------------------------------------------------------------------
const scimTokenText = root.querySelector("#scimNew");

async function loadScim() {
  const r = await call("/scim-tokens");
  const body = root.querySelector("#scimTokens tbody");
  body.textContent = "";
  if (!r.ok) return guard(r);
  root.querySelector("#scimUrl").textContent = r.body.endpoint;
  for (const t of r.body.items) {
    body.append(el("tr", {}, el("td", { id: "s-" + t.id }, t.name), el("td", {}, el("code", {}, "scim_" + t.prefix + "_...")),
      el("td", {}, when(t.created)), el("td", {}, t.lastUsed ? when(t.lastUsed) : tx("js.admin.never")), el("td", {}, t.expires ? when(t.expires) : tx("js.admin.never")),
      el("td", {}, word("status", t.revoked ? "revoked" : t.active ? "active" : "expired")),
      el("td", {}, t.active ? el("button", { class: "secondary", "aria-describedby": "s-" + t.id, onclick: () => revokeScim(t) }, tx("js.admin.revoke")) : null)));
  }
  const s = await call("/scim-settings");
  if (!s.ok) return guard(s);
  const f = root.querySelector("#scimSettings").elements;
  f.defaultRole.value = s.body.defaultRole;
  f.groupRoles.value = Object.entries(s.body.groupRoles || {}).map(([g, role]) => g + " = " + role).join("\n");
  f.deactivationBans.checked = !!s.body.deactivationBans;
}

async function createScim(ev) {
  ev.preventDefault();
  say("");
  scimTokenText.hidden = true;
  const input = ev.target.elements.name;
  const r = await call("/scim-tokens", { method: "POST", body: { name: input.value } });
  if (await guard(r)) {
    input.value = "";
    scimTokenText.textContent = tx("js.admin.copyToken", { token: r.body.token });
    scimTokenText.hidden = false;
    await loadScim();
  }
}

async function revokeScim(t) {
  if (!confirm(tx("js.admin.confirmRevokeToken", { name: t.name }))) return;
  say("");
  if (await guard(await call("/scim-tokens/" + encodeURIComponent(t.id), { method: "DELETE" }), tx("js.admin.revoked"))) await loadScim();
}

async function saveScim(ev) {
  ev.preventDefault();
  say("");
  const f = ev.target.elements;
  const groupRoles = {};
  for (const line of f.groupRoles.value.split("\n")) {
    const i = line.lastIndexOf("=");
    if (i > 0) groupRoles[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    else if (line.trim()) return say(tx("js.admin.groupFormat"));
  }
  await guard(await call("/scim-settings", { method: "PUT", body: { defaultRole: f.defaultRole.value, groupRoles, deactivationBans: f.deactivationBans.checked } }), tx("js.admin.saved"));
}

// ---- tags --------------------------------------------------------------------
async function loadTags() {
  const r = await call("/tags");
  if (!r.ok) return guard(r);
  tags = r.body.items;
  const list = root.querySelector("#taglist");
  list.textContent = "";
  for (const t of tags) {
    list.append(el("span", { class: "tag", title: t.description || "" }, t.name, canManage ? " " : null,
      canManage ? el("button", { class: "linklike", "aria-label": tx("js.admin.deleteTag", { name: t.name }), onclick: () => deleteTag(t) }, "x") : null));
  }
  if (!tags.length) list.append(el("span", { class: "muted" }, tx("js.admin.noTags")));
}

async function addTag(ev) {
  ev.preventDefault();
  say("");
  const input = ev.target.elements.name;
  if (await guard(await call("/tags", { method: "POST", body: { name: input.value } }), tx("js.admin.tagAdded"))) {
    input.value = "";
    await loadTags();
  }
}

async function deleteTag(t) {
  if (!confirm(tx("js.admin.confirmDeleteTag", { name: t.name }))) return;
  say("");
  if (await guard(await call("/tags/" + encodeURIComponent(t.id), { method: "DELETE" }), tx("js.admin.tagDeleted"))) {
    await Promise.all([loadTags(), loadMembers(), loadInvitations()]);
  }
}

// ---- boot --------------------------------------------------------------------
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
  myRank = RANK[org.role] || 0;
  canManage = myRank >= RANK.admin;

  root.querySelector("#orgName").textContent = org.name;
  root.querySelector("#myRole").textContent = tx("js.admin.youAre." + org.role);
  document.title = org.name;
  for (const s of root.querySelectorAll("[data-manage]")) s.hidden = !canManage;
  root.querySelector("#inviteRole").replaceWith(Object.assign(roleSelect("member", myRank), { id: "inviteRole", name: "role" }));
  root.querySelector("#scimDefaultRole").replaceWith(Object.assign(roleSelect("member", Math.min(myRank, RANK.admin)), { id: "scimDefaultRole", name: "defaultRole" }));

  signinPanel.hidden = true;
  mainPanel.hidden = false;
  await Promise.all([loadMembers(), canManage ? loadInvitations() : null, canManage ? loadScim() : null, loadTags()]);
  return true;
}

root.querySelector("#signout").addEventListener("click", signOut);
root.querySelector("#inviteForm").addEventListener("submit", invite);
root.querySelector("#tagForm").addEventListener("submit", addTag);
root.querySelector("#scimTokenForm").addEventListener("submit", createScim);
root.querySelector("#scimSettings").addEventListener("submit", saveScim);

(async () => {
  const t = session.get();
  if (t && (await enter(t))) return;
  signinPanel.hidden = false;
  await initSignIn({
    root: signinPanel, collection, captcha,
    onToken: async (tok) => {
      session.set(tok);
      if (!(await enter(tok))) say(tx("js.admin.signInFailed"));
    },
  });
})();
