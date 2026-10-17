// Overseer console — live updates (WBS 17.74–17.79, 17.136–17.139).
//
// Events are signals only: on each committed event the page refetches the
// affected server-rendered fragments. The browser keeps no workflow state
// and does no arithmetic on it, so duplicate events are harmless (the
// refetch is idempotent) and a sequence gap simply triggers a full
// refetch of every fragment. When the stream is unavailable the page falls
// back to bounded polling. Closing the browser never affects execution.
(function () {
  "use strict";
  var body = document.body;
  var project = body.getAttribute("data-live-project");
  var conn = document.getElementById("conn");
  var updated = document.getElementById("last-update");
  var last = parseInt(body.getAttribute("data-live-after") || "0", 10) || 0;
  var es = null, pollTimer = null, pollDelay = 10000, refreshTimer = null, errorSince = 0;

  function setState(state, label) {
    if (!conn) { return; }
    conn.setAttribute("data-state", state);
    conn.textContent = label;
  }
  function stamp() {
    if (updated) { updated.textContent = "Last update " + new Date().toLocaleTimeString(); }
  }
  function regions() { return document.querySelectorAll("[data-fragment-url]"); }

  function refresh(el) {
    var url = el.getAttribute("data-fragment-url");
    return fetch(url, { headers: { "Accept": "text/html" }, credentials: "same-origin", cache: "no-store" })
      .then(function (r) { if (!r.ok) { throw new Error(String(r.status)); } return r.text(); })
      .then(function (html) {
        // The fragment is rendered and escaped by the server templates.
        el.innerHTML = html;
        stamp();
      });
  }
  function refreshAll() {
    var rs = regions(), ps = [];
    for (var i = 0; i < rs.length; i++) { ps.push(refresh(rs[i]).catch(function () {})); }
    return Promise.all(ps);
  }
  function scheduleRefresh() {
    if (refreshTimer) { return; }
    refreshTimer = setTimeout(function () { refreshTimer = null; refreshAll(); }, 250);
  }

  function onEvent(ev) {
    var seq = parseInt(ev.lastEventId || "0", 10);
    if (!seq || seq <= last) { return; }          // duplicate delivery: ignore
    if (seq > last + 1) { last = seq; refreshAll(); return; } // gap: reconcile
    last = seq;
    scheduleRefresh();
  }

  function startPolling() {
    if (pollTimer) { return; }
    setState("offline", "Offline — polling");
    var tick = function () {
      pollTimer = null;
      if (document.hidden) { pollTimer = setTimeout(tick, pollDelay); return; }
      refreshAll().then(function () {
        pollDelay = Math.min(pollDelay * 2, 60000);  // bounded backoff
        pollTimer = setTimeout(tick, pollDelay);
      });
    };
    pollTimer = setTimeout(tick, pollDelay);
  }
  function stopPolling() {
    if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
    pollDelay = 10000;
  }

  function connect() {
    if (!project || !window.EventSource) { startPolling(); return; }
    setState("reconnecting", "Connecting");
    es = new EventSource("/api/v1/projects/" + encodeURIComponent(project) + "/events/stream?after=" + last);
    es.addEventListener("open", function () {
      errorSince = 0;
      stopPolling();
      setState("live", "Live");
      refreshAll();  // converge after (re)connect
    });
    es.addEventListener("execution", onEvent);
    es.addEventListener("reconnect", function () {
      es.close();
      setState("reconnecting", "Reconnecting");
      setTimeout(connect, 1000);
    });
    es.addEventListener("error", function () {
      if (!errorSince) { errorSince = Date.now(); }
      setState("reconnecting", "Reconnecting");
      if (es.readyState === EventSource.CLOSED || Date.now() - errorSince > 30000) {
        es.close();
        startPolling();
        setTimeout(function () { stopPolling(); connect(); }, 60000);
      }
    });
  }

  if (project) { connect(); } else { setState("offline", "Offline — static page"); }
})();
