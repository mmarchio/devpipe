// Overseer console — model pickers. A text input marked
// data-model-provider="<provider select id>" data-model-kind="worker|compression"
// becomes a select of the models the chosen provider serves
// (GET /api/v1/providers/{kind}/{name}/models). When the provider cannot
// list its models the free-text input stays. Without JavaScript the text
// input is used as-is.
(function () {
  "use strict";

  function setup(input) {
    var providerSel = document.getElementById(input.dataset.modelProvider);
    if (!providerSel) { return; }
    var kind = input.dataset.modelKind;
    var select = document.createElement("select");
    select.name = input.name;
    select.hidden = true;
    select.disabled = true;
    input.insertAdjacentElement("afterend", select);
    var hint = document.createElement("div");
    hint.className = "hint";
    hint.setAttribute("aria-live", "polite");
    select.insertAdjacentElement("afterend", hint);
    var seq = 0;

    // Exactly one of input / select is enabled (and so submitted) and
    // carries the id the label points at.
    function showText(msg) {
      select.hidden = true; select.disabled = true; select.removeAttribute("id");
      input.hidden = false; input.disabled = false; input.id = input.name;
      hint.textContent = msg || "";
    }
    function showSelect(models, current) {
      select.textContent = "";
      var ph = new Option("Select…", "");
      select.add(ph);
      models.forEach(function (m) { select.add(new Option(m, m)); });
      if (current && models.indexOf(current) < 0) {
        select.add(new Option(current + " (not listed by provider)", current));
      }
      select.value = current || "";
      input.hidden = true; input.disabled = true; input.removeAttribute("id");
      select.hidden = false; select.disabled = false; select.id = input.name;
      hint.textContent = "";
    }
    function current() { return select.disabled ? input.value : select.value; }

    function load() {
      var name = providerSel.value;
      var mine = ++seq;
      if (!name) { showText(""); return; }
      var value = current();
      hint.textContent = "Loading models…";
      fetch("/api/v1/providers/" + encodeURIComponent(kind) + "/" + encodeURIComponent(name) + "/models",
        { credentials: "same-origin", headers: { Accept: "application/json" } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (body) {
          if (mine !== seq) { return; }
          if (body && body.available && body.models && body.models.length) {
            showSelect(body.models, value);
          } else {
            input.value = value;
            showText("The provider did not list its models; enter the model name.");
          }
        })
        .catch(function () {
          if (mine !== seq) { return; }
          input.value = value;
          showText("The provider did not list its models; enter the model name.");
        });
    }

    providerSel.addEventListener("change", function () {
      // A different provider's model would not apply.
      input.value = ""; select.value = "";
      load();
    });
    load();
  }

  function init() {
    var inputs = document.querySelectorAll("input[data-model-provider]");
    for (var i = 0; i < inputs.length; i++) { setup(inputs[i]); }
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
