// Overseer console — append-phase file loader.
// Dropping or choosing a .md/.txt file fills the append-phase form: the file
// name without its extension becomes the phase name and the file contents
// become the initial WBS. Nothing is submitted until the user appends; the
// zone stays hidden without JavaScript and the plain form still works.
(function () {
  "use strict";
  var MAX_BYTES = 512 << 10; // form-encoding inflates text; stay well under the 1 MiB body limit
  var zone = document.querySelector("[data-phase-upload]");
  if (!zone) { return; }
  var form = zone.closest("form");
  var input = zone.querySelector("input[type=file]");
  var errorEl = zone.querySelector("[data-phase-upload-error]");
  var nameEl = form.querySelector("input[name=name]");
  var wbsEl = form.querySelector("textarea[name=wbs]");

  function showError(msg) {
    errorEl.textContent = msg;
    errorEl.hidden = !msg;
  }

  function load(file) {
    showError("");
    if (!file) { return; }
    var m = /^(.+)\.(md|txt)$/i.exec(file.name);
    if (!m) { showError("Only .md or .txt files can be loaded."); return; }
    if (file.size > MAX_BYTES) { showError("File is too large (limit 512 KiB)."); return; }
    var reader = new FileReader();
    reader.onload = function () {
      nameEl.value = m[1].slice(0, nameEl.maxLength > 0 ? nameEl.maxLength : undefined);
      wbsEl.value = String(reader.result);
      nameEl.focus();
    };
    reader.onerror = function () { showError("The file could not be read."); };
    reader.readAsText(file);
  }

  input.addEventListener("change", function () { load(input.files[0]); });
  zone.addEventListener("dragover", function (ev) {
    ev.preventDefault();
    ev.dataTransfer.dropEffect = "copy";
    zone.classList.add("dragover");
  });
  zone.addEventListener("dragleave", function (ev) {
    if (!zone.contains(ev.relatedTarget)) { zone.classList.remove("dragover"); }
  });
  zone.addEventListener("drop", function (ev) {
    ev.preventDefault();
    zone.classList.remove("dragover");
    var files = ev.dataTransfer.files;
    if (files.length > 1) {
      // Several files: hand them to the multi-file upload form.
      if (window.overseerPhaseFiles && window.overseerPhaseFiles(files)) { return; }
      showError("Drop one file at a time.");
      return;
    }
    load(files[0]);
  });
  zone.hidden = false;
})();

// Multi-file phase upload: choosing or dropping several .md/.txt files
// lists them (in the file-name order the server appends them) and checks
// them before the form submits; the server checks everything again. The
// plain file input works without JavaScript.
(function () {
  "use strict";
  var MAX_TOTAL = (2 << 20) - (64 << 10); // request limit 2 MiB, less multipart overhead
  var MAX_FILE = 1 << 20; // WBS document limit
  var form = document.querySelector("[data-phases-upload]");
  if (!form) { return; }
  var zone = form.querySelector("[data-phases-drop]");
  var input = form.querySelector("input[type=file]");
  var list = form.querySelector("[data-phases-list]");
  var errorEl = form.querySelector("[data-phases-error]");
  var submit = form.querySelector("[data-phases-submit]");

  function problems(files) {
    var out = [], total = 0;
    for (var i = 0; i < files.length; i++) {
      var f = files[i];
      total += f.size;
      if (!/^.+\.(md|txt)$/i.test(f.name)) { out.push(f.name + ": only .md or .txt files can be uploaded"); }
      else if (f.size === 0) { out.push(f.name + ": the file is empty"); }
      else if (f.size > MAX_FILE) { out.push(f.name + ": larger than 1 MiB"); }
    }
    if (total > MAX_TOTAL) { out.push("the files total more than 2 MiB; upload them in smaller groups"); }
    return out;
  }

  function render() {
    var files = Array.prototype.slice.call(input.files || []);
    files.sort(function (a, b) { return a.name < b.name ? -1 : a.name > b.name ? 1 : 0; });
    list.textContent = "";
    files.forEach(function (f) {
      var li = document.createElement("li");
      li.textContent = f.name.replace(/\.(md|txt)$/i, "") + " — " + f.name + " (" + Math.max(1, Math.round(f.size / 1024)) + " KiB)";
      list.appendChild(li);
    });
    list.hidden = files.length === 0;
    var bad = problems(files);
    errorEl.textContent = bad.join("; ");
    errorEl.hidden = bad.length === 0;
    submit.disabled = files.length === 0 || bad.length > 0;
    submit.textContent = files.length > 1 ? "Append " + files.length + " phases" : "Append phase" + (files.length ? "" : "s");
  }

  function take(files) {
    try {
      input.files = files;
    } catch (e) {
      return false;
    }
    render();
    zone.scrollIntoView({ block: "nearest" });
    return true;
  }
  window.overseerPhaseFiles = take;

  input.addEventListener("change", render);
  zone.addEventListener("dragover", function (ev) {
    ev.preventDefault();
    ev.dataTransfer.dropEffect = "copy";
    zone.classList.add("dragover");
  });
  zone.addEventListener("dragleave", function (ev) {
    if (!zone.contains(ev.relatedTarget)) { zone.classList.remove("dragover"); }
  });
  zone.addEventListener("drop", function (ev) {
    ev.preventDefault();
    zone.classList.remove("dragover");
    if (!take(ev.dataTransfer.files)) { errorEl.textContent = "This browser cannot accept dropped files; choose them instead."; errorEl.hidden = false; }
  });
  form.querySelector("[data-phases-drop-hint]").hidden = false;
  render();
})();
