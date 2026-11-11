// The recovery page of the person (#83): type the code staff gave you, then
// create a passkey through the ordinary passkey reset routes. Nothing here
// stores the code or the reset token anywhere: they live in this page only.
import { el, api, t } from "/orgs/assets/auth.js";
import { helpers } from "/api/passkeys-client.js";

const root = document.getElementById("app");
const panels = {
  enter: root.querySelector("#enter"),
  enroll: root.querySelector("#enroll"),
  done: root.querySelector("#done"),
  report: root.querySelector("#report"),
};
const statusEl = root.querySelector("#rstatus");

let resetToken = "";
let collection = "";

function show(name) {
  for (const [k, p] of Object.entries(panels)) p.hidden = k !== name;
}

function say(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = "msg " + (kind || "err");
  statusEl.hidden = !text;
}

// ---- the code --------------------------------------------------------------
const form = root.querySelector("#codeForm");

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  say("");
  const r = await api("/api/recovery/redeem", { body: { code: form.elements.code.value } });
  if (r.status === 429) return say(t("js.tooMany"));
  if (!r.ok || !r.body.token) return say(t("js.recovery.failed"));
  resetToken = r.body.token;
  collection = r.body.collection;
  form.elements.code.value = "";
  show("enroll");
  root.querySelector("#enrollBtn").focus();
});

// ---- the passkey -----------------------------------------------------------
root.querySelector("#enrollBtn").addEventListener("click", async () => {
  say("");
  if (!window.PublicKeyCredential) return say(t("js.recovery.unsupported"));
  const base = "/api/collections/" + encodeURIComponent(collection);
  try {
    const begin = await api(base + "/confirm-passkey-reset/begin", { body: { token: resetToken } });
    if (!begin.ok) throw new Error("begin");
    let publicKey = begin.body.options;
    if (publicKey && publicKey.publicKey) publicKey = publicKey.publicKey;
    const cred = await navigator.credentials.create({ publicKey: helpers.decodePublicKeyOptions(publicKey) });
    const name = root.querySelector("#deviceName").value.trim() || t("js.recovery.defaultDevice");
    const fin = await api(base + "/confirm-passkey-reset/finish", {
      body: Object.assign({ token: resetToken, sessionToken: begin.body.sessionToken, name }, helpers.encodeAttestation(cred)),
    });
    if (!fin.ok) throw new Error("finish");
  } catch (err) {
    return say(t("js.recovery.enrollFailed"));
  }
  resetToken = "";
  root.querySelector("#doneText").textContent = t("js.recovery.enrolled");
  show("done");
});

// ---- "this was not me" -----------------------------------------------------
root.querySelector("#reportBtn").addEventListener("click", async () => {
  say("");
  const m = /report=([^&]+)/.exec(location.hash);
  const r = m ? await api("/api/recovery/report", { body: { token: decodeURIComponent(m[1]) } }) : { ok: false };
  if (!r.ok) return say(t("js.recovery.reportFailed"));
  history.replaceState(null, "", location.pathname);
  root.querySelector("#report").replaceChildren(el("p", { class: "msg ok", role: "status" }, t("js.recovery.reported")));
});

// ---- which panel -----------------------------------------------------------
(function start() {
  const hash = location.hash || "";
  if (/^#report=/.test(hash)) return show("report");
  const m = /^#code=(.+)$/.exec(hash);
  if (m) {
    form.elements.code.value = decodeURIComponent(m[1]);
    // the fragment is not needed any more; keep it out of the history
    history.replaceState(null, "", location.pathname);
  }
  show("enter");
  form.elements.code.focus();
})();
