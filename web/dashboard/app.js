// ducktel dashboard — plain vanilla JS, no build step, no dependencies.
//
// Talks to the REST API in internal/webapi, which wraps the exact same
// internal/telemetry.Core the MCP tools use. Auth reuses the same read
// token as the MCP server. A browser that answered the HTTP Basic
// challenge on /dashboard is already authenticated (the password is the
// token); otherwise the token can be entered once here, kept in
// localStorage (so it survives closing the tab/browser — a deliberate
// convenience-over-exposure tradeoff; a 401 still clears it immediately),
// and sent as a Bearer header on every /api/* call.

import { applyInvestigateView, enterInvestigate, initInvestigate, refreshInvestigate } from "./investigate.js";

const TOKEN_KEY = "ducktel_token";

export const state = {
  tenant: null,
  tenants: [],
  connected: false,
  // metricInfo is {type, unit} of the selected metric, from metric_query.
  metricInfo: null,
  metricFilters: [],
  // min_value/max_value/limit of an exemplars deep link.
  metricRange: {},
  // writtenHash is the last hash we set ourselves — see the hashchange
  // listener for why every self-write must be recognisable.
  writtenHash: "",
};

// Request tokens per panel: a slower earlier run cannot overwrite a newer one.
const seq = {};
export function ticket(panel) { seq[panel] = (seq[panel] || 0) + 1; return seq[panel]; }
export function current(panel, t) { return seq[panel] === t; }

// --- deep links ---
//
// Every search/query run rewrites the URL hash to exactly the params it used,
// so the address bar is always a permalink to what's on screen — copy it,
// share it, or have an agent construct one directly (#traces?tenant=...&
// trace_id=... or #metrics?tenant=...&metric_name=...&aggregation=...) to
// land straight on a specific view instead of an empty form.

export function parseHash() {
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
export function setHash(view, params) {
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

// api issues an authenticated request to /api/*. A stored token is sent as
// Bearer. Otherwise the browser may already have HTTP Basic credentials
// from the /dashboard challenge; credentials: "same-origin" forwards them.
// A stored Bearer header replaces Basic on that request, so a leftover or
// rotated localStorage token would 401 even when Basic is valid. On 401,
// drop the stored token and retry once without Bearer so Basic can be
// used. Only a second 401 re-shows the gate.
export async function api(path, opts) {
  const token = getToken();
  let res = await apiFetch(path, opts, token);
  if (res.status === 401 && token) {
    clearToken();
    res = await apiFetch(path, opts, "");
  }
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

function apiFetch(path, opts, token) {
  const headers = Object.assign({}, (opts && opts.headers) || {});
  if (token) headers["Authorization"] = "Bearer " + token;
  else delete headers["Authorization"];
  return fetch(path, Object.assign({ credentials: "same-origin" }, opts, { headers }));
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
export function toast(msg, isErr) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.classList.toggle("err", !!isErr);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.textContent = ""; }, 4000);
}

function requireTenant() {
  if (!state.tenant) toast("no tenant selected - send some telemetry first", true);
  return !!state.tenant;
}
export function splitFilter(s) {
  const i = s.indexOf(":");
  return { key: i === -1 ? s : s.slice(0, i), value: i === -1 ? "" : s.slice(i + 1) };
}
export function joinFilter(key, value) { return key + ":" + value; }

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
    await Promise.all([loadServices(), loadMetricNames()]);
    // Re-run whichever tab is visible, so its results and hash agree with
    // the newly selected tenant.
    const tab = activeTab();
    if (tab === "logs") await runLogSearch();
    else if (tab === "metrics") await runMetricQuery();
    else if (tab === "investigate") await refreshInvestigate();
    else await runTraceSearch();
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

function activeTab() {
  const btn = document.querySelector(".tab-btn.active");
  return btn ? btn.dataset.tab : "traces";
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
    btn.addEventListener("click", () => {
      activateTab(btn.dataset.tab);
      if (btn.dataset.tab === "investigate") enterInvestigate();
    });
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
    if (k) out.push(joinFilter(k, v));
  });
  return out;
}

function statusClass(code) {
  if (code === "STATUS_CODE_ERROR") return "status-error";
  if (code === "STATUS_CODE_OK") return "status-ok";
  return "status-unset";
}

export function fmtTime(us) {
  if (!us) return "";
  return new Date(us / 1000).toLocaleString();
}

// Form <-> deep-link mapping: one table per form drives both directions.
const TRACES_FIELDS = [
  { param: "service_name", elementId: "f-service", default: "" },
  { param: "since_minutes", elementId: "f-since", default: "60" },
  { param: "limit", elementId: "f-limit", default: "100" },
  { param: "min_ms", elementId: "f-min-ms", default: "" },
  { param: "max_ms", elementId: "f-max-ms", default: "" },
];
const LOGS_FIELDS = [
  { param: "service_name", elementId: "l-service", default: "" },
  { param: "severity", elementId: "l-severity", default: "" },
  { param: "search", elementId: "l-search", default: "" },
  { param: "since_minutes", elementId: "l-since", default: "60" },
  { param: "limit", elementId: "l-limit", default: "100" },
];
const METRICS_FIELDS = [
  { param: "metric_name", elementId: "m-name", default: "" },
  { param: "aggregation", elementId: "m-agg", default: "avg" },
  { param: "since_minutes", elementId: "m-since", default: "60" },
  { param: "group_by", elementId: "m-groupby", default: "" },
];

function formParams(params, fields) {
  for (const { param, elementId, default: def } of fields) {
    const value = document.getElementById(elementId).value.trim();
    if (value || def) params.set(param, value || def);
  }
}

function fillFields(fields, params) {
  for (const { param, elementId, default: def } of fields) document.getElementById(elementId).value = params.get(param) || def;
}

// showTraceResults switches the traces top pane back from a slow-request
// list to the span search table.
function showTraceResults() {
  document.getElementById("slow-results").classList.add("hidden");
  document.getElementById("traces-table").classList.remove("hidden");
}

async function runTraceSearch(keepTraceId) {
  if (!requireTenant()) return;
  // A pending slow-request fetch belongs to a view we are leaving.
  ticket("slow");
  showTraceResults();
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  formParams(params, TRACES_FIELDS);
  collectAttrFilters().forEach((f) => params.append("filter", f));

  // Keep trace_id in the hash for a deep-linked trace, so the waterfall
  // stays a permalink.
  const hashParams = new URLSearchParams(params);
  if (keepTraceId) hashParams.set("trace_id", keepTraceId);
  setHash("traces", hashParams);

  const t = ticket("traces");
  let resp;
  try {
    resp = await api("/api/traces?" + params.toString());
  } catch (e) {
    if (current("traces", t)) toast(e.message, true);
    return;
  }
  if (!current("traces", t)) return;

  const tbody = document.querySelector("#traces-table tbody");
  tbody.innerHTML = "";
  if (!keepTraceId) { ticket("waterfall"); document.getElementById("waterfall").classList.add("hidden"); }

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
      <td>${traceCell(r.trace_id)}</td>`;
    tr.addEventListener("click", () => loadWaterfall(r.trace_id));
    tbody.appendChild(tr);
  });
}

// runSlowestSearch asks /api/slow for the slowest requests matching the
// traces form. It renders the rows into the same pane as the span search, so
// the form's service, duration and attribute filters mean the same thing.
async function runSlowestSearch() {
  if (!requireTenant()) return;
  // The span table and any pending span search are replaced by this view.
  ticket("traces");
  const t = ticket("slow");

  const base = new URLSearchParams({
    tenant: state.tenant,
    since_minutes: document.getElementById("f-since").value || "60",
  });
  const extra = { limit: "20" };
  const service = document.getElementById("f-service").value.trim();
  if (service) extra.service_name = service;
  const minMs = document.getElementById("f-min-ms").value.trim();
  const maxMs = document.getElementById("f-max-ms").value.trim();
  if (minMs) extra.min_value = minMs;
  if (maxMs) extra.max_value = maxMs;
  const params = slowParams(base, collectAttrFilters(), extra);

  let rows;
  try {
    rows = await api("/api/slow?" + params.toString());
  } catch (e) {
    if (current("slow", t)) toast(e.message, true);
    return;
  }
  if (!current("slow", t)) return;

  // This is a new result set, so an older waterfall no longer belongs to it.
  ticket("waterfall");
  document.getElementById("waterfall").classList.add("hidden");
  const box = document.getElementById("slow-results");
  box.innerHTML = "";
  box.appendChild(slowTable(rows || [], params.get("since_minutes")));
  document.getElementById("traces-table").classList.add("hidden");
  box.classList.remove("hidden");
}

// fmtMs formats a millisecond value from the API without inventing decimals
// it did not have.
export function fmtMs(v) {
  const n = Number(v);
  return (Number.isFinite(n) ? Math.round(n * 100) / 100 : 0) + " ms";
}

// traceSummaryMarkup renders the compact trace summary above the waterfall.
// Every number comes from /api/traces/{id}/summary; only the bar widths are
// scaled here, because a bar has to be a percentage of the pane.
export function traceSummaryMarkup(s) {
  if (!s) return "";
  const parts = [];
  const root = s.root;
  if (root && (root.service_name || root.span_name)) {
    parts.push(`${escapeHtml(root.service_name)} — ${escapeHtml(root.span_name)}`);
  }
  parts.push((Number(s.duration_ms) || 0).toFixed(1) + " ms");
  const untraced = Number(s.untraced_ms) || 0;
  if (untraced > 0) parts.push("untraced " + fmtMs(untraced));
  const spanCount = Number(s.span_count) || 0;
  parts.push(spanCount + " span" + (spanCount === 1 ? "" : "s") + (s.truncated ? " (truncated)" : ""));
  const errorCount = Number(s.error_count) || 0;
  if (errorCount > 0) parts.push(errorCount + " error" + (errorCount === 1 ? "" : "s"));
  const bySeverity = (s.logs && s.logs.by_severity) || {};
  const severities = Object.entries(bySeverity)
    .map(([sev, n]) => `${escapeHtml(sev)} ${escapeHtml(String(n))}`)
    .join(" · ");
  if (severities) parts.push("logs: " + severities);
  if (s.root_missing) parts.push("root missing");

  const groups = Array.isArray(s.groups) ? s.groups : [];
  let html = `<div class="trace-summary">
    <div class="ts-line">${parts.join(" · ")}</div>`;

  if (groups.length) {
    const duration = Number(s.duration_ms) || 0;
    const more = Number(s.groups_more) || 0;
    html += `<div class="ts-heading">where the time went${more > 0 ? ` · ${more} more group${more === 1 ? "" : "s"}` : ""}</div>
      <table class="data-table ts-groups">
        <thead><tr><th>Span</th><th>Count</th><th>Self</th><th>Total</th><th>Max</th><th>Errors</th></tr></thead>
        <tbody>`;
    html += groups.map((g) => {
      const self = Number(g.self_ms) || 0;
      let pct = duration > 0 ? (self / duration) * 100 : 0;
      if (!Number.isFinite(pct)) pct = 0;
      pct = Math.min(Math.max(pct, 0), 100);
      return `<tr class="ts-group" data-svc="${escapeHtml(g.service_name)}" data-name="${escapeHtml(g.span_name)}">
        <td class="ts-span-cell">
          <div class="ts-span">${escapeHtml(g.service_name)} — ${escapeHtml(g.span_name)}</div>
          <div class="ts-bar-track"><div class="ts-bar" style="width:${pct.toFixed(1)}%"></div></div>
        </td>
        <td>${escapeHtml(String(Number(g.count) || 0))}×</td>
        <td class="ts-self">${escapeHtml(fmtMs(g.self_ms))}</td>
        <td>${escapeHtml(fmtMs(g.total_ms))}</td>
        <td>${escapeHtml(fmtMs(g.max_ms))}</td>
        <td>${escapeHtml(String(Number(g.errors) || 0))}</td>
      </tr>`;
    }).join("");
    html += `</tbody></table>`;
  }

  const errors = Array.isArray(s.errors) ? s.errors : [];
  if (errors.length) {
    html += `<div class="ts-heading">errors</div><div class="ts-errors">` +
      errors.map((e) => `<div class="ts-error">${escapeHtml(e.service_name)} — ${escapeHtml(e.span_name)}: ${escapeHtml(e.status_message || "")}</div>`).join("") +
      `</div>`;
  }

  const problems = (s.logs && Array.isArray(s.logs.problems)) ? s.logs.problems : [];
  if (problems.length) {
    html += `<div class="wf-log-heading">log problems</div><div class="wf-logs">${problems.map(logLineMarkup).join("")}</div>`;
  }

  return html + `</div>`;
}

// bindSummaryGroups wires group rows to the waterfall rows they describe: the
// summary says where the time went, the highlight says which rows that is.
function bindSummaryGroups(wf) {
  wf.querySelectorAll(".ts-group").forEach((groupRow) => {
    groupRow.addEventListener("click", () => {
      const svc = groupRow.dataset.svc || "";
      const name = groupRow.dataset.name || "";
      const wasActive = groupRow.classList.contains("active-group");
      wf.querySelectorAll(".ts-group.active-group").forEach((r) => r.classList.remove("active-group"));
      wf.querySelectorAll(".wf-row.group-highlight").forEach((r) => r.classList.remove("group-highlight"));
      if (wasActive) return;
      groupRow.classList.add("active-group");
      let first = null;
      wf.querySelectorAll(".wf-row").forEach((r) => {
        if (r.dataset.svc === svc && r.dataset.name === name) {
          r.classList.add("group-highlight");
          if (!first) first = r;
        }
      });
      if (first) first.scrollIntoView({ block: "nearest" });
    });
  });
}

// renderWaterfall draws one trace's spans, logs and summary into wf. It is
// separate from loadWaterfall so the investigate tab can show the same
// waterfall inline without switching tabs.
export function renderWaterfall(wf, traceId, spans, logs, summary) {
  logs = Array.isArray(logs) ? logs : [];
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
      <div class="wf-row" data-svc="${escapeHtml(s.service_name)}" data-name="${escapeHtml(s.span_name)}">
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
  wf.innerHTML = `<h3>Waterfall — ${escapeHtml(traceId)}${logCount}</h3>` +
    traceSummaryMarkup(summary) + rows.join("") + traceLevelBlock;
  bindSummaryGroups(wf);
  // The pane keeps its own scroll across loads; a freshly selected trace
  // should always start at its top.
  wf.scrollTop = 0;
}

async function loadWaterfall(traceId) {
  const t = ticket("waterfall");
  const tenant = encodeURIComponent(state.tenant);

  // Spans, the trace's log records and its summary are independent; fetch
  // all three up front. The log and summary fetches are best-effort: a
  // waterfall without them is still useful, so their failures must not kill
  // the whole view.
  const spansP = api("/api/traces/" + encodeURIComponent(traceId) + "?tenant=" + tenant);
  const logsP = api("/api/logs?tenant=" + tenant + "&trace_id=" + encodeURIComponent(traceId) + "&limit=1000")
    .then((r) => r.rows)
    .catch(() => []);
  const summaryP = api("/api/traces/" + encodeURIComponent(traceId) + "/summary?tenant=" + tenant)
    .catch(() => null);

  let resp;
  try {
    resp = await spansP;
  } catch (e) {
    if (current("waterfall", t)) toast(e.message, true);
    logsP.catch(() => {});
    return;
  }
  if (!current("waterfall", t)) return;
  const logs = await logsP;
  if (!current("waterfall", t)) return;
  const summary = await summaryP;
  if (!current("waterfall", t)) return;

  // Record trace_id in the hash so this exact waterfall is itself a
  // permalink, not just the search that found it.
  const { params: hashParams } = parseHash();
  hashParams.set("trace_id", traceId);
  setHash("traces", hashParams);

  const spans = resp.rows;
  const wf = document.getElementById("waterfall");
  renderWaterfall(wf, traceId, spans, logs, summary);
}

// --- logs view ---

function severityClass(sev) {
  const s = (sev || "").toUpperCase();
  if (s === "ERROR" || s === "FATAL") return "status-error";
  if (s === "WARN") return "sev-warn";
  if (s === "DEBUG" || s === "TRACE") return "sev-dim";
  return "";
}

// traceCell renders a trace column: a truncated id (the row navigates to
// the waterfall) or an explicit muted marker. Records logged outside any
// span legitimately carry no trace id, and an empty cell reads as
// "something is broken" rather than "this record was never part of a
// trace".
function traceCell(traceId) {
  if (!traceId) return `<span class="no-trace">no trace</span>`;
  return `<span class="mono">${escapeHtml(traceId.slice(0, 12))}…</span>`;
}

// logLineMarkup renders one compact log record, used under a waterfall span
// (attached by span_id) and in the trace-level block.
export function logLineMarkup(r) {
  return `<div class="wf-log">
    <span class="wf-log-sev ${severityClass(r.severity_text)}">${escapeHtml(r.severity_text || "")}</span>
    <span class="log-body" title="${escapeHtml(r.body || "")}">${escapeHtml(r.body || "")}</span>
    <span class="wf-log-time">${fmtTime(r.timestamp)}</span>
  </div>`;
}

// viewTrace jumps to a trace's waterfall: switches to the traces tab and
// loads it under a clean traces hash. The optional since keeps an exemplar's
// time window; trace_id in the hash makes the waterfall a permalink.
function viewTrace(traceId, since) {
  // Keep the current view as a history entry, so Back returns to it (setHash
  // below replaces, it does not push).
  history.pushState(null, "", location.href);
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  if (since) params.set("since_minutes", since);
  params.set("trace_id", traceId);
  setHash("traces", params);
  activateTab("traces");
  loadWaterfall(traceId);
}

async function runLogSearch() {
  if (!requireTenant()) return;
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  formParams(params, LOGS_FIELDS);

  setHash("logs", params);

  const t = ticket("logs");
  let resp;
  try {
    resp = await api("/api/logs?" + params.toString());
  } catch (e) {
    if (current("logs", t)) toast(e.message, true);
    return;
  }
  if (!current("logs", t)) return;

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

// appendFilterParams writes filter pairs in the endpoint's own form. A string
// is an equality filter; an object carries op so exclusions reach filter_not.
function appendFilterParams(p, filters) {
  for (const f of filters || []) {
    if (typeof f === "string") {
      p.append("filter", f);
      continue;
    }
    const param = f.op === "!=" ? "filter_not" : "filter";
    p.append(param, joinFilter(f.key, f.value));
  }
}

export function metricParams(base, aggregation, filters, extra) {
  const p = new URLSearchParams();
  for (const k of ["tenant", "metric_name", "since_minutes", "until_minutes"]) if (base.has(k)) p.set(k, base.get(k));
  p.set("aggregation", aggregation);
  appendFilterParams(p, filters);
  for (const [k, v] of Object.entries(extra || {})) if (v !== null && v !== undefined && v !== "") p.set(k, v);
  return p;
}

// slowParams is the /api/slow counterpart of metricParams: the same base
// window, the same filter pairs, and the endpoint's own extra parameters.
export function slowParams(base, filters, extra) {
  const p = new URLSearchParams();
  for (const k of ["tenant", "metric_name", "since_minutes", "until_minutes"]) if (base.has(k)) p.set(k, base.get(k));
  appendFilterParams(p, filters);
  for (const [k, v] of Object.entries(extra || {})) if (v !== null && v !== undefined && v !== "") p.set(k, v);
  return p;
}

async function runMetricQuery() {
  if (!requireTenant()) return;
  const metricName = document.getElementById("m-name").value;
  if (!metricName) {
    toast("choose a metric name first", true);
    return;
  }
  const base = new URLSearchParams({ tenant: state.tenant, metric_name: metricName, since_minutes: document.getElementById("m-since").value || "60" });
  const agg = document.getElementById("m-agg").value;
  const groupBy = document.getElementById("m-groupby").value.trim();
  const extra = groupBy ? { group_by: groupBy } : {};
  if (agg === "exemplars") Object.assign(extra, state.metricRange);
  const params = metricParams(base, agg, state.metricFilters, extra);

  setHash("metrics", params);

  const out = document.getElementById("metric-result");
  const dist = document.getElementById("metric-dist");
  dist.innerHTML = "";
  const t = ticket("metrics");
  const infoP = loadMetricInfo(t);
  let resp;
  try {
    [resp] = await Promise.all([api("/api/metrics?" + params.toString()), infoP]);
  } catch (e) {
    if (current("metrics", t)) toast(e.message, true);
    return;
  }
  if (!current("metrics", t)) return;
  const unit = metricUnit();

  if (resp.rows.length === 0) {
    out.innerHTML = `<div class="empty">no data points matched</div>`;
    return;
  }

  // metric_query aggregates the whole window into one value per group (or
  // one value overall) — not a time series — so a bar comparison (or a
  // single stat when there's nothing to compare) is the right shape, not a
  // line chart.
  if (agg === "exemplars") {
    out.innerHTML = "";
    out.appendChild(exemplarTable(resp, unit, params.get("since_minutes")));
    return;
  }
  const valueUnit = agg === "count" ? "" : unit;
  const isHistogram = state.metricInfo && state.metricInfo.type === "histogram";
  const groupCols = resp.columns.filter((c) => c !== "value");
  if (groupCols.length === 0) {
    out.innerHTML = `<div class="stat-big">${fmtValue(resp.rows[0].value, valueUnit)}</div>`;
    if (isHistogram) await renderDistribution("all", state.metricFilters, params);
    return;
  }

  const max = Math.max(...resp.rows.map((r) => Number(r.value) || 0), 1);
  const labels = [];
  const bars = resp.rows.map((r, i) => {
    const v = Number(r.value) || 0;
    const pct = (v / max) * 100;
    // Every group key in the label, not just the first.
    const label = groupCols.map((c) => (r[c] === null || r[c] === undefined ? "—" : String(r[c]))).join(" · ");
    labels[i] = label;
    const filters = isHistogram ? groupFilters(groupCols, r) : null;
    const clickable = filters ? ` clickable" data-idx="${i}` : "";
    return `
      <div class="bar-row${clickable}" title="${escapeHtml(label)}${filters ? " — click for the distribution" : ""}">
        <div class="bar-label">${escapeHtml(label)}</div>
        <div class="bar-track"><div class="bar-fill" style="width:${pct}%"></div></div>
        <div class="bar-value">${fmtValue(v, valueUnit)}</div>
      </div>`;
  });
  out.innerHTML = `<div class="bar-chart">${bars.join("")}</div>`;
  if (isHistogram) {
    out.querySelectorAll(".bar-row.clickable").forEach((el) => {
      el.addEventListener("click", () => {
        const i = Number(el.dataset.idx);
        renderDistribution(labels[i], state.metricFilters.concat(groupFilters(groupCols, resp.rows[i])), params);
      });
    });
  }
}

// --- metric type and unit ---
//
// Asked through the same metric_query an agent uses: count grouped by
// metric_type and metric_unit. No separate endpoint, so the dashboard sees
// exactly what the MCP tools see. Cached per tenant/metric/window because
// otherwise every Run repeats this query.
const metricInfoCache = new Map();

async function loadMetricInfo(t) {
  const el = document.getElementById("m-info");
  const name = document.getElementById("m-name").value;
  const since = document.getElementById("m-since").value || "60";
  state.metricInfo = null;
  el.textContent = "";
  if (!state.tenant || !name) return;
  const key = [state.tenant, name, since].join("|");
  let row = metricInfoCache.get(key);
  if (!metricInfoCache.has(key)) {
    const p = metricParams(new URLSearchParams({ tenant: state.tenant, metric_name: name, since_minutes: since }), "count", [], { group_by: "metric_type,metric_unit" });
    let resp;
    try { resp = await api("/api/metrics?" + p.toString()); } catch (e) { return; }
    if (t !== undefined && !current("metrics", t)) return;
    row = (resp && resp.rows[0]) || null;
    metricInfoCache.set(key, row);
  }
  if (t !== undefined && !current("metrics", t)) return;
  if (!row) return;
  state.metricInfo = { type: row.metric_type, unit: row.metric_unit || "" };
  const hint = {
    histogram: "count = observations · p50/p95/p99 from buckets · click a bar for the distribution",
    exponential_histogram: "count = observations",
    gauge: "count = exported points, not events",
    sum: "count = exported points, not events",
  }[row.metric_type] || "";
  el.innerHTML = `<b>${escapeHtml(row.metric_type)}${state.metricInfo.unit ? " · " + escapeHtml(state.metricInfo.unit) : ""}</b>` +
    (hint ? ` <span class="muted">${escapeHtml(hint)}</span>` : "");
}

function metricUnit() {
  return (state.metricInfo && state.metricInfo.unit) || "";
}

// groupFilters turns one result row's group values into filter=key:value
// pairs, so the distribution of that bar is the same metric_query with a
// filter. Only keys a filter can match (data-point or resource attributes)
// qualify; a missing value cannot be filtered on.
function groupFilters(groupCols, row) {
  const out = [];
  for (const c of groupCols) {
    const v = row[c];
    if (v === null || v === undefined) return null;
    let key = null;
    if (c.startsWith("attr.")) key = c.slice(5);
    else if (c.startsWith("attributes.")) key = c.slice(11);
    else if (c === "service_name") key = "service.name";
    if (!key) return null;
    out.push(joinFilter(key, v));
  }
  return out;
}

// --- histogram distribution ---
//
// aggregation=buckets on the same metric_query, plus p50/p95/p99 from the
// server, so nothing is re-derived here.
// renderDistribution is the metrics tab's distribution for the current form.
// The investigate tab reuses renderDistributionInto with its own target, unit
// and bucket handler, so both views present the same buckets and quantiles.
async function renderDistribution(label, filters, baseParams) {
  return renderDistributionInto(document.getElementById("metric-dist"), label, filters, baseParams, {});
}

export async function renderDistributionInto(target, label, filters, baseParams, opts) {
  opts = opts || {};
  const key = opts.ticketKey || "dist";
  const t = ticket(key);
  const ask = (agg) => api("/api/metrics?" + metricParams(baseParams, agg, filters).toString());
  let buckets, p50, p95, p99;
  try {
    [buckets, p50, p95, p99] = await Promise.all(["buckets", "p50", "p95", "p99"].map(ask));
  } catch (e) {
    if (current(key, t)) {
      if (opts.onError) opts.onError(e.message);
      else toast(e.message, true);
    }
    return;
  }
  if (!current(key, t)) return;
  const rows = buckets.rows;
  const total = rows.reduce((s, r) => s + (Number(r.value) || 0), 0);
  if (total === 0) {
    target.innerHTML = `<div class="empty">no observations for ${escapeHtml(label)}</div>`;
    return;
  }
  // Trim empty buckets at both ends; keep empty ones in between.
  let first = rows.findIndex((r) => Number(r.value) > 0);
  let last = rows.length - 1 - [...rows].reverse().findIndex((r) => Number(r.value) > 0);
  const shown = rows.slice(first, last + 1);
  const unit = opts.unit !== undefined ? opts.unit : metricUnit();
  const q = { p50: val(p50), p95: val(p95), p99: val(p99) };
  const prevLe = (i) => (i > 0 ? rows[i - 1].le : null);
  const max = Math.max(...shown.map((r) => Number(r.value) || 0), 1);
  const lines = shown.map((r, j) => {
    const i = first + j;
    const le = r.le;
    const lo = prevLe(i);
    const name = le === null ? `> ${fmtValue(lo, unit)}` : `≤ ${fmtValue(le, unit)}`;
    const marks = Object.entries(q)
      .filter(([, v]) => v !== null && (lo === null || v > lo) && (le === null || v <= le))
      .map(([k]) => `<span class="qmark">${k}</span>`).join("");
    const v = Number(r.value) || 0;
    const selected = opts.selectedFn && opts.selectedFn(lo, le) ? " selected" : "";
    return `
      <div class="bar-row clickable${selected}" data-i="${i}" title="click for the requests in this bucket">
        <div class="bar-label">${name}</div>
        <div class="bar-track"><div class="bar-fill dist" style="width:${(v / max) * 100}%"></div></div>
        <div class="bar-value">${fmtValue(v)}</div>
        <div class="bar-marks">${marks}</div>
      </div>`;
  });
  // The metrics tab needs a place for the bucket's requests; investigate
  // handles the click itself and passes no slot.
  const slot = opts.onBucket ? "" : `<div id="${opts.bucketSlotId || "bucket-req"}"></div>`;
  target.innerHTML = `
    <div class="dist-head"><b>${escapeHtml(label)}</b> · ${fmtValue(total)} observations ·
      p50 ${fmtValue(q.p50, unit)} · p95 ${fmtValue(q.p95, unit)} · p99 ${fmtValue(q.p99, unit)}</div>
    <div class="bar-chart">${lines.join("")}</div>
    ${slot}`;
  target.querySelectorAll(".bar-row.clickable").forEach((el) => {
    el.addEventListener("click", () => {
      const i = Number(el.dataset.i);
      if (opts.onBucket) opts.onBucket(prevLe(i), rows[i].le, unit);
      else showBucketRequests(prevLe(i), rows[i].le, filters, baseParams, unit);
    });
  });
}

// --- from a bucket to the requests in it ---
//
// /api/slow on the same metric, narrowed to the bucket's bounds: the concrete
// requests the SDK attached to those observations, each already carrying its
// brief trace summary. Each leads to its trace (waterfall + logs). "All spans
// in this range" is span_search with min_ms/max_ms, for when the results are
// too few.
async function showBucketRequests(lo, le, filters, baseParams, unit) {
  const t = ticket("bucket");
  const box = document.getElementById("bucket-req");
  const extra = { limit: "20" };
  if (lo !== null) extra.min_value = lo;
  if (le !== null) extra.max_value = le;
  const p = slowParams(baseParams, filters, extra);
  let rows;
  try {
    rows = await api("/api/slow?" + p.toString());
  } catch (e) {
    if (current("bucket", t)) toast(e.message, true);
    return;
  }
  if (!current("bucket", t)) return;
  rows = rows || [];
  const range = `${lo === null ? "" : fmtValue(lo, unit)} – ${le === null ? "∞" : fmtValue(le, unit)}`;
  box.innerHTML = `<div class="dist-head"><b>requests ${range}</b> ·
    ${rows.length} request${rows.length === 1 ? "" : "s"}</div>`;

  // Same bounds as a span duration filter, when the unit converts to ms.
  const toMs = { ms: 1, s: 1000, us: 0.001 }[unit];
  if (toMs !== undefined) {
    const sp = new URLSearchParams();
    sp.set("tenant", baseParams.get("tenant"));
    sp.set("since_minutes", baseParams.get("since_minutes"));
    sp.set("limit", "100");
    if (lo !== null) sp.set("min_ms", lo * toMs);
    if (le !== null) sp.set("max_ms", le * toMs);
    const svc = filters.map(splitFilter).find((f) => f.key === "service.name");
    if (svc) sp.set("service_name", svc.value);
    box.querySelector(".dist-head").insertAdjacentHTML("beforeend",
      ` · <a href="#traces?${escapeHtml(sp.toString())}">all spans in this range →</a>`);
  }
  if (rows.length === 0) {
    box.insertAdjacentHTML("beforeend", `<div class="empty">no exemplars in this bucket — the sender attached none, or they had no trace</div>`);
    return;
  }
  box.appendChild(slowTable(rows, baseParams.get("since_minutes")));
}

// slowTable renders /api/slow rows: the measurement and the brief trace
// summary the API already attached, so no follow-up query is needed. opts
// can replace the default tab-switching trace click.
export function slowTable(rows, since, opts) {
  opts = opts || {};
  const table = document.createElement("table");
  table.className = "data-table";
  table.innerHTML = `<thead><tr><th>Time</th><th>Value</th><th>Request</th><th>Spans</th>
    <th>Untraced</th><th>Top group</th><th>Errors</th><th>Log</th><th>Trace</th></tr></thead><tbody></tbody>`;
  const tbody = table.querySelector("tbody");
  if (!rows.length) {
    tbody.innerHTML = `<tr><td colspan="9" class="empty">no slow requests matched</td></tr>`;
    return table;
  }
  rows.forEach((r) => {
    const trace = r.trace || {};
    const root = trace.root;
    const groups = Array.isArray(trace.groups) ? trace.groups : [];
    const g = groups[0];
    const problems = (trace.logs && Array.isArray(trace.logs.problems)) ? trace.logs.problems : [];
    const p = problems[0];
    const request = root && root.span_name ? root.span_name : "-";
    const untraced = Number(trace.untraced_ms) || 0;
    const top = g ? `${Number(g.count) || 0}× ${g.span_name} · self ${fmtMs(g.self_ms)}` : "-";
    const logText = p ? `${p.severity_text || ""} ${p.body || ""}`.trim() : "";
    const tr = document.createElement("tr");
    tr.className = "clickable";
    tr.innerHTML = `
      <td>${escapeHtml(fmtTime(r.timestamp))}</td>
      <td>${escapeHtml(fmtValue(r.value, r.unit))}</td>
      <td class="ts-request" title="${escapeHtml(request)}">${escapeHtml(request)}</td>
      <td>${escapeHtml(String(Number(trace.span_count) || 0))}</td>
      <td>${escapeHtml(untraced > 0 ? fmtMs(untraced) : "-")}</td>
      <td class="ts-top" title="${escapeHtml(top)}">${escapeHtml(top)}</td>
      <td>${escapeHtml(String(Number(trace.error_count) || 0))}</td>
      <td class="ts-log" title="${escapeHtml(logText)}">${p ? `<span class="${severityClass(p.severity_text)}">${escapeHtml(p.severity_text || "")}</span> ${escapeHtml(p.body || "")}` : "-"}</td>
      <td>${traceCell(r.trace_id)}</td>`;
    tr.addEventListener("click", () => {
      if (opts.onTraceClick) opts.onTraceClick(r.trace_id);
      else viewTrace(r.trace_id, since);
    });
    tbody.appendChild(tr);
  });
  return table;
}

// exemplarTable renders aggregation=exemplars rows; a row opens its trace.
function exemplarTable(resp, unit, since) {
  const groupCols = resp.columns.filter((c) => !["timestamp", "value", "trace_id", "span_id"].includes(c));
  const table = document.createElement("table");
  table.className = "data-table";
  table.innerHTML = `<thead><tr><th>Time</th>${groupCols.map((c) => `<th>${escapeHtml(c)}</th>`).join("")}
    <th>Value</th><th>Trace</th></tr></thead><tbody></tbody>`;
  const tbody = table.querySelector("tbody");
  if (resp.rows.length === 0) {
    tbody.innerHTML = `<tr><td colspan="${groupCols.length + 3}" class="empty">no exemplars</td></tr>`;
  }
  resp.rows.forEach((r) => {
    const tr = document.createElement("tr");
    tr.className = "clickable";
    tr.innerHTML = `<td>${fmtTime(r.timestamp)}</td>` +
      groupCols.map((c) => `<td>${escapeHtml(r[c] === null || r[c] === undefined ? "—" : r[c])}</td>`).join("") +
      `<td>${fmtValue(r.value, unit)}</td><td>${traceCell(r.trace_id)}</td>`;
    tr.addEventListener("click", () => viewTrace(r.trace_id, since));
    tbody.appendChild(tr);
  });
  return table;
}

// --- metric filters (from a deep link) ---
//
// The metrics form has no filter editor; filters arrive in a deep link
// (an agent's metric_query with filters) and are shown as removable chips.
function setMetricFilters(list) {
  state.metricFilters = list.slice();
  const el = document.getElementById("m-filters");
  const r = state.metricRange;
  const range = r.min_value || r.max_value || r.limit
    ? `<span class="chip">value ${escapeHtml(r.min_value || "")}–${escapeHtml(r.max_value || "∞")}${r.limit ? ", limit " + escapeHtml(r.limit) : ""} <button type="button" data-range="1" title="remove">×</button></span>`
    : "";
  el.innerHTML = range + state.metricFilters.map((f, i) =>
    `<span class="chip">${escapeHtml(f)} <button type="button" data-i="${i}" title="remove">×</button></span>`).join("");
  const rb = el.querySelector("button[data-range]");
  if (rb) rb.addEventListener("click", () => { state.metricRange = {}; setMetricFilters(state.metricFilters); runMetricQuery(); });
  el.querySelectorAll("button[data-i]").forEach((b) => b.addEventListener("click", () => {
    setMetricFilters(state.metricFilters.filter((_, j) => j !== Number(b.dataset.i)));
    runMetricQuery();
  }));
}

function val(resp) {
  const r = resp && resp.rows[0];
  return r && r.value !== null && r.value !== undefined ? Number(r.value) : null;
}

// fmtValue returns HTML: metric_unit comes from whatever the sender reported,
// so it is escaped here, and callers put the result into markup as is.
export function fmtValue(v, unit) {
  if (v === null || v === undefined) return "—";
  const n = Number(v);
  const s = Number.isInteger(n) ? String(n) : n.toFixed(3);
  return unit ? `${s} ${escapeHtml(unit)}` : s;
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

export function escapeHtml(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

// --- deep-link application ---

function fillTracesForm(params) {
  fillFields(TRACES_FIELDS, params);

  const container = document.getElementById("f-attr-filters");
  container.innerHTML = "";
  params.getAll("filter").forEach((f) => {
    const { key, value } = splitFilter(f);
    addAttrFilterRow(key, value);
  });
}

function fillLogsForm(params) {
  fillFields(LOGS_FIELDS, params);
}

function fillMetricsForm(params) {
  fillFields(METRICS_FIELDS, params);
  state.metricRange = {};
  for (const k of ["min_value", "max_value", "limit"]) {
    if (params.get(k)) state.metricRange[k] = params.get(k);
  }
  setMetricFilters(params.getAll("filter"));
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
    await Promise.all([loadServices(), loadMetricNames()]);
  }

  if (view === "investigate" || view === "compare") {
    activateTab("investigate");
    await applyInvestigateView(params);
    return;
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
    const traceId = params.get("trace_id");
    // Load the deep-linked waterfall before the slow trace list search.
    if (traceId) await loadWaterfall(traceId);
    await runTraceSearch(traceId);
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
  await Promise.all([loadServices(), loadMetricNames(), loadConfig()]);
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
  initInvestigate();

  document.getElementById("traces-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runTraceSearch();
  });
  document.getElementById("f-slowest").addEventListener("click", runSlowestSearch);
  document.getElementById("logs-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runLogSearch();
  });
  document.getElementById("metrics-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runMetricQuery();
  });
  document.getElementById("m-name").addEventListener("change", () => {
    // Filters from a deep link belonged to the previous metric.
    state.metricRange = {};
    setMetricFilters([]);
    loadMetricInfo(ticket("metrics"));
  });
  document.getElementById("m-agg").addEventListener("change", () => {
    if (document.getElementById("m-agg").value !== "exemplars") {
      state.metricRange = {};
      setMetricFilters(state.metricFilters);
    }
  });
  document.getElementById("flush-btn").addEventListener("click", runFlush);
  document.getElementById("logout-btn").addEventListener("click", logout);

  window.addEventListener("hashchange", onHashchange);
  // Keep the traces split sized to the window (a resized viewport changes the
  // space both panes get).
  window.addEventListener("resize", fitTracesSplit);

  // Try to connect even with no stored token: a browser that already
  // answered the /dashboard Basic challenge will get in without pasting
  // the token again. api() shows the gate on 401.
  connect();
}

// Scripts that only import the pure helpers (the node tests) have no DOM,
// and must not try to boot the dashboard.
if (typeof document !== "undefined") init();
