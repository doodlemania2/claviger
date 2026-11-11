// Accessible mode of the hosted pages (#83, 83-3): "Larger text and high
// contrast". A classic script loaded in <head>, so the saved choice is applied
// before the first paint (no flash of small text). Every read and write of
// localStorage is wrapped: the page works when storage is blocked, the choice
// then just lasts until the page is closed.
(function () {
  var KEY = "claviger_a11y";
  var root = document.documentElement;
  var on = false;

  try { on = window.localStorage.getItem(KEY) === "large"; } catch (e) { /* storage blocked */ }

  function apply() {
    if (on) root.setAttribute("data-a11y", "large");
    else root.removeAttribute("data-a11y");
  }

  function save() {
    try {
      if (on) window.localStorage.setItem(KEY, "large");
      else window.localStorage.removeItem(KEY);
    } catch (e) { /* storage blocked */ }
  }

  apply();

  function wire() {
    var btn = document.getElementById("a11yToggle");
    if (!btn) return;
    btn.setAttribute("aria-pressed", on ? "true" : "false");
    btn.hidden = false; // without script the button would do nothing, so it starts hidden
    btn.addEventListener("click", function () {
      on = !on;
      apply();
      save();
      btn.setAttribute("aria-pressed", on ? "true" : "false");
    });
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", wire);
  else wire();

  // keep two open tabs in step
  window.addEventListener("storage", function (ev) {
    if (ev.key !== KEY) return;
    on = ev.newValue === "large";
    apply();
    var btn = document.getElementById("a11yToggle");
    if (btn) btn.setAttribute("aria-pressed", on ? "true" : "false");
  });
})();
