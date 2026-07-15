(function () {
  const $ = (id) => document.getElementById(id);
  const btnOn = $("btn-on"), btnOff = $("btn-off");
  let es = null;
  let scenario = "cascade";

  const LABELS = {
    cascade: "<strong>Scenario:</strong> Agent #42 is handling 6 contacts. Contact #1001 fails <code>AssignContact</code>.",
    multi: "<strong>Scenario:</strong> Two agents (#42 and #55) active. Contact #1001 fails <code>AssignContact</code> on agent #42 — does the cascade spread?",
    stuck: "<strong>Scenario:</strong> Agent #50; contact #2001 is stuck in <code>ROUTING</code> past the ring timeout.",
    acw: "<strong>Scenario:</strong> Agent #60; contact #3001 stuck in <code>AFTER_CONTACT_WORK</code> past the ACW timeout — agent blocked.",
    queue: "<strong>Scenario:</strong> Contact #4001 stuck in <code>QUEUING</code> past the match SLA while agent #70 is available.",
    "live-cascade": "<strong>Live (ic-dev):</strong> <code>orch-entity-failure-queue</code> (CloudWatch) cascade bursts + real victims (Loki) — read-only, dry-run.",
    "live-stuck": "<strong>Live (ic-dev):</strong> contacts stuck in <code>ROUTING</code> past the ring timeout — real <code>ContactStateChangeV2</code> dwell (mon-na1 Loki).",
    "live-acw": "<strong>Live (ic-dev):</strong> agent contacts stuck in <code>AFTER_CONTACT_WORK</code> — real <code>AgentContactStateChangeV2</code> dwell (mon-na1 Loki).",
    "live-queue": "<strong>Live (ic-dev):</strong> contacts stuck in <code>QUEUING</code> past the match SLA — real <code>ContactStateChangeV2</code> dwell (mon-na1 Loki).",
  };
  // signal + default scan window per live option
  const LIVE = {
    "live-cascade": { signal: "cascade", scan: "24h" },
    "live-stuck": { signal: "stuck", scan: "2h" },
    "live-acw": { signal: "acw", scan: "2h" },
    "live-queue": { signal: "queue", scan: "2h" },
  };
  let liveES = null, liveName = "live-cascade";

  function setButtons(disabled) { btnOn.disabled = disabled; btnOff.disabled = disabled; }

  // ── Stepper helpers ──────────────────────────────────────────────────────────
  function stepReset() {
    ["step-detect", "step-diagnose", "step-heal"].forEach(id => {
      const el = $(id); if (el) el.className = "step";
    });
    ["step-line-1", "step-line-2"].forEach(id => {
      const el = $(id); if (el) el.className = "step-line";
    });
  }
  function stepActivate(stepId) {
    const el = $(stepId); if (el) el.className = "step active";
  }
  function stepDone(stepId, lineId, variant) {
    const el = $(stepId); if (el) el.className = "step " + (variant || "done");
    if (lineId) { const ln = $(lineId); if (ln) ln.className = "step-line done"; }
  }

  // ── Confidence meter color band ──────────────────────────────────────────────
  function confBand(pct) {
    if (pct < 70) return "low";
    if (pct < 80) return "mid";
    return "high";
  }

  function reset(mode) {
    $("feed").innerHTML = "";
    $("floor").innerHTML = "";
    $("stats").innerHTML = "";
    $("diag-body").classList.add("hidden");
    $("diag-empty").classList.remove("hidden");
    $("verdict").classList.add("hidden");
    stepReset();
    const pill = $("mode-pill");
    pill.textContent = mode === "on" ? "Sentinel ON" : "Sentinel OFF";
    pill.className = "pill " + (mode === "on" ? "pill-on" : "pill-off");
  }

  function tileClassFor(rec) {
    if (rec.Wiped) return "tile wiped";
    if (rec.Recovered) return "tile recovered";
    if (rec.Stuck) return "tile stuck";
    if (rec.State === "ROUTING") return "tile seed";
    return "tile healthy";
  }

  // renderScene builds the floor dynamically from all agent+contact records.
  // Works for single-agent (all existing scenarios) and multi-agent equally.
  function renderScene(d) {
    const floor = $("floor");
    floor.innerHTML = "";

    // Group contacts by agentNo
    const agents = d.records.filter((r) => r.Kind === "AGENT").sort((a, b) => a.ID - b.ID);
    const contactsByAgent = {};
    d.records.filter((r) => r.Kind === "CONTACT").forEach((r) => {
      const key = r.AgentNo || 0;
      if (!contactsByAgent[key]) contactsByAgent[key] = [];
      contactsByAgent[key].push(r);
    });

    // For single-agent scenarios the store has exactly 1 agent; multi has 2+
    agents.forEach((ag) => {
      const agWiped = ag.Wiped;
      const section = document.createElement("div");
      section.className = "floor-section";

      const agentRow = document.createElement("div");
      agentRow.className = "agent-row";

      const agTile = document.createElement("div");
      agTile.className = "tile agent" + (agWiped ? " wiped" : "");
      agTile.dataset.id = "agent-" + ag.ID;
      agTile.innerHTML =
        `<div class="tile-label">AGENT</div>` +
        `<div class="tile-id">#${ag.ID}</div>` +
        `<div class="tile-state">${agWiped ? "WIPED" : ag.State}</div>`;
      agentRow.appendChild(agTile);

      const arrow = document.createElement("div");
      arrow.className = "arrow";
      arrow.textContent = "handles ▸";
      agentRow.appendChild(arrow);
      section.appendChild(agentRow);

      const contactGrid = document.createElement("div");
      contactGrid.className = "contacts";
      const agContacts = (contactsByAgent[ag.AgentNo] || []).sort((a, b) => a.ID - b.ID);
      agContacts.forEach((r) => {
        const el = document.createElement("div");
        el.className = tileClassFor(r);
        el.dataset.id = r.ID;
        el.innerHTML =
          `<div class="tile-label">CONTACT</div>` +
          `<div class="tile-id">#${r.ID}</div>` +
          `<div class="tile-state">${r.Wiped ? "—" : r.State}</div>`;
        contactGrid.appendChild(el);
      });

      section.appendChild(contactGrid);
      floor.appendChild(section);
    });
  }

  function addLog(d) {
    // Advance stepper based on tag (only for Sentinel ON path)
    if (d.tag === "DETECT" || d.tag === "CWLOGS" || d.tag === "LOKI") {
      stepDone("step-detect", "step-line-1");
      stepActivate("step-diagnose");
    } else if (d.tag === "HEAL") {
      stepDone("step-diagnose", "step-line-2");
      stepDone("step-heal", null, "healed");
    }
    // FQUEUE/FAILURE tags don't advance the stepper — they belong to the OFF path
    const line = document.createElement("div");
    line.className = "feed-line";
    line.innerHTML =
      `<span class="ts">${d.ts}</span>` +
      `<span class="tag tag-${d.tag}">${d.tag}</span>` +
      `<span class="msg">${d.msg}</span>`;
    const feed = $("feed");
    feed.appendChild(line);
    feed.scrollTop = feed.scrollHeight;
  }

  function showDiagnosis(d) {
    $("diag-empty").classList.add("hidden");
    $("diag-body").classList.remove("hidden");
    $("diag-engine").textContent = d.engine || "FSM-grounded";
    $("diag-root").textContent = d.rootCause;
    $("diag-action").textContent = d.action;
    $("diag-expl").textContent = d.explanation;
    const pct = Math.round((d.confidence || 0) * 100);
    $("diag-conf").textContent = pct + "%";
    const meter = $("diag-meter");
    meter.setAttribute("data-conf", confBand(pct));
    setTimeout(() => (meter.style.width = pct + "%"), 50);
    // Advance stepper: diagnosis received
    stepDone("step-detect", "step-line-1");
    stepActivate("step-diagnose");
  }

  function markResult(d) {
    (d.preserved || []).forEach((id) => {
      const el = document.querySelector(`.tile[data-id="${id}"]`);
      if (el) el.className = "tile saved";
    });
    (d.quarantined || []).forEach((id) => {
      const el = document.querySelector(`.tile[data-id="${id}"]`);
      if (el) el.className = "tile wiped";
    });
  }

  function showSummary(d) {
    const stats = $("stats");
    stats.innerHTML = "";
    for (const s of d.stats) {
      const el = document.createElement("div");
      el.className = "stat" + (s.kind ? " " + s.kind : "");
      el.innerHTML = `<div class="stat-num">${s.num}</div><div class="stat-lbl">${s.label}</div>`;
      stats.appendChild(el);
    }
    const v = $("verdict");
    v.classList.remove("hidden");
    v.className = "verdict " + (d.verdict.good ? "good" : "bad");
    v.textContent = d.verdict.text;
    // Finalize stepper: good = healed (teal), bad = skipped (red — no diagnosis/heal applied)
    if (d.verdict.good) {
      stepDone("step-diagnose", "step-line-2");
      stepDone("step-heal", null, "healed");
    } else {
      stepDone("step-detect", "step-line-1");
      const diagEl = $("step-diagnose"); if (diagEl) diagEl.className = "step skipped";
      const line2 = $("step-line-2"); if (line2) line2.className = "step-line skipped";
      const healEl = $("step-heal"); if (healEl) healEl.className = "step skipped";
    }
  }

  const params = new URLSearchParams(location.search);
  const fast = params.get("fast") === "1" ? "&fast=1" : "";

  function run(mode) {
    if (es) es.close();
    reset(mode);
    setButtons(true);
    // Kick off stepper at DETECT stage
    stepActivate("step-detect");
    es = new EventSource(`/api/run?scenario=${scenario}&mode=${mode}${fast}`);
    es.addEventListener("scene", (e) => renderScene(JSON.parse(e.data)));
    es.addEventListener("log", (e) => addLog(JSON.parse(e.data)));
    es.addEventListener("diagnosis", (e) => showDiagnosis(JSON.parse(e.data)));
    es.addEventListener("result", (e) => markResult(JSON.parse(e.data)));
    es.addEventListener("summary", (e) => {
      showSummary(JSON.parse(e.data));
      es.close();
      setButtons(false);
    });
    es.onerror = () => { setButtons(false); if (es) es.close(); };
  }

  function showStatus(d) {
    const v = $("verdict");
    v.classList.remove("hidden");
    const cls = d.state === "error" || d.state === "not-configured" ? "bad" : "good";
    v.className = "verdict " + cls;
    v.textContent = (d.state === "connected" ? "● " : "") + d.text;
    if (d.engine) $("diag-engine").textContent = d.engine;
  }

  let liveClosing = false, liveOnce = false;
  function disconnectLive() {
    if (liveES) { liveClosing = true; liveES.close(); liveES = null; }
  }

  // opts: { lookback, once, signal }. once=Scan (single poll), !once=Watch (continuous 30s).
  function connectLive(opts) {
    disconnectLive();
    liveClosing = false;
    liveOnce = !!opts.once;
    // Refresh the right panel so only THIS action's output shows (no carry-over).
    $("feed").innerHTML = "";
    $("diag-body").classList.add("hidden");
    $("diag-empty").classList.remove("hidden");
    $("verdict").classList.add("hidden");
    stepReset();
    stepActivate("step-detect");
    $("mode-pill").textContent = opts.once ? "SCAN" : "● LIVE";
    $("mode-pill").className = "pill pill-on";
    showStatus({ state: "connecting", text: "Connecting to ic-dev…" });
    const q = new URLSearchParams();
    if (opts.lookback) q.set("lookback", opts.lookback);
    if (opts.signal) q.set("signal", opts.signal);
    if (opts.once) q.set("once", "1");
    liveES = new EventSource("/api/live?" + q.toString());
    liveES.addEventListener("status", (e) => showStatus(JSON.parse(e.data)));
    liveES.addEventListener("log", (e) => addLog(JSON.parse(e.data)));
    liveES.addEventListener("diagnosis", (e) => showDiagnosis(JSON.parse(e.data)));
    liveES.addEventListener("detection", (e) => {
      const d = JSON.parse(e.data);
      if (d.healthy) {
        addLog({ ts: "", tag: "OK", msg: `healthy — nothing detected in window` });
        stepDone("step-detect", "step-line-1");
        stepDone("step-diagnose", "step-line-2");
        stepDone("step-heal", null, "done");
      } else {
        addLog({ ts: "", tag: "DETECT", msg: `${d.total} detected → WOULD ${d.action}` });
        stepDone("step-detect", "step-line-1");
        stepDone("step-diagnose", "step-line-2");
        stepDone("step-heal", null, "healed");
      }
      // Scan mode: server closes after this detection; mark intentional so the imminent
      // onerror isn't shown as a failure (also used for headless screenshots).
      if (liveOnce) { liveClosing = true; setTimeout(disconnectLive, 250); }
    });
    liveES.onerror = () => { if (!liveClosing) showStatus({ state: "error", text: "Live stream disconnected." }); };
  }

  function startLive(once) {
    const cfg = LIVE[liveName] || LIVE["live-cascade"];
    connectLive({ signal: cfg.signal, lookback: once ? cfg.scan : "15m", once });
  }

  function pickScenario(name) {
    scenario = name;
    disconnectLive();
    $("scenario-label").innerHTML = LABELS[name];
    const live = name.startsWith("live-");
    if (live) liveName = name;
    // Mutually exclusive dropdowns: the active group shows the choice, the other resets.
    $("sim-select").value = live ? "" : name;
    $("live-select").value = live ? name : "";
    document.querySelector(".buttons").style.display = live ? "none" : "flex";
    $("live-controls").style.display = live ? "flex" : "none";
    reset("idle");
    $("mode-pill").textContent = "idle";
    $("mode-pill").className = "pill pill-idle";
    if (live) startLive(true); // default: retrospective scan for the chosen signal
  }

  $("sim-select").addEventListener("change", (e) => { if (e.target.value) pickScenario(e.target.value); });
  $("live-select").addEventListener("change", (e) => { if (e.target.value) pickScenario(e.target.value); });
  $("btn-scan").addEventListener("click", () => startLive(true));
  $("btn-watch").addEventListener("click", () => startLive(false));
  btnOn.addEventListener("click", () => run("on"));
  btnOff.addEventListener("click", () => run("off"));

  let initial = params.get("scenario");
  if (initial === "live") initial = "live-cascade"; // back-compat
  pickScenario(["cascade", "multi", "stuck", "acw", "queue", "live-cascade", "live-stuck", "live-acw", "live-queue"].includes(initial) ? initial : "cascade");

  // Headless/demo autorun: ?autorun=on|off fires a run on load.
  const autorun = params.get("autorun");
  if (autorun === "on" || autorun === "off") {
    setTimeout(() => run(autorun), 150);
  }
})();
