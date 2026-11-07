// ducktel dashboard — plain vanilla JS, no build step, no dependencies.
//
// Talks to the REST API in internal/webapi, which wraps the exact same
// internal/telemetry.Core the MCP tools use. Auth reuses the same read
// token as the MCP server: entered once here, kept in localStorage (so it
// survives closing the tab/browser — a deliberate convenience-over-exposure
// tradeoff; a 401 still clears it immediately), sent as a Bearer header on
// every /api/* call.

const TOKEN_KEY = "ducktel_token";

const state = {
  tenant: null,
  tenants: [],
  connected: false,
  // writtenHash is the last hash we set ourselves — see the hashchange
  // listener for why every self-write must be recognisable.
  writtenHash: "",
};

// --- deep links ---
//
// Every search/query run rewrites the URL hash to exactly the params it used,
// so the address bar is always a permalink to what's on screen — copy it,
// share it, or have an agent construct one directly (#traces?tenant=...&
// trace_id=... or #metrics?tenant=...&metric_name=...&aggregation=...) to
// land straight on a specific view instead of an empty form.

function parseHash() {
  const raw = location.hash.replace(/^#/, "");
  const qIdx = raw.indexOf("?");
  const view = qIdx === -1 ? raw : raw.slice(0, qIdx);
  const query = qIdx === -1 ? "" : raw.slice(qIdx + 1);
  return { view, params: new URLSearchParams(query) };
}

// setHash updates the fragment via replaceState (no history entry per
// search) and records what it wrote. The recording is load-bearing: browsers
// have been observed firing hashchange for replaceState fragment changes, so
// "we wrote it" is the only reliable way to tell our own URL updates apart
// from a real navigation.
function setHash(view, params) {
  const next = "#" + view + "?" + params.toString();
  if (location.hash !== next) {
    state.writtenHash = next;
    history.replaceState(null, "", next);
  }
}

// --- token / auth ---

function getToken() {
  return localStorage.getItem(TOKEN_KEY) || "";
}

function setToken(t) {
  localStorage.setItem(TOKEN_KEY, t);
}

function clearToken() {
  localStorage.removeItem(TOKEN_KEY);
}

// api issues an authenticated request to /api/*. On 401 it clears the stored
// token and re-shows the gate, since the token is either wrong or was never
// set (the server has no login flow — a 401 here always means "type the
// token again").
async function api(path, opts) {
  const headers = Object.assign({}, (opts && opts.headers) || {});
  const token = getToken();
  if (token) headers["Authorization"] = "Bearer " + token;

  const res = await fetch(path, Object.assign({}, opts, { headers }));
  if (res.status === 401) {
    clearToken();
    showGate("Rejected — check the token and try again.");
    throw new Error("unauthorized");
  }
  if (!res.ok) {
    const body = await res.text();
    throw new Error(body || (res.status + " " + res.statusText));
  }
  const ct = res.headers.get("Content-Type") || "";
  if (!ct.includes("application/json")) return null;

  const body = await res.json();
  // A query that matched nothing used to serialize as "rows":null (Go's nil
  // slice), and every caller here iterates rows — one null crashed the whole
  // post-login load with no visible error. The server now always sends []
  // for an empty result, but normalise here too so an older server can never
  // brick the UI again.
  if (body && body.rows === null) body.rows = [];
  return body;
}

function showGate(errorMsg) {
  document.getElementById("app").classList.add("hidden");
  document.getElementById("gate").classList.remove("hidden");
  document.getElementById("gate-error").textContent = errorMsg || "";
}

function hideGate() {
  document.getElementById("gate").classList.add("hidden");
  document.getElementById("app").classList.remove("hidden");
}

// --- toast ---

let toastTimer = null;
function toast(msg, isErr) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.classList.toggle("err", !!isErr);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.textContent = ""; }, 4000);
}

// --- tenant resolution ---
//
// The picker only appears if there's genuinely more than one tenant.
// With several (test tenants alongside the real one), the previously
// chosen tenant is remembered and restored, because defaulting to
// whichever sorts first is almost never the one you want.

const TENANT_KEY = "ducktel_tenant";

function preferredTenant() {
  const saved = localStorage.getItem(TENANT_KEY);
  return saved && state.tenants.includes(saved) ? saved : state.tenants[0];
}

async function resolveTenant() {
  const resp = await api("/api/tenants");
  state.tenants = resp.rows.map((r) => r.tenant).filter(Boolean);

  const wrap = document.getElementById("tenant-picker-wrap");
  const label = document.getElementById("tenant-label");
  const select = document.getElementById("tenant-picker");

  if (state.tenants.length === 0) {
    state.tenant = null;
    label.textContent = "no tenants seen yet";
    label.classList.remove("hidden");
    wrap.classList.add("hidden");
    return;
  }

  if (state.tenants.length === 1) {
    state.tenant = state.tenants[0];
    label.textContent = "tenant: " + state.tenant;
    label.classList.remove("hidden");
    wrap.classList.add("hidden");
    return;
  }

  // More than one — show the picker. Prefer the last tenant this browser
  // used, then fall back to the first alphabetically.
  label.classList.add("hidden");
  wrap.classList.remove("hidden");
  select.innerHTML = state.tenants.map((t) => `<option value="${escapeHtml(t)}">${escapeHtml(t)}</option>`).join("");
  state.tenant = preferredTenant();
  select.value = state.tenant;
  select.onchange = async () => {
    state.tenant = select.value;
    localStorage.setItem(TENANT_KEY, state.tenant);
    await loadServices();
    await loadMetricNames();
    await runTraceSearch();
  };
}

// syncTenantPicker reflects state.tenant into whichever tenant UI is visible
// (picker or label), for when a deep link names a tenant other than the
// auto-selected default.
function syncTenantPicker() {
  const wrap = document.getElementById("tenant-picker-wrap");
  if (!wrap.classList.contains("hidden")) {
    document.getElementById("tenant-picker").value = state.tenant;
  }
  const label = document.getElementById("tenant-label");
  if (!label.classList.contains("hidden")) {
    label.textContent = "tenant: " + state.tenant;
  }
}

// --- services (for the traces and logs filter dropdowns) ---

async function loadServices() {
  const tracesSelect = document.getElementById("f-service");
  const logsSelect = document.getElementById("l-service");
  if (!state.tenant) {
    tracesSelect.innerHTML = `<option value="">any</option>`;
    logsSelect.innerHTML = `<option value="">any</option>`;
    return;
  }
  const tenant = encodeURIComponent(state.tenant);

  // The two signals' service sets can differ; each dropdown asks for its own.
  const [tr, lg] = await Promise.all([
    api("/api/services?tenant=" + tenant),
    api("/api/services?tenant=" + tenant + "&source=logs"),
  ]);
  const fill = (select, resp) => {
    const services = resp.rows.map((r) => r.service_name).filter(Boolean);
    select.innerHTML = `<option value="">any</option>` + services
      .map((s) => `<option value="${escapeHtml(s)}">${escapeHtml(s)}</option>`)
      .join("");
  };
  fill(tracesSelect, tr);
  fill(logsSelect, lg);
}

// --- metric names (for the metrics form dropdown) ---
//
// metric_query matches metric_name exactly, and there was previously no way
// to discover what's actually been ingested short of guessing — this closes
// that gap the same way loadServices does for the traces filter.
async function loadMetricNames() {
  const select = document.getElementById("m-name");
  if (!state.tenant) {
    select.innerHTML = `<option value="">-- select --</option>`;
    return;
  }
  const resp = await api("/api/metric-names?tenant=" + encodeURIComponent(state.tenant));
  const names = resp.rows.map((r) => r.metric_name).filter(Boolean);
  const current = select.value;
  select.innerHTML = `<option value="">-- select --</option>` + names
    .map((n) => `<option value="${escapeHtml(n)}">${escapeHtml(n)}</option>`)
    .join("");
  if (names.includes(current)) select.value = current;
}

// --- config (flush button visibility) ---

async function loadConfig() {
  const cfg = await api("/api/config");
  document.getElementById("flush-btn").classList.toggle("hidden", !cfg.flush_enabled);
}

// --- tabs ---

function activateTab(name) {
  document.querySelectorAll(".tab-btn").forEach((b) => b.classList.toggle("active", b.dataset.tab === name));
  document.querySelectorAll(".tab").forEach((sec) => sec.classList.toggle("hidden", sec.id !== "tab-" + name));
  if (name === "traces") fitTracesSplit();
}

// fitTracesSplit sizes the traces tab to the remaining viewport so its two
// panes (table, waterfall) each scroll on their own instead of the page
// scrolling past the waterfall. The tab's own offset gives the header and
// filter height, so nothing here has to track their sizes. The floor keeps
// short windows on page-level scrolling rather than clipping either pane.
function fitTracesSplit() {
  const tab = document.getElementById("tab-traces");
  if (tab.classList.contains("hidden")) return;
  const top = tab.getBoundingClientRect().top;
  const h = Math.max(window.innerHeight - top - 16, 420);
  tab.style.height = h + "px";
}

function initTabs() {
  document.querySelectorAll(".tab-btn").forEach((btn) => {
    btn.addEventListener("click", () => activateTab(btn.dataset.tab));
  });
}

// --- traces view ---

function addAttrFilterRow(key, value) {
  const row = document.createElement("div");
  row.className = "attr-filter-row";
  row.innerHTML = `
    <input type="text" placeholder="key" class="attr-key">
    <input type="text" placeholder="value" class="attr-value">
    <button type="button" class="secondary remove-filter">×</button>`;
  row.querySelector(".attr-key").value = key || "";
  row.querySelector(".attr-value").value = value || "";
  row.querySelector(".remove-filter").addEventListener("click", () => row.remove());
  document.getElementById("f-attr-filters").appendChild(row);
}

function initAttrFilters() {
  document.getElementById("f-add-filter").addEventListener("click", () => addAttrFilterRow("", ""));
}

function collectAttrFilters() {
  const rows = document.querySelectorAll("#f-attr-filters .attr-filter-row");
  const out = [];
  rows.forEach((row) => {
    const k = row.querySelector(".attr-key").value.trim();
    const v = row.querySelector(".attr-value").value.trim();
    if (k) out.push(k + ":" + v);
  });
  return out;
}

function statusClass(code) {
  if (code === "STATUS_CODE_ERROR") return "status-error";
  if (code === "STATUS_CODE_OK") return "status-ok";
  return "status-unset";
}

function fmtTime(us) {
  if (!us) return "";
  return new Date(us / 1000).toLocaleString();
}

async function runTraceSearch() {
  if (!state.tenant) {
    toast("no tenant selected — send some telemetry first", true);
    return;
  }
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  const service = document.getElementById("f-service").value;
  if (service) params.set("service_name", service);
  params.set("since_minutes", document.getElementById("f-since").value || "60");
  params.set("limit", document.getElementById("f-limit").value || "100");
  collectAttrFilters().forEach((f) => params.append("filter", f));

  setHash("traces", params);

  let resp;
  try {
    resp = await api("/api/traces?" + params.toString());
  } catch (e) {
    toast(e.message, true);
    return;
  }

  const tbody = document.querySelector("#traces-table tbody");
  tbody.innerHTML = "";
  document.getElementById("waterfall").classList.add("hidden");

  if (resp.rows.length === 0) {
    tbody.innerHTML = `<tr><td colspan="6" class="empty">no spans matched</td></tr>`;
    return;
  }

  resp.rows.forEach((r) => {
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${fmtTime(r.start_time)}</td>
      <td>${escapeHtml(r.service_name || "")}</td>
      <td>${escapeHtml(r.span_name || "")}</td>
      <td>${(r.duration_ms || 0).toFixed(1)} ms</td>
      <td class="${statusClass(r.status_code)}">${escapeHtml((r.status_code || "").replace("STATUS_CODE_", ""))}</td>
      <td class="mono">${escapeHtml((r.trace_id || "").slice(0, 12))}…</td>`;
    tr.addEventListener("click", () => loadWaterfall(r.trace_id));
    tbody.appendChild(tr);
  });
}

async function loadWaterfall(traceId) {
  const tenant = encodeURIComponent(state.tenant);

  // Spans and the trace's log records are independent; fetch both up front.
  // The log fetch is best-effort: a waterfall without its logs is still
  // useful, so a failure there must not kill the whole view.
  const spansP = api("/api/traces/" + encodeURIComponent(traceId) + "?tenant=" + tenant);
  const logsP = api("/api/logs?tenant=" + tenant + "&trace_id=" + encodeURIComponent(traceId) + "&limit=1000")
    .then((r) => r.rows)
    .catch(() => []);

  let resp;
  try {
    resp = await spansP;
  } catch (e) {
    toast(e.message, true);
    logsP.catch(() => {});
    return;
  }
  const logs = await logsP;

  // Record trace_id in the hash so this exact waterfall is itself a
  // permalink, not just the search that found it.
  const { params: hashParams } = parseHash();
  hashParams.set("trace_id", traceId);
  setHash("traces", hashParams);

  const spans = resp.rows;
  const wf = document.getElementById("waterfall");
  wf.classList.remove("hidden");
  if (spans.length === 0) {
    wf.innerHTML = `<div class="empty">no spans</div>`;
    return;
  }

  // Attach each log record to its span via span_id; records whose span_id
  // matches no span (or is empty) are shown in a trace-level block instead
  // of being dropped.
  const spanIDs = new Set(spans.map((s) => s.span_id));
  const logsBySpan = new Map();
  const traceLevel = [];
  for (const l of logs) {
    if (l.span_id && spanIDs.has(l.span_id)) {
      if (!logsBySpan.has(l.span_id)) logsBySpan.set(l.span_id, []);
      logsBySpan.get(l.span_id).push(l);
    } else {
      traceLevel.push(l);
    }
  }

  const minStart = Math.min(...spans.map((s) => s.start_time));
  const maxEnd = Math.max(...spans.map((s) => s.end_time));
  const total = Math.max(maxEnd - minStart, 1);

  // Depth by walking the parent chain; capped so a cyclic/missing parent
  // cannot loop forever.
  const bySpanID = new Map(spans.map((s) => [s.span_id, s]));
  function depthOf(span) {
    let d = 0;
    let cur = span;
    while (cur && cur.parent_span_id && bySpanID.has(cur.parent_span_id) && d < 50) {
      cur = bySpanID.get(cur.parent_span_id);
      d++;
    }
    return d;
  }

  const rows = spans.map((s) => {
    const left = ((s.start_time - minStart) / total) * 100;
    const width = Math.max(((s.end_time - s.start_time) / total) * 100, 0.3);
    const depth = depthOf(s);
    const spanLogs = logsBySpan.get(s.span_id) || [];
    const logBlock = spanLogs.length
      ? `<div class="wf-logs" style="padding-left:${depth * 14 + 14}px">${spanLogs.map(logLineMarkup).join("")}</div>`
      : "";
    return `
      <div class="wf-row">
        <div class="wf-label" style="padding-left:${depth * 14}px" title="${escapeHtml(s.service_name)} — ${escapeHtml(s.span_name)}">
          ${escapeHtml(s.service_name)} — ${escapeHtml(s.span_name)}
        </div>
        <div class="wf-track">
          <div class="wf-bar ${statusClass(s.status_code)}" style="left:${left}%;width:${width}%"></div>
        </div>
        <div class="wf-duration">${(s.duration_ms || 0).toFixed(1)} ms</div>
      </div>` + logBlock;
  });

  let traceLevelBlock = "";
  if (traceLevel.length) {
    traceLevelBlock = `<h4 class="wf-log-heading">records not attached to a span</h4>
      <div class="wf-logs">${traceLevel.map(logLineMarkup).join("")}</div>`;
  }

  const logCount = logs.length
    ? ` — ${logs.length} log record${logs.length === 1 ? "" : "s"}`
    : "";
  wf.innerHTML = `<h3>Waterfall — ${escapeHtml(traceId)}${logCount}</h3>` + rows.join("") + traceLevelBlock;
  // The pane keeps its own scroll across loads; a freshly selected trace
  // should always start at its top.
  wf.scrollTop = 0;
}

// --- logs view ---

function severityClass(sev) {
  const s = (sev || "").toUpperCase();
  if (s === "ERROR" || s === "FATAL") return "status-error";
  if (s === "WARN") return "sev-warn";
  if (s === "DEBUG" || s === "TRACE") return "sev-dim";
  return "";
}

// traceCell renders the logs table's trace column: a truncated id (the row
// navigates to the waterfall) or an explicit muted marker. Records logged
// outside any span legitimately carry no trace id, and an empty cell reads
// as "something is broken" rather than "this record was never part of a
// trace".
function traceCell(traceId) {
  if (!traceId) return `<span class="no-trace">no trace</span>`;
  return `<span class="mono">${escapeHtml(traceId.slice(0, 12))}…</span>`;
}

// logLineMarkup renders one compact log record, used under a waterfall span
// (attached by span_id) and in the trace-level block.
function logLineMarkup(r) {
  return `<div class="wf-log">
    <span class="wf-log-sev ${severityClass(r.severity_text)}">${escapeHtml(r.severity_text || "")}</span>
    <span class="log-body" title="${escapeHtml(r.body || "")}">${escapeHtml(r.body || "")}</span>
    <span class="wf-log-time">${fmtTime(r.timestamp)}</span>
  </div>`;
}

// viewTrace jumps from a log row to its trace's waterfall: switches to the
// traces tab and loads it under a clean traces hash (the log filters would
// be meaningless there).
function viewTrace(traceId) {
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  params.set("trace_id", traceId);
  setHash("traces", params);
  activateTab("traces");
  loadWaterfall(traceId);
}

async function runLogSearch() {
  if (!state.tenant) {
    toast("no tenant selected — send some telemetry first", true);
    return;
  }
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  const service = document.getElementById("l-service").value;
  if (service) params.set("service_name", service);
  const severity = document.getElementById("l-severity").value;
  if (severity) params.set("severity", severity);
  const search = document.getElementById("l-search").value.trim();
  if (search) params.set("search", search);
  params.set("since_minutes", document.getElementById("l-since").value || "60");
  params.set("limit", document.getElementById("l-limit").value || "100");

  setHash("logs", params);

  let resp;
  try {
    resp = await api("/api/logs?" + params.toString());
  } catch (e) {
    toast(e.message, true);
    return;
  }

  const tbody = document.querySelector("#logs-table tbody");
  tbody.innerHTML = "";

  if (resp.rows.length === 0) {
    tbody.innerHTML = `<tr><td colspan="5" class="empty">no log records matched</td></tr>`;
    return;
  }

  resp.rows.forEach((r) => {
    const tr = document.createElement("tr");
    const traceId = r.trace_id || "";
    tr.innerHTML = `
      <td>${fmtTime(r.timestamp)}</td>
      <td>${escapeHtml(r.service_name || "")}</td>
      <td class="${severityClass(r.severity_text)}">${escapeHtml(r.severity_text || "")}</td>
      <td class="log-body" title="${escapeHtml(r.body || "")}">${escapeHtml(r.body || "")}</td>
      <td>${traceCell(traceId)}</td>`;
    if (traceId) {
      tr.classList.add("clickable");
      tr.addEventListener("click", () => viewTrace(traceId));
    }
    tbody.appendChild(tr);
  });
}

// --- metrics view ---

async function runMetricQuery() {
  if (!state.tenant) {
    toast("no tenant selected — send some telemetry first", true);
    return;
  }
  const metricName = document.getElementById("m-name").value;
  if (!metricName) {
    toast("choose a metric name first", true);
    return;
  }
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  params.set("metric_name", metricName);
  params.set("aggregation", document.getElementById("m-agg").value);
  params.set("since_minutes", document.getElementById("m-since").value || "60");
  const groupBy = document.getElementById("m-groupby").value.trim();
  if (groupBy) params.set("group_by", groupBy);

  setHash("metrics", params);

  const out = document.getElementById("metric-result");
  let resp;
  try {
    resp = await api("/api/metrics?" + params.toString());
  } catch (e) {
    toast(e.message, true);
    return;
  }

  if (resp.rows.length === 0) {
    out.innerHTML = `<div class="empty">no data points matched</div>`;
    return;
  }

  // metric_query aggregates the whole window into one value per group (or
  // one value overall) — not a time series — so a bar comparison (or a
  // single stat when there's nothing to compare) is the right shape, not a
  // line chart.
  const groupCol = resp.columns.find((c) => c !== "value");
  if (!groupCol) {
    out.innerHTML = `<div class="stat-big">${fmtValue(resp.rows[0].value)}</div>`;
    return;
  }

  const max = Math.max(...resp.rows.map((r) => Number(r.value) || 0), 1);
  const bars = resp.rows.map((r) => {
    const v = Number(r.value) || 0;
    const pct = (v / max) * 100;
    return `
      <div class="bar-row">
        <div class="bar-label" title="${escapeHtml(String(r[groupCol]))}">${escapeHtml(String(r[groupCol]))}</div>
        <div class="bar-track"><div class="bar-fill" style="width:${pct}%"></div></div>
        <div class="bar-value">${fmtValue(v)}</div>
      </div>`;
  });
  out.innerHTML = `<div class="bar-chart">${bars.join("")}</div>`;
}

function fmtValue(v) {
  if (v === null || v === undefined) return "—";
  const n = Number(v);
  return Number.isInteger(n) ? String(n) : n.toFixed(3);
}

// --- flush ---

async function runFlush() {
  try {
    await api("/api/flush", { method: "POST" });
    toast("flushed — buffered telemetry is now on disk");
  } catch (e) {
    toast(e.message, true);
  }
}

// --- logout ---
//
// The token now persists in localStorage (see the top-of-file note), so
// there needs to be an explicit way to forget it — otherwise the only way to
// clear a stored token is a 401 or manually clearing browser storage.
function logout() {
  clearToken();
  // Also forget the remembered tenant: logout is an explicit hand-over of
  // the browser, so the next person starts at the default.
  localStorage.removeItem(TENANT_KEY);
  state.tenant = null;
  state.tenants = [];
  state.connected = false;
  showGate();
}

// --- misc ---

function escapeHtml(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

// --- deep-link application ---

function fillTracesForm(params) {
  const service = params.get("service_name");
  if (service) document.getElementById("f-service").value = service;
  const since = params.get("since_minutes");
  if (since) document.getElementById("f-since").value = since;
  const limit = params.get("limit");
  if (limit) document.getElementById("f-limit").value = limit;

  const container = document.getElementById("f-attr-filters");
  container.innerHTML = "";
  params.getAll("filter").forEach((f) => {
    const idx = f.indexOf(":");
    const k = idx === -1 ? f : f.slice(0, idx);
    const v = idx === -1 ? "" : f.slice(idx + 1);
    addAttrFilterRow(k, v);
  });
}

function fillLogsForm(params) {
  const service = params.get("service_name");
  if (service) document.getElementById("l-service").value = service;
  const severity = params.get("severity");
  if (severity) document.getElementById("l-severity").value = severity;
  const search = params.get("search");
  if (search) document.getElementById("l-search").value = search;
  const since = params.get("since_minutes");
  if (since) document.getElementById("l-since").value = since;
  const limit = params.get("limit");
  if (limit) document.getElementById("l-limit").value = limit;
}

function fillMetricsForm(params) {
  const name = params.get("metric_name");
  if (name) document.getElementById("m-name").value = name;
  const agg = params.get("aggregation");
  if (agg) document.getElementById("m-agg").value = agg;
  const since = params.get("since_minutes");
  if (since) document.getElementById("m-since").value = since;
  const groupBy = params.get("group_by");
  if (groupBy) document.getElementById("m-groupby").value = groupBy;
}

// applyInitialView reads the URL hash (if any) and either lands directly on
// the view it describes, or falls back to the plain "show recent traces"
// default. Runs once after connecting, and again on hashchange (a pasted
// link, or back/forward) so the same open tab can be redirected to a
// different deep link without a full reload.
async function applyInitialView() {
  const { view, params } = parseHash();

  const hashTenant = params.get("tenant");
  if (hashTenant && state.tenants.includes(hashTenant) && hashTenant !== state.tenant) {
    state.tenant = hashTenant;
    localStorage.setItem(TENANT_KEY, state.tenant);
    syncTenantPicker();
    await loadServices();
    await loadMetricNames();
  }

  if (view === "logs") {
    activateTab("logs");
    fillLogsForm(params);
    if (state.tenant) {
      await runLogSearch();
    }
    return;
  }

  if (view === "metrics") {
    activateTab("metrics");
    fillMetricsForm(params);
    if (document.getElementById("m-name").value) {
      await runMetricQuery();
    }
    return;
  }

  activateTab("traces");
  fillTracesForm(params);
  if (state.tenant) {
    await runTraceSearch();
    const traceId = params.get("trace_id");
    if (traceId) {
      await loadWaterfall(traceId);
    }
  }
}

// onHashchange re-applies the view for a pasted deep link or back/forward
// navigation — but ONLY when the new hash is one we did not write ourselves.
// Browsers have been observed firing hashchange for our own replaceState
// fragment updates, and acting on those produced a live feedback loop:
// clicking a log's trace id set #traces?...&trace_id, the event re-ran
// applyInitialView, its trace search rewrote the hash without the trace id,
// the waterfall restored it, and every write re-triggered the other — an
// endless storm that left the view at the mercy of whichever call landed
// last (the reported "sometimes the same link does not load"). writtenHash
// is updated BEFORE replaceState precisely so even a synchronously-fired
// event is recognised as ours.
function onHashchange() {
  if (!state.connected) return;
  if (location.hash === state.writtenHash) return; // our own write, not navigation
  applyInitialView();
}

// --- boot ---

async function connect() {
  try {
    await resolveTenant();
  } catch (e) {
    if (e.message !== "unauthorized") toast(e.message, true);
    return;
  }
  hideGate();
  await loadServices();
  await loadMetricNames();
  await loadConfig();
  state.connected = true;
  // Lands on whatever the URL hash describes, or falls back to a plain
  // recent-traces view so the first thing you see isn't an empty table.
  await applyInitialView();
}

function initGate() {
  const input = document.getElementById("gate-token");
  const stored = getToken();
  if (stored) input.value = "";

  document.getElementById("gate-connect").addEventListener("click", async () => {
    const v = input.value.trim();
    if (v) setToken(v);
    await connect();
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") document.getElementById("gate-connect").click();
  });
}

function init() {
  initGate();
  initTabs();
  initAttrFilters();

  document.getElementById("traces-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runTraceSearch();
  });
  document.getElementById("logs-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runLogSearch();
  });
  document.getElementById("metrics-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runMetricQuery();
  });
  document.getElementById("flush-btn").addEventListener("click", runFlush);
  document.getElementById("logout-btn").addEventListener("click", logout);

  window.addEventListener("hashchange", onHashchange);
  // Keep the traces split sized to the window (a resized viewport changes the
  // space both panes get).
  window.addEventListener("resize", fitTracesSplit);

  // If a token is already stored (persists across tabs/restarts now — see
  // the top-of-file note on localStorage), skip the gate.
  if (getToken()) {
    connect();
  } else {
    showGate();
  }
}

init();
