import { session, el, api, when, createCaptcha, initSignIn } from "/orgs/assets/auth.js";

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

function say(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = "msg " + (kind || "err");
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
  const sel = el("select", {}, ROLES.filter((r) => RANK[r] <= max || r === current).map((r) => el("option", { value: r }, r)));
  sel.value = current;
  return sel;
}

async function guard(r, okText) {
  if (r.ok) {
    if (okText) say(okText, "ok");
    return true;
  }
  if (r.status === 401) return signOut(), false;
  say(r.body.message || "That did not work.");
  return false;
}

// ---- members ---------------------------------------------------------------
async function loadMembers() {
  const r = await call("/members?perPage=200");
  const body = root.querySelector("#members tbody");
  body.textContent = "";
  if (!r.ok) return guard(r);
  for (const m of r.body.items) {
    const isSelf = me && m.user === me.id;
    const editable = canManage && RANK[m.role] <= myRank;
    const who = el("td", {}, m.name || m.email || m.user, m.name && m.email ? el("div", { class: "muted" }, m.email) : null);
    if (!editable) {
      body.append(el("tr", {}, who, el("td", {}, m.role), el("td", {}, m.status), el("td", {}, (m.tags || []).join(", ")),
        el("td", {}, canManage && isSelf ? el("button", { class: "secondary", onclick: () => removeMember(m, true) }, "Leave") : null)));
      continue;
    }
    const role = roleSelect(m.role, myRank);
    const status = el("select", { disabled: isSelf }, el("option", { value: "active" }, "active"), el("option", { value: "suspended" }, "suspended"));
    status.value = m.status;
    const tagInput = el("input", { value: (m.tags || []).join(", "), "aria-label": "Tags" });
    body.append(el("tr", {}, who, el("td", {}, role), el("td", {}, status), el("td", {}, tagInput),
      el("td", {},
        el("button", { onclick: () => saveMember(m, role.value, status.value, splitTags(tagInput.value)) }, "Save"),
        el("button", { class: "secondary", onclick: () => removeMember(m, isSelf) }, isSelf ? "Leave" : "Remove"))));
  }
}

async function saveMember(m, role, status, tagNames) {
  say("");
  const patch = { tags: tagNames };
  if (role !== m.role) patch.role = role;
  if (status !== m.status) patch.status = status;
  if (await guard(await call("/members/" + encodeURIComponent(m.id), { method: "PATCH", body: patch }), "Saved.")) {
    await Promise.all([loadMembers(), loadTags()]);
  }
}

async function removeMember(m, self) {
  if (!confirm(self ? "Leave this organization?" : "Remove this member?")) return;
  say("");
  if (await guard(await call("/members/" + encodeURIComponent(m.id), { method: "DELETE" }), self ? "" : "Removed.")) {
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
    body.append(el("tr", {}, el("td", {}, inv.email), el("td", {}, inv.role), el("td", {}, (inv.tags || []).join(", ")),
      el("td", {}, inv.status), el("td", {}, inv.status === "accepted" ? when(inv.acceptedAt) : when(inv.expires)), el("td", {}, String(inv.sendCount)),
      el("td", {},
        live && mine ? el("button", { onclick: () => resend(inv) }, "Resend") : null,
        inv.status === "pending" && mine ? el("button", { class: "secondary", onclick: () => revoke(inv) }, "Revoke") : null)));
  }
}

async function resend(inv) {
  say("");
  const r = await call("/invitations/" + encodeURIComponent(inv.id) + "/resend", { method: "POST" });
  if (await guard(r, r.body.emailSent === false ? "Renewed, but the email could not be sent." : "Sent again.")) await loadInvitations();
}

async function revoke(inv) {
  if (!confirm("Revoke the invitation for " + inv.email + "?")) return;
  say("");
  if (await guard(await call("/invitations/" + encodeURIComponent(inv.id), { method: "DELETE" }), "Revoked.")) await loadInvitations();
}

async function invite(ev) {
  ev.preventDefault();
  say("");
  const f = ev.target.elements;
  const r = await call("/invitations", { method: "POST", body: { email: f.email.value, role: f.role.value, tags: splitTags(f.tags.value) } });
  if (await guard(r, r.body.emailSent === false ? "Invitation created, but the email could not be sent. Resend it from the list." : "Invitation sent.")) {
    f.email.value = "";
    f.tags.value = "";
    await Promise.all([loadInvitations(), loadTags()]);
  }
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
      canManage ? el("button", { class: "linklike", "aria-label": "Delete tag " + t.name, onclick: () => deleteTag(t) }, "x") : null));
  }
  if (!tags.length) list.append(el("span", { class: "muted" }, "No tags yet."));
}

async function addTag(ev) {
  ev.preventDefault();
  say("");
  const input = ev.target.elements.name;
  if (await guard(await call("/tags", { method: "POST", body: { name: input.value } }), "Tag added.")) {
    input.value = "";
    await loadTags();
  }
}

async function deleteTag(t) {
  if (!confirm('Delete the tag "' + t.name + '"? It is removed from every member.')) return;
  say("");
  if (await guard(await call("/tags/" + encodeURIComponent(t.id), { method: "DELETE" }), "Tag deleted.")) {
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
  root.querySelector("#myRole").textContent = "You are " + (org.role === "admin" || org.role === "owner" ? "an " : "a ") + org.role + ".";
  document.title = org.name;
  for (const s of root.querySelectorAll("[data-manage]")) s.hidden = !canManage;
  root.querySelector("#inviteRole").replaceWith(Object.assign(roleSelect("member", myRank), { id: "inviteRole", name: "role" }));

  signinPanel.hidden = true;
  mainPanel.hidden = false;
  await Promise.all([loadMembers(), canManage ? loadInvitations() : null, loadTags()]);
  return true;
}

root.querySelector("#signout").addEventListener("click", signOut);
root.querySelector("#inviteForm").addEventListener("submit", invite);
root.querySelector("#tagForm").addEventListener("submit", addTag);

(async () => {
  const t = session.get();
  if (t && (await enter(t))) return;
  signinPanel.hidden = false;
  await initSignIn({
    root: signinPanel, collection, captcha,
    onToken: async (tok) => {
      session.set(tok);
      if (!(await enter(tok))) say("Sign-in failed. Try again.");
    },
  });
})();
