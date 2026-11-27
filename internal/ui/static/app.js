/* pulse viewer UI — vanilla JS, no build step, no framework, no CDN. */
(() => {
  "use strict";

  const REFRESH_MS = 10000;
  const CHART_REFRESH_MS = 30000;
  const HISTORY_WINDOW = "6h";

  let chart = null;
  let targets = [];
  let selected = null;
  let chartFetchedAt = 0;
  let refreshing = false;

  const $ = (sel) => document.querySelector(sel);

  // Every string from the API is escaped: target names and error text are
  // operator-controlled but must never be able to inject markup.
  const escapeHTML = (value) =>
    String(value ?? "").replace(/[&<>"']/g, (ch) => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    })[ch]);

  const pct = (v) => (v == null ? "–" : (v * 100).toFixed(1) + "%");
  const ms = (v) => (v == null ? "–" : Math.round(v) + " ms");

  function ago(iso) {
    if (!iso) return "never";
    const seconds = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
    if (seconds < 90) return Math.round(seconds) + "s ago";
    if (seconds < 5400) return Math.round(seconds / 60) + "m ago";
    if (seconds < 172800) return Math.round(seconds / 3600) + "h ago";
    return Math.round(seconds / 86400) + "d ago";
  }

  async function api(path) {
    const res = await fetch(path, {
      headers: { Accept: "application/json" },
      cache: "no-store",
      credentials: "same-origin",
    });
    if (res.status === 401) throw new Error("authentication required — reload and sign in");
    if (!res.ok) throw new Error(path + ": " + res.status);
    return res.json();
  }

  function banner(message) {
    const el = $("#error");
    if (!message) { el.hidden = true; el.textContent = ""; return; }
    el.hidden = false;
    el.textContent = message;
  }

  function renderSummary(status) {
    const s = status.summary || {};
    const item = (label, value, cls) =>
      `<span><b class="${cls || ""}">${value ?? 0}</b>${escapeHTML(label)}</span>`;
    // "1 probers" in a header is the kind of thing a screenshot catches and a test
    // does not.
    const counted = (label, value, cls) =>
      item(value === 1 ? label.replace(/s$/, "") : label, value, cls);
    $("#summary").innerHTML = [
      item("up", s.up, "up-text"),
      item("down", s.down, "down-text"),
      item("stale", s.stale, "stale-text"),
      item("maint", s.maintenance, "maint-text"),
      item("unknown", s.unknown),
      counted("targets", s.targets),
      counted("probers", s.probers),
      s.probers_missing ? item("missing", s.probers_missing, "down-text") : "",
    ].join("");
    $("#updated").textContent = "updated " + new Date(status.generated_at).toLocaleTimeString();
  }

  function renderProbers(probers) {
    if (!probers.length) {
      $("#probers").innerHTML = `<span class="muted">No prober has checked in yet.</span>`;
      return;
    }
    $("#probers").innerHTML = probers.map((p) => {
      const lag = p.state === "MISSING" ? "never seen" : "seen " + ago(p.last_seen);
      const version = p.version ? " · " + escapeHTML(p.version) : "";
      return `<span class="prober">
        <span class="pill ${escapeHTML(String(p.state).toLowerCase())}">${escapeHTML(p.state)}</span>
        <strong>${escapeHTML(p.name)}</strong>
        <span class="lag">${escapeHTML(lag)}${version}</span>
      </span>`;
    }).join("");
  }

  function uptimeBars(uptime) {
    const windows = ["1h", "24h", "7d"];
    return `<div class="uptime">` + windows.map((w) => {
      const value = uptime && uptime[w] != null ? uptime[w] : null;
      const width = value == null ? 0 : Math.max(0, Math.min(1, value)) * 100;
      const cls = value == null ? "nodata" : (value < 0.99 ? "degraded" : "");
      return `<div class="uptime-row">
        <span class="uptime-label">${w}</span>
        <span class="uptime-bar"><span class="uptime-fill ${cls}" style="width:${width}%"></span></span>
        <span class="uptime-value">${pct(value)}</span>
      </div>`;
    }).join("") + `</div>`;
  }

  function renderTargets(targets) {
    if (!targets.length) {
      $("#targets").innerHTML = `<span class="muted">No targets reported yet.</span>`;
      return;
    }
    $("#targets").innerHTML = targets.map((t) => {
      const state = String(t.state || "UNKNOWN").toLowerCase();
      const latency = t.latency_ms || {};
      const failure = t.last && !t.last.ok
        ? `<p class="error">${escapeHTML(t.last.error_class || "error")}: ${escapeHTML(t.last.error || "")}</p>`
        : "";
      const planned = t.maintenance
        ? `<p class="planned">planned maintenance — recorded, not alarmed</p>`
        : "";
      return `<article class="card ${state}" data-target="${escapeHTML(t.name)}" data-prober="${escapeHTML(t.prober)}" tabindex="0"
                       role="button" aria-label="Show history for ${escapeHTML(t.name)} on ${escapeHTML(t.prober)}">
        <header>
          <span class="pill ${state}">${escapeHTML(t.state)}</span>
          <strong>${escapeHTML(t.name)}</strong>
          <span class="meta">${escapeHTML(t.group || "")}${t.group && t.type ? " · " : ""}${escapeHTML(t.type || "")}</span>
        </header>
        ${uptimeBars(t.uptime)}
        <div class="numbers">
          ${latency.p50 == null
            ? `<span class="muted">no successful probes</span>`
            : `<span>p50 ${ms(latency.p50)}</span><span>p95 ${ms(latency.p95)}</span>`}
          <span>${escapeHTML(t.prober)}</span>
        </div>
        ${failure}
        ${planned}
      </article>`;
    }).join("");
  }

  function destroyChart() {
    if (chart) { chart.destroy(); chart = null; }
  }

  function drawChart(results) {
    const host = $("#chart");
    destroyChart();
    host.innerHTML = "";
    if (!results.length) {
      host.innerHTML = `<p class="muted">No samples in the last ${HISTORY_WINDOW}.</p>`;
      return;
    }

    // uPlot wants column arrays; failed probes become nulls so the line breaks
    // instead of pretending the latency was zero.
    const xs = [];
    const ys = [];
    for (const r of results) {
      xs.push(Date.parse(r.at) / 1000);
      ys.push(r.ok ? r.duration_ms : null);
    }

    const width = Math.max(320, host.clientWidth || 640);
    chart = new uPlot({
      width,
      height: 220,
      scales: { x: { time: true } },
      legend: { show: false },
      series: [
        {},
        {
          label: "latency",
          stroke: "#58a6ff",
          width: 2,
          spanGaps: false,
          points: { show: results.length < 60, size: 3 },
          value: (u, v) => (v == null ? "fail/no data" : v + " ms"),
        },
      ],
      axes: [
        { stroke: "#8b949e", grid: { stroke: "#21262d" }, ticks: { stroke: "#21262d" } },
        { stroke: "#8b949e", grid: { stroke: "#21262d" }, ticks: { stroke: "#21262d" }, size: 44 },
      ],
    }, [xs, ys], host);
  }

  function renderDetailMeta(target, results) {
    const ok = results.filter((r) => r.ok).length;
    const failed = results.length - ok;
    const durations = results.filter((r) => r.ok).map((r) => r.duration_ms).sort((a, b) => a - b);
    const p50 = durations.length ? durations[Math.ceil(0.5 * durations.length) - 1] : null;
    const p95 = durations.length ? durations[Math.ceil(0.95 * durations.length) - 1] : null;
    const lastFailure = [...results].reverse().find((r) => !r.ok);

    const bits = [
      `${results.length} samples`,
      `${ok} ok`,
      `${failed} failed`,
      `p50 ${ms(p50)}`,
      `p95 ${ms(p95)}`,
    ];
    if (lastFailure) {
      bits.push(`last error: ${escapeHTML(lastFailure.error_class)}: ${escapeHTML(lastFailure.error || "")}`);
    }
    $("#detail-meta").innerHTML = bits.map((b) => `<span>${b}</span>`).join("");
    $("#import-url").textContent =
      `${target.prober} · ${target.group || "no group"} · ${target.type || "unknown type"} · ` +
      `state since ${target.since ? new Date(target.since).toLocaleString() : "unknown"}`;
  }

  function findTarget(prober, name) {
    return targets.find((t) => t.prober === prober && t.name === name);
  }

  async function selectTarget(prober, name) {
    if (!findTarget(prober, name)) return;
    if (!selected || selected.prober !== prober || selected.name !== name) {
      destroyChart();
      $("#chart").textContent = "Loading history…";
      $("#detail-meta").textContent = "";
      $("#import-url").textContent = "";
    }
    // A slow previous request must not draw one prober's data under another's title.
    const selection = { prober, name };
    selected = selection;
    $("#detail").hidden = false;
    $("#detail-title").textContent = `${name} · ${prober}`;
    chartFetchedAt = Date.now();
    try {
      const query = new URLSearchParams({ prober, target: name, window: HISTORY_WINDOW });
      const payload = await api(`/api/v1/history?${query}`);
      if (selected !== selection) return;
      const results = payload.results || [];
      drawChart(results);
      renderDetailMeta(findTarget(prober, name), results);
    } catch (err) {
      if (selected === selection) banner(err.message);
    }
  }

  async function refresh() {
    if (refreshing) return;
    refreshing = true;
    try {
      const status = await api("/api/v1/status");
      banner("");
      renderSummary(status);
      renderProbers(status.probers || []);
      targets = status.targets || [];
      renderTargets(targets);

      if (selected && !findTarget(selected.prober, selected.name)) {
        selected = null;
        $("#detail").hidden = true;
        destroyChart();
      } else if (selected && Date.now() - chartFetchedAt > CHART_REFRESH_MS) {
        void selectTarget(selected.prober, selected.name);
      }
    } catch (err) {
      banner(err.message);
    } finally {
      refreshing = false;
    }
  }

  function onResize() {
    if (!chart) return;
    const host = $("#chart");
    chart.setSize({ width: Math.max(320, host.clientWidth || 640), height: 220 });
  }

  document.addEventListener("click", (event) => {
    const card = event.target.closest("[data-target]");
    if (card) selectTarget(card.dataset.prober, card.dataset.target);
  });
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Enter" && event.key !== " ") return;
    const card = event.target.closest("[data-target]");
    if (!card) return;
    event.preventDefault();
    selectTarget(card.dataset.prober, card.dataset.target);
  });

  $("#refresh").addEventListener("click", refresh);
  $("#detail-close").addEventListener("click", () => {
    selected = null;
    $("#detail").hidden = true;
    destroyChart();
  });
  window.addEventListener("resize", onResize, { passive: true });

  refresh();
  setInterval(() => { if (!document.hidden) refresh(); }, REFRESH_MS);
})();
