// The registers page of an org (#87, 87-5): registrars finalize entries and
// write marginal notes; owners and admins read. Everything goes through
// /api/registers; the server decides who may do what and the page only shows
// it. Finalizing and annotating need a fresh passkey assertion, which the page
// asks for when the server says so. Nothing is ever parsed as HTML.
import { session, el, api, when, createCaptcha, initSignIn, t } from "/orgs/assets/auth.js";
import { PasskeysClient } from "/api/passkeys-client.js";

const root = document.getElementById("app");
const slug = root.dataset.slug;
const collection = root.dataset.collection;

const signinPanel = root.querySelector("#signin");
const mainPanel = root.querySelector("#main");
const statusEl = root.querySelector("#pstatus");
const loadingEl = root.querySelector("#loading");
const listPanel = root.querySelector("#listPanel");
const entryPanel = root.querySelector("#entry");
const noteForm = root.querySelector("#noteForm");
const captcha = createCaptcha(root.querySelector("#captchaBox"));

const KINDS = ["correction", "later_event", "legal_note", "retraction", "data_subject_note"];
const COLUMN_TYPES = ["text", "select", "date", "number"];
const PER_PAGE = 25;

let org = null;
let me = null;
let access = { registrar: false, manager: false };
let registers = [];
let current = null; // the register being shown
let page = 1;
let open = null; // { entry, notes } of the entry being shown

function say(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = "msg " + (kind || "err");
  statusEl.setAttribute("role", kind === "ok" ? "status" : "alert");
  statusEl.hidden = !text;
  if (text && kind !== "ok") statusEl.focus();
}

const call = (path, opts) => api(path, Object.assign({ token: session.get() }, opts || {}));

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
    return { ok: false, status: 0, body: { message: t("js.registers.stepUpFailed") } };
  }
  r = await run();
  return r;
}

function refusal(r, fallback) {
  if (r.status === 401) return signOut();
  say(r.body && r.body.message ? r.body.message : t(fallback || "js.admin.failed"));
}

function value(f, record) {
  const v = record[f.name];
  if (v === undefined || v === null || v === "" || (Array.isArray(v) && !v.length)) return "";
  if (f.type === "date") return String(v).slice(0, 10);
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}

const stateWord = (s) => t("js.registers.state." + s);

// ---- the list ----------------------------------------------------------------
function columns() {
  return (current.fields || []).filter((f) => COLUMN_TYPES.includes(f.type)).slice(0, 3);
}

async function loadEntries() {
  const state = root.querySelector("#stateSelect").value;
  const q = "?org=" + encodeURIComponent(org.id) + "&page=" + page + "&perPage=" + PER_PAGE + (state ? "&state=" + state : "");
  const r = await call("/api/registers/" + encodeURIComponent(current.collection) + q);
  if (r.status === 401) return signOut();
  if (!r.ok) return say(t("js.registers.loadFailed"));

  const cols = columns();
  const head = root.querySelector("#headRow");
  head.textContent = "";
  for (const c of [t("js.registers.col.state"), t("js.registers.col.number")].concat(cols.map((f) => f.name))) head.append(el("th", { scope: "col" }, c));
  head.append(el("th", { scope: "col" }, el("span", { class: "sr-only" }, t("js.registers.open"))));

  const body = root.querySelector("#entries tbody");
  body.textContent = "";
  for (const item of r.body.items) {
    const rec = item.record || {};
    const label = cols.length ? value(cols[0], rec) || item.id : item.id;
    const id = "e-" + item.id;
    body.append(el("tr", { "data-entry": item.id, "data-state": item.state },
      el("td", { id }, el("span", { class: "tag" }, stateWord(item.state)), rec.restricted ? el("span", { class: "tag" }, t("js.registers.restricted")) : null),
      el("td", {}, rec.seq ? String(rec.seq) : ""),
      cols.map((f) => el("td", {}, value(f, rec))),
      el("td", {}, el("button", { type: "button", class: "secondary", "aria-describedby": id, "aria-label": t("js.registers.open") + ": " + label, onclick: () => openEntry(item.id) }, t("js.registers.open")))));
  }
  root.querySelector("#empty").hidden = r.body.items.length > 0;

  const pages = Math.max(1, Math.ceil(r.body.totalItems / PER_PAGE));
  root.querySelector("#pageInfo").textContent = t("js.registers.pageOf", { page: r.body.page, pages });
  root.querySelector("#prev").disabled = r.body.page <= 1;
  root.querySelector("#next").disabled = r.body.page >= pages;
}

// ---- one entry with its margin ---------------------------------------------------
function fact(name, text) {
  if (!text) return [];
  return [el("dt", {}, name), el("dd", {}, text)];
}

function renderEntry(focus) {
  const { entry, notes } = open;
  const rec = entry.record || {};

  root.querySelector("#entryTitle").textContent = t("js.registers.entryTitle", { id: entry.id });
  const facts = root.querySelector("#facts");
  facts.textContent = "";
  facts.append(...fact(t("js.registers.col.state"), stateWord(entry.state)));
  for (const f of current.fields || []) facts.append(...fact(f.name, value(f, rec)));
  facts.append(...fact(t("js.registers.finalizedAt"), when(rec[current.finalizeField])));
  facts.append(...fact(t("js.registers.finalizedBy"), entry.finalizedByName));
  facts.append(...fact(t("js.registers.secondSigner"), entry.secondSignerName));
  facts.append(...fact(t("js.registers.pendingBy"), entry.state === "pending" ? entry.pendingByName : ""));
  facts.append(...fact(t("js.registers.chainNumber"), rec.seq ? String(rec.seq) : ""));
  facts.append(...fact(t("js.registers.chainHash"), rec.hash ? el("code", {}, rec.hash) : ""));

  // what the person may do; the server has the last word
  const mine = entry.state === "pending" && rec.pendingBy === me.id;
  const fin = root.querySelector("#finalize");
  fin.hidden = !(access.registrar && entry.state !== "final" && !mine);
  fin.textContent = entry.state === "pending" ? t("js.registers.confirm") : current.signers === 2 ? t("js.registers.request") : t("js.registers.finalize");
  root.querySelector("#withdraw").hidden = !(entry.state === "pending" && (mine || access.manager));
  root.querySelector("#certBtn").hidden = entry.state !== "final";
  const waiting = root.querySelector("#waiting");
  waiting.hidden = !mine;
  waiting.textContent = mine ? t("js.registers.waiting") : "";

  // the margin: oldest first, a retracted note struck through
  const retracted = new Set(notes.filter((n) => n.kind === "retraction" && n.supersedes).map((n) => n.supersedes));
  const byId = new Map(notes.map((n) => [n.id, n]));
  const list = root.querySelector("#notes");
  list.textContent = "";
  for (const n of notes) {
    const target = n.supersedes && byId.get(n.supersedes);
    list.append(el("li", { class: "note" + (retracted.has(n.id) ? " retracted" : ""), "data-note": n.id, "data-kind": n.kind },
      el("p", { class: "meta" }, t("js.registers.noteMeta", { when: when(n.created), kind: t("js.registers.kind." + n.kind), author: n.authorName || n.author || "" }),
        retracted.has(n.id) ? el("span", { class: "tag" }, t("js.registers.retracted")) : null),
      el("p", { class: "text" }, n.text),
      el("p", { class: "meta" }, t("js.registers.reasonLine", { reason: n.reason })),
      target ? el("p", { class: "meta" }, t("js.registers.retracts", { when: when(target.created) })) : null));
  }
  root.querySelector("#noNotes").hidden = notes.length > 0;

  // the form: only a registrar, only on a final entry
  noteForm.hidden = !(access.registrar && entry.state === "final");
  const sup = root.querySelector("#nSup");
  sup.textContent = "";
  for (const n of notes) {
    if (n.kind === "retraction" || retracted.has(n.id)) continue;
    sup.append(el("option", { value: n.id }, when(n.created) + " - " + t("js.registers.kind." + n.kind)));
  }
  syncKind();

  entryPanel.hidden = false;
  if (focus) root.querySelector("#entryTitle").focus();
}

function syncKind() {
  const retraction = root.querySelector("#nKind").value === "retraction";
  const sup = root.querySelector("#nSup");
  const has = sup.options.length > 0;
  root.querySelector("#supLabel").hidden = !retraction;
  sup.required = retraction;
  sup.disabled = !retraction || !has;
}

async function openEntry(id, focus) {
  closeCert();
  const r = await call("/api/registers/" + encodeURIComponent(current.collection) + "/" + encodeURIComponent(id));
  if (r.status === 401) return signOut();
  if (!r.ok) return refusal(r, "js.registers.loadFailed");
  if (focus !== false) say("");
  open = { entry: r.body, notes: r.body.annotations || [] };
  renderEntry(focus !== false);
}

function closeEntry() {
  open = null;
  closeCert();
  entryPanel.hidden = true;
}

async function refreshAfter(message) {
  const id = open.entry.id;
  await openEntry(id, false);
  await loadEntries();
  say(message, "ok");
}

async function finalize() {
  const { entry } = open;
  const q = entry.state === "pending" ? "js.registers.confirmConfirm" : current.signers === 2 ? "js.registers.confirmRequest" : "js.registers.confirmFinalize";
  if (!confirm(t(q))) return;
  say("");
  const r = await stepped(() => call("/api/registers/" + encodeURIComponent(current.collection) + "/" + encodeURIComponent(entry.id) + "/finalize", { method: "POST" }));
  if (!r.ok) return refusal(r);
  await refreshAfter(r.body.final ? t("js.registers.finalized") : t("js.registers.requested"));
}

async function withdraw() {
  const { entry } = open;
  say("");
  const r = await call("/api/registers/" + encodeURIComponent(current.collection) + "/" + encodeURIComponent(entry.id) + "/finalize", { method: "DELETE" });
  if (!r.ok) return refusal(r);
  await refreshAfter(t("js.registers.withdrawn"));
}

async function annotate(ev) {
  ev.preventDefault();
  const { entry } = open;
  say("");
  const f = noteForm.elements;
  const body = { kind: f.kind.value, text: f.text.value, reason: f.reason.value };
  if (body.kind === "retraction") body.supersedes = f.supersedes.value;
  const r = await stepped(() => call("/api/registers/" + encodeURIComponent(current.collection) + "/" + encodeURIComponent(entry.id) + "/annotations", { method: "POST", body }));
  if (!r.ok) return refusal(r);
  noteForm.reset();
  await refreshAfter(t("js.registers.noted"));
}

// ---- the certificate ----------------------------------------------------------------
// The certificate route answers a finished page for a bearer token. It is
// fetched with the token and shown in a sandboxed frame (no script can run in
// it), so the person can print it or save it as a PDF.
function closeCert() {
  root.querySelector("#certBox").hidden = true;
  root.querySelector("#certHost").textContent = "";
}

async function showCert() {
  say("");
  const id = open.entry.id;
  const res = await fetch("/api/registers/" + encodeURIComponent(current.collection) + "/" + encodeURIComponent(id) + "/certificate", { headers: { Authorization: session.get() } });
  if (res.status === 401) return signOut();
  if (!res.ok) return say(res.status === 409 ? t("js.registers.certNeedsFinal") : t("js.registers.certFailed"));
  const html = await res.text();
  // no script can run in the frame; it may be read by the page only to print it
  const frame = el("iframe", { id: "certFrame", class: "certframe", title: t("js.registers.certFrame"), sandbox: "allow-same-origin allow-modals" });
  frame.srcdoc = html;
  const host = root.querySelector("#certHost");
  host.textContent = "";
  host.append(frame);
  const box = root.querySelector("#certBox");
  box.hidden = false;
  root.querySelector("#certTitle").focus();
}

function printCert() {
  const frame = root.querySelector("#certFrame");
  if (frame && frame.contentWindow) {
    frame.contentWindow.focus();
    frame.contentWindow.print();
  }
}

// ---- boot --------------------------------------------------------------------
function chooseRegister(name) {
  current = registers.find((r) => r.collection === name) || registers[0];
  page = 1;
  closeEntry();
  say("");
  return loadEntries();
}

async function loadRegisters() {
  loadingEl.hidden = false;
  const r = await call("/api/registers?org=" + encodeURIComponent(org.id));
  loadingEl.hidden = true;
  if (r.status === 401) return signOut();
  if (r.status === 403 || r.status === 404) return deny(t("js.registers.noAccess"));
  if (!r.ok) return say(t("js.registers.loadFailed"));
  access = { registrar: !!r.body.registrar, manager: !!r.body.manager };
  registers = r.body.items || [];
  root.querySelector("#none").hidden = registers.length > 0;
  listPanel.hidden = registers.length === 0;
  const ro = root.querySelector("#readonly");
  ro.hidden = access.registrar || registers.length === 0;
  ro.textContent = ro.hidden ? "" : t("js.registers.readOnly");
  if (!registers.length) return;
  const select = root.querySelector("#regSelect");
  select.textContent = "";
  for (const reg of registers) select.append(el("option", { value: reg.collection }, reg.collection));
  await chooseRegister(registers[0].collection);
}

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
    deny(t("js.registers.notMember"));
    return true;
  }
  org = r.body;
  root.querySelector("#orgName").textContent = org.name;
  document.title = org.name + " - " + document.title;

  signinPanel.hidden = true;
  mainPanel.hidden = false;
  await loadRegisters();
  return true;
}

const kindSelect = root.querySelector("#nKind");
for (const k of KINDS) kindSelect.append(el("option", { value: k }, t("js.registers.kind." + k)));
kindSelect.addEventListener("change", syncKind);

root.querySelector("#signout").addEventListener("click", signOut);
root.querySelector("#regSelect").addEventListener("change", (ev) => chooseRegister(ev.target.value));
root.querySelector("#stateSelect").addEventListener("change", () => { page = 1; say(""); loadEntries(); });
root.querySelector("#prev").addEventListener("click", () => { page -= 1; loadEntries(); });
root.querySelector("#next").addEventListener("click", () => { page += 1; loadEntries(); });
root.querySelector("#entryClose").addEventListener("click", closeEntry);
root.querySelector("#finalize").addEventListener("click", finalize);
root.querySelector("#withdraw").addEventListener("click", withdraw);
root.querySelector("#certBtn").addEventListener("click", showCert);
root.querySelector("#certPrint").addEventListener("click", printCert);
root.querySelector("#certClose").addEventListener("click", closeCert);
noteForm.addEventListener("submit", annotate);

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
