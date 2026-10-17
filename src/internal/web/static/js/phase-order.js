// Overseer console — drag-and-drop phase reordering.
// Movable rows (phases that have not started) are dragged by their handle;
// locked rows keep their positions and movable phases are reshuffled among
// the movable slots, mirroring the server rule. The new order is saved with
// the form's Save button (hidden phase_ids inputs, first to last). Without
// JavaScript the Move up / Move down buttons still reorder one step at a
// time.
(function () {
  "use strict";
  var form = document.querySelector("[data-phase-order]");
  if (!form) { return; }
  var tbody = form.querySelector("[data-phase-order-rows]");
  var actions = form.querySelector("[data-phase-order-actions]");
  if (!tbody || !actions) { return; }
  var statusEl = form.querySelector("[data-phase-order-status]");
  var rows = function () { return Array.prototype.slice.call(tbody.querySelectorAll("tr[data-phase-id]")); };
  var movable = function () { return rows().filter(function (r) { return r.hasAttribute("data-movable"); }); };

  // The order number shown in each slot stays with the slot.
  var slotNumbers = rows().map(function (r) { return numberCell(r).firstChild.nodeValue; });
  var original = movable();
  var dragged = null;

  function numberCell(row) {
    return row.querySelector("input[name=phase_ids]").parentNode;
  }

  // layout places the movable rows, in seq order, into the movable slots.
  function layout(seq) {
    var next = 0;
    var ordered = rows().map(function (r) { return r.hasAttribute("data-movable") ? seq[next++] : r; });
    ordered.forEach(function (r, i) {
      tbody.appendChild(r);
      numberCell(r).firstChild.nodeValue = slotNumbers[i];
    });
    var changed = seq.some(function (r, i) { return r !== original[i]; });
    actions.hidden = !changed;
    statusEl.textContent = changed ? "Order changed — save to apply." : "";
  }

  rows().forEach(function (row) {
    if (!row.hasAttribute("data-movable")) { return; }
    var handle = row.querySelector(".drag-handle");
    handle.addEventListener("mousedown", function () { row.draggable = true; });
    handle.addEventListener("touchstart", function () { row.draggable = true; }, { passive: true });
    row.addEventListener("dragstart", function (ev) {
      dragged = row;
      row.classList.add("dragging");
      ev.dataTransfer.effectAllowed = "move";
      ev.dataTransfer.setData("text/plain", row.getAttribute("data-phase-id"));
    });
    row.addEventListener("dragend", function () {
      row.draggable = false;
      row.classList.remove("dragging");
      rows().forEach(function (r) { r.classList.remove("drop-target"); });
      dragged = null;
    });
  });

  tbody.addEventListener("dragover", function (ev) {
    if (!dragged) { return; }
    var target = ev.target.closest("tr[data-movable]");
    if (!target || target === dragged) { return; }
    ev.preventDefault();
    ev.dataTransfer.dropEffect = "move";
    var rect = target.getBoundingClientRect();
    var after = ev.clientY > rect.top + rect.height / 2;
    var seq = movable().filter(function (r) { return r !== dragged; });
    var at = seq.indexOf(target) + (after ? 1 : 0);
    seq.splice(at, 0, dragged);
    rows().forEach(function (r) { r.classList.toggle("drop-target", r === target); });
    layout(seq);
  });
  tbody.addEventListener("drop", function (ev) { if (dragged) { ev.preventDefault(); } });

  form.querySelector("[data-phase-order-reset]").addEventListener("click", function () { layout(original.slice()); });
  form.classList.add("dnd");
})();
