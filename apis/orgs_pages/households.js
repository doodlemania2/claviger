// "My household" (#80): the households of the signed-in person. A guardian
// (or church staff who belong to the household) can change what the API
// allows; everyone else sees the page read-only. Everything goes through
// /api/households; nothing is cached or kept beyond this page.
import { session, el, api, t, word, createCaptcha, initSignIn } from "/orgs/assets/auth.js";

const root = document.getElementById("app");
const collection = root.dataset.collection;
const signinPanel = root.querySelector("#signin");
const mainPanel = root.querySelector("#main");
const statusEl = root.querySelector("#hstatus");
const listEl = root.querySelector("#households");
const loadingEl = root.querySelector("#loading");
const captcha = createCaptcha(root.querySelector("#captchaBox"));

const ROLES = ["head", "spouse", "adult", "child", "other"];
let seq = 0;
// set when the photo routes answered 404/405: the backend of this server does not have them
let photosUnavailable = false;

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

// problem turns a failed response into text the person can act on.
function problem(r) {
  const msg = (r.body && r.body.message) || "";
  if (r.status === 409 && /not encrypted/i.test(msg)) return t("js.households.notEncrypted");
  if (r.status === 403) return t("js.households.denied");
  if (r.status === 404) return t("js.households.gone");
  if (r.status === 429) return t("js.tooMany");
  return t("js.households.failed") + (msg ? " " + msg : "");
}

const call = (path, opts) => api("/api/households" + path, Object.assign({ token: session.get() }, opts || {}));

// field(label, input) builds a label with its control; ids tie them together for screen readers
function field(label, control, help) {
  const id = "f" + ++seq;
  control.id = id;
  const kids = [el("label", { for: id }, label), control];
  if (help) {
    const hid = id + "h";
    control.setAttribute("aria-describedby", hid);
    kids.push(el("p", { class: "muted small", id: hid }, help));
  }
  return el("div", { class: "fld" }, ...kids);
}

// form builds a form with its own error/success line (an alert, focused on failure)
function form(label, fields, onSave) {
  const msg = el("p", { class: "msg err", role: "alert", tabindex: "-1", hidden: true });
  const ok = el("p", { class: "msg ok", role: "status", hidden: true });
  const btn = el("button", { type: "submit" }, t("js.households.save"));
  const f = el("form", { class: "hhform", "aria-label": label }, el("div", { class: "fields" }, ...fields), msg, ok, btn);
  f.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    msg.hidden = ok.hidden = true;
    btn.disabled = true;
    const r = await onSave();
    btn.disabled = false;
    if (r.status === 401) return signOut();
    if (!r.ok) {
      msg.textContent = problem(r);
      msg.hidden = false;
      msg.focus();
      return;
    }
    ok.textContent = t("js.households.saved");
    ok.hidden = false;
  });
  return f;
}

const text = (name, value, extra) => el("input", Object.assign({ name, value: value || "", autocomplete: "off" }, extra || {}));

// ---- the household ----------------------------------------------------------
function detailsPart(h, canEdit) {
  const addr = h.address && typeof h.address === "object" ? h.address : {};
  const str = (k) => (typeof addr[k] === "string" ? addr[k] : "");

  if (!canEdit) {
    const dl = el("dl", { class: "facts" });
    const add = (k, v) => v && dl.append(el("dt", {}, t(k)), el("dd", {}, v));
    add("js.households.street", str("street"));
    add("js.households.city", str("city"));
    add("js.households.region", str("region"));
    add("js.households.postal", str("postal"));
    add("js.households.homePhone", h.homePhone);
    return dl;
  }

  const name = text("name", h.name, { required: true, maxlength: 200 });
  const street = text("street", str("street"), { autocomplete: "street-address" });
  const city = text("city", str("city"), { autocomplete: "address-level2" });
  const region = text("region", str("region"), { autocomplete: "address-level1" });
  const postal = text("postal", str("postal"), { autocomplete: "postal-code" });
  const phone = text("homePhone", h.homePhone, { type: "tel", autocomplete: "tel" });

  return form(t("js.households.details"), [
    field(t("js.households.name"), name), field(t("js.households.street"), street), field(t("js.households.city"), city),
    field(t("js.households.region"), region), field(t("js.households.postal"), postal), field(t("js.households.homePhone"), phone),
  ], async () => {
    // keep the keys of an address this form does not know about
    const address = Object.assign({}, addr, { street: street.value, city: city.value, region: region.value, postal: postal.value });
    const r = await call("/" + encodeURIComponent(h.id), { method: "PATCH", body: { name: name.value, address, homePhone: phone.value } });
    if (r.ok) {
      const title = document.getElementById("hh-" + h.id);
      if (title) title.textContent = name.value;
    }
    return r;
  });
}

// ---- a person ---------------------------------------------------------------
async function showPhoto(img, h, m) {
  try {
    const res = await fetch("/api/files/_householdMembers/" + encodeURIComponent(m.id) + "/" + encodeURIComponent(m.photo), {
      headers: { Authorization: session.get() },
    });
    if (!res.ok) return;
    const blob = await res.blob();
    // data: URLs are allowed by the page policy, blob: URLs are not
    const url = await new Promise((resolve, reject) => {
      const fr = new FileReader();
      fr.onload = () => resolve(fr.result);
      fr.onerror = reject;
      fr.readAsDataURL(blob);
    });
    img.src = url;
    img.hidden = false;
  } catch (e) { /* no picture: the name is enough */ }
}

function photoPart(h, m) {
  const img = el("img", { class: "avatar", alt: t("js.households.photoAlt", { name: m.name }), hidden: true, width: 64, height: 64 });
  if (m.photo) showPhoto(img, h, m);

  const file = el("input", { type: "file", accept: "image/jpeg,image/png,image/webp" });
  const msg = el("p", { class: "msg err", role: "alert", tabindex: "-1", hidden: true });
  const ok = el("p", { class: "msg ok", role: "status", hidden: true });
  const up = el("button", { type: "button", class: "secondary" }, t("js.households.photoUpload"));
  const rm = el("button", { type: "button", class: "secondary" }, t("js.households.photoRemove"));
  rm.hidden = !m.photo;
  const box = el("div", { class: "photo" }, img, field(t("js.households.photo"), file, t("js.households.photoHelp")), up, rm, msg, ok);

  function fail(text) {
    msg.textContent = text;
    msg.hidden = false;
    ok.hidden = true;
    msg.focus();
  }

  // when the server has no photo routes (404/405) the controls go away
  function unavailable() {
    photosUnavailable = true;
    box.textContent = "";
    box.append(el("p", { class: "muted", role: "status" }, t("js.households.photoUnavailable")));
  }

  const url = "/api/households/" + encodeURIComponent(h.id) + "/members/" + encodeURIComponent(m.id) + "/photo";

  up.addEventListener("click", async () => {
    msg.hidden = ok.hidden = true;
    if (!file.files.length) return fail(t("js.households.photoPick"));
    const fd = new FormData();
    fd.append("photo", file.files[0]);
    up.disabled = true;
    let res;
    try {
      res = await fetch(url, { method: "POST", headers: { Authorization: session.get() }, body: fd });
    } catch (e) {
      up.disabled = false;
      return fail(t("js.households.failed"));
    }
    up.disabled = false;
    const body = await res.json().catch(() => ({}));
    if (res.status === 404 || res.status === 405) return unavailable();
    if (res.status === 401) return signOut();
    if (!res.ok) return fail(problem({ status: res.status, body }));
    if (body.photo) {
      m.photo = body.photo;
      await showPhoto(img, h, m);
    }
    rm.hidden = false;
    file.value = "";
    ok.textContent = t("js.households.photoSaved");
    ok.hidden = false;
  });

  rm.addEventListener("click", async () => {
    msg.hidden = ok.hidden = true;
    const res = await fetch(url, { method: "DELETE", headers: { Authorization: session.get() } });
    const body = await res.json().catch(() => ({}));
    if (res.status === 404 || res.status === 405) return unavailable();
    if (res.status === 401) return signOut();
    if (!res.ok && res.status !== 204) return fail(problem({ status: res.status, body }));
    img.hidden = true;
    rm.hidden = true;
    ok.textContent = t("js.households.photoRemoved");
    ok.hidden = false;
  });

  return box;
}

function personPart(h, m, canEdit) {
  const tags = [];
  if (m.id === h.me) tags.push(t("js.households.you"));
  if (m.guardian) tags.push(t("js.households.guardian"));
  if (m.isMinor) tags.push(t("js.households.minor"));
  if (m.status && m.status !== "active") tags.push(word("households.status", m.status));

  const head = el("legend", {}, m.name, tags.map((x) => el("span", { class: "tag" }, x)));
  const box = el("fieldset", { class: "person" }, head);
  const health = m.birthDate !== undefined; // the server sends these only to those who may see them

  if (!canEdit) {
    const dl = el("dl", { class: "facts" }, el("dt", {}, t("js.households.role")), el("dd", {}, word("households.role", m.role)));
    if (health && m.birthDate) dl.append(el("dt", {}, t("js.households.birthDate")), el("dd", {}, m.birthDate));
    box.append(dl);
    return box;
  }

  const name = text("name", m.name, { required: true, maxlength: 200 });
  const role = el("select", { name: "role" }, ...ROLES.map((r) => el("option", { value: r, selected: r === m.role }, word("households.role", r))));
  const fields = [field(t("js.households.personName"), name), field(t("js.households.role"), role)];
  let birth, allergies, medical;
  if (m.isMinor || h.me === m.id) {
    birth = text("birthDate", (m.birthDate || "").slice(0, 10), { type: "date", autocomplete: "off" });
    allergies = text("allergies", m.allergies || "", { maxlength: 500 });
    medical = el("textarea", { name: "medical", rows: 3, maxlength: 2000, autocomplete: "off" });
    medical.value = m.medical || "";
    fields.push(field(t("js.households.birthDate"), birth), field(t("js.households.allergies"), allergies), field(t("js.households.medical"), medical));
  }

  box.append(form(m.name, fields, async () => {
    const body = { name: name.value, role: role.value };
    if (birth) {
      body.birthDate = birth.value;
      body.allergies = allergies.value;
      body.medical = medical.value;
    }
    const r = await call("/" + encodeURIComponent(h.id) + "/members/" + encodeURIComponent(m.id), { method: "PATCH", body });
    if (r.ok) head.firstChild.textContent = name.value;
    return r;
  }));
  if (m.isMinor && !photosUnavailable) box.append(photoPart(h, m));
  return box;
}

function householdPart(h) {
  const canEdit = !!(h.guardian || h.staff);
  const hid = "hh-" + h.id;
  const sec = el("section", { class: "hh", "aria-labelledby": hid },
    el("h2", { id: hid }, h.name),
    el("p", { class: "muted" }, canEdit ? t("js.households.guardianNote") : t("js.households.readOnlyNote")),
    detailsPart(h, canEdit),
    el("h3", {}, t("js.households.people")),
  );
  for (const m of h.members || []) sec.append(personPart(h, m, canEdit));
  return sec;
}

async function load() {
  say("");
  loadingEl.hidden = false;
  listEl.textContent = "";
  const r = await call("/mine");
  loadingEl.hidden = true;
  if (r.status === 401) return signOut();
  if (r.status === 404) return say(t("js.households.unavailable"));
  if (!r.ok) return say(t("js.households.loadFailed"));
  const items = r.body.items || [];
  if (!items.length) {
    listEl.append(el("p", { class: "muted" }, t("js.households.none")));
    return;
  }
  for (const h of items) listEl.append(householdPart(h));
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
