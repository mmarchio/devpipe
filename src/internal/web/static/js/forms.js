// Overseer console — form submission guard (WBS 17.165).
// After a submit the form's buttons are disabled and show a pending label;
// the server (POST → redirect → GET) remains the idempotency authority and
// the page re-renders from the backend. Without JavaScript forms still work.
(function () {
  "use strict";
  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    if (!(form instanceof HTMLFormElement)) { return; }
    if (form.dataset.submitted === "1") { ev.preventDefault(); return; }
    form.dataset.submitted = "1";
    var buttons = form.querySelectorAll("button[type=submit], button:not([type])");
    for (var i = 0; i < buttons.length; i++) {
      var b = buttons[i];
      b.setAttribute("aria-busy", "true");
      b.disabled = true;
      if (b.dataset.pending) { b.textContent = b.dataset.pending; }
    }
  }, true);
  // Back/forward cache restores must re-enable forms.
  window.addEventListener("pageshow", function () {
    var forms = document.querySelectorAll("form[data-submitted='1']");
    for (var i = 0; i < forms.length; i++) {
      forms[i].dataset.submitted = "";
      var bs = forms[i].querySelectorAll("button");
      for (var j = 0; j < bs.length; j++) { bs[j].disabled = false; bs[j].removeAttribute("aria-busy"); }
    }
  });
})();
