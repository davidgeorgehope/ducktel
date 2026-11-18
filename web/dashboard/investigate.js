// ducktel dashboard: the Investigate tab.
//
// One shared context (tenant, service, filters, window) drives every panel,
// from the time charts down to the compare rows that explain the slow group.
// The URL hash is the same context in the same parameter names the MCP tools
// use, so an agent can hand a person a link and the person can hand it back.

import {
  state as dashState,
  api,
  ticket,
  current,
  setHash,
  toast,
  escapeHtml,
  fmtTime,
  fmtMs,
  metricParams,
  slowParams,
  slowTable,
  logLineMarkup,
  renderDistributionInto,
  renderWaterfall,
} from "./app.js";

const PRESET_MINUTES = [60, 360, 1440, 10080];
const STEP_MINUTES = [1, 5, 15, 60];
const LATENCY_METRIC_NAMES = ["http.server.request.duration", "http.server.duration"];
const ERROR_COLUMNS = ["attr.http.response.status_code", "attr.http.status_code"];
// REQUEST_ROWS is how many requests show before "show all".
const REQUEST_ROWS = 10;
const CHART = { w: 320, h: 82, left: 46, right: 8, top: 8, bottom: 8 };

const inv = {
  ready: false,
  inited: false,
  tenant: "",
  serviceName: "",
  spanName: "",
  spanKind: "",
  metricName: "",
  metricManual: false,
  filters: [],
  from: "",
  to: "",
  minMs: null,
  maxMs: null,
  traceId: "",
  limit: null,
  presetMinutes: 60,
  metricNames: [],
  metricNamesTenant: null,
  servicesTenant: null,
  metricInfo: null,
  noLatency: false,
  noLatencyReason: "",
};

// --- pure URL and number logic ---

function timeMs(v) {
  if (v instanceof Date) return v.getTime();
  if (typeof v === "number") return v;
  if (!v) return NaN;
  return Date.parse(v);
}

function parseIntParam(v, def) {
  if (v === null || v === undefined || v === "") return def;
  const n = Number.parseInt(v, 10);
  return Number.isFinite(n) ? n : def;
}

function iso(v) {
  const n = timeMs(v);
  return Number.isFinite(n) ? new Date(n).toISOString() : "";
}

// parseFilter keeps a colon inside the value: only the first one separates
// the key from the value, matching the API's own split.
export function parseFilter(raw, op) {
  const s = String(raw === null || raw === undefined ? "" : raw);
  const i = s.indexOf(":");
  return {
    key: i === -1 ? s : s.slice(0, i),
    value: i === -1 ? "" : s.slice(i + 1),
    op: op === "!=" ? "!=" : "",
  };
}

// matchingPreset returns the preset whose length matches a window, if any,
// so "reset" knows where to go after a drag.
export function matchingPreset(from, to) {
  const a = timeMs(from);
  const b = timeMs(to);
  if (!Number.isFinite(a) || !Number.isFinite(b) || b <= a) return null;
  const minutes = Math.round((b - a) / 60000);
  return PRESET_MINUTES.includes(minutes) ? minutes : null;
}

// resolveWindow turns the accepted time forms into absolute ISO 8601 UTC.
// Absolute from/to win; otherwise since_minutes/until_minutes are resolved
// against now. With neither, the default is the last hour.
export function resolveWindow(params, now) {
  const nowMs = Number.isFinite(timeMs(now)) ? timeMs(now) : Date.now();
  let fromMs = timeMs(params.get("from"));
  let toMs = timeMs(params.get("to"));
  if (Number.isFinite(fromMs) && !Number.isFinite(toMs)) toMs = nowMs;
  if (!Number.isFinite(fromMs) && Number.isFinite(toMs)) fromMs = toMs - 60 * 60000;
  if (Number.isFinite(fromMs) && Number.isFinite(toMs) && toMs > fromMs) {
    return { from: new Date(fromMs).toISOString(), to: new Date(toMs).toISOString() };
  }

  let since = parseIntParam(params.get("since_minutes"), 60);
  let until = parseIntParam(params.get("until_minutes"), 0);
  if (since <= 0) since = 60;
  if (until < 0) until = 0;
  // The API rejects an end at or before the start; a one-minute floor keeps
  // a rounded link usable instead of a 400.
  if (until >= since) since = until + 1;
  return {
    from: new Date(nowMs - since * 60000).toISOString(),
    to: new Date(nowMs - until * 60000).toISOString(),
  };
}

// stateFromParams is the hash to screen direction. It accepts the MCP link
// forms (since_minutes/until_minutes) as well as the canonical from/to.
export function stateFromParams(params, now) {
  const win = resolveWindow(params, now);
  const filters = [];
  for (const pair of [["filter", ""], ["filter_not", "!="]]) {
    for (const raw of params.getAll(pair[0])) {
      const f = parseFilter(raw, pair[1]);
      if (f.key) filters.push(f);
    }
  }
  const numberOrNull = (v) => {
    if (v === null || v === undefined || v === "") return null;
    const n = Number(v);
    return Number.isFinite(n) ? n : null;
  };
  const limit = parseIntParam(params.get("limit"), null);
  return {
    tenant: params.get("tenant") || "",
    serviceName: params.get("service_name") || "",
    spanName: params.get("span_name") || "",
    spanKind: params.get("span_kind") || "",
    metricName: params.get("metric_name") || "",
    filters,
    from: win.from,
    to: win.to,
    minMs: numberOrNull(params.get("min_ms")),
    maxMs: numberOrNull(params.get("max_ms")),
    traceId: params.get("trace_id") || "",
    limit: limit !== null && limit > 0 ? limit : null,
    presetMinutes: matchingPreset(win.from, win.to) || 60,
  };
}

// paramsFromState is the screen to hash direction, always absolute from/to.
export function paramsFromState(s) {
  const p = new URLSearchParams();
  if (s.tenant) p.set("tenant", s.tenant);
  if (s.serviceName) p.set("service_name", s.serviceName);
  if (s.spanName) p.set("span_name", s.spanName);
  if (s.spanKind) p.set("span_kind", s.spanKind);
  for (const f of s.filters || []) {
    p.append(f.op === "!=" ? "filter_not" : "filter", f.key + ":" + f.value);
  }
  if (s.metricName) p.set("metric_name", s.metricName);
  if (s.from) p.set("from", iso(s.from));
  if (s.to) p.set("to", iso(s.to));
  if (s.minMs !== null && s.minMs !== undefined && Number.isFinite(Number(s.minMs))) p.set("min_ms", String(s.minMs));
  if (s.maxMs !== null && s.maxMs !== undefined && Number.isFinite(Number(s.maxMs))) p.set("max_ms", String(s.maxMs));
  if (s.traceId) p.set("trace_id", s.traceId);
  if (s.limit) p.set("limit", String(s.limit));
  return p;
}

// toApiWindow is the only place relative minutes are derived. Ceil the start
// and floor the end so the API window never silently includes less than the
// screen shows; until stays at or above zero.
export function toApiWindow(from, to, now) {
  const nowMs = Number.isFinite(timeMs(now)) ? timeMs(now) : Date.now();
  let fromMs = timeMs(from);
  let toMs = timeMs(to);
  if (!Number.isFinite(fromMs) || !Number.isFinite(toMs) || toMs <= fromMs) {
    fromMs = nowMs - 60 * 60000;
    toMs = nowMs;
  }
  let since = Math.ceil((nowMs - fromMs) / 60000);
  let until = Math.floor((nowMs - toMs) / 60000);
  if (!Number.isFinite(until) || until < 0) until = 0;
  if (!Number.isFinite(since) || since < 1) since = 1;
  if (until >= since) since = until + 1;
  return { sinceMinutes: since, untilMinutes: until };
}

// chooseStep picks the smallest useful step near 120 points across the
// window, from the four sizes the dashboard already talks in.
export function chooseStep(from, to) {
  const a = timeMs(from);
  const b = timeMs(to);
  if (!Number.isFinite(a) || !Number.isFinite(b) || b <= a) return "1m";
  const target = (b - a) / 60000 / 120;
  for (const m of STEP_MINUTES) if (target <= m) return m + "m";
  return "1h";
}

const MS_PER_UNIT = { ms: 1, s: 1000, us: 0.001, ns: 0.000001 };

export function unitToMs(unit) {
  const u = String(unit || "").trim();
  if (u === "\u00b5s" || u === "\u03bcs") return 0.001;
  return Object.prototype.hasOwnProperty.call(MS_PER_UNIT, u) ? MS_PER_UNIT[u] : null;
}

export function toMs(value, unit) {
  const f = unitToMs(unit);
  if (f === null || value === null || value === undefined) return null;
  const n = Number(value);
  return Number.isFinite(n) ? n * f : null;
}

export function fromMs(ms, unit) {
  const f = unitToMs(unit);
  if (f === null || ms === null || ms === undefined) return null;
  const n = Number(ms);
  return Number.isFinite(n) ? n / f : null;
}

// sumServerErrors adds the counts of 5xx status codes from a count grouped by
// a status-code attribute. Codes may arrive as strings from JSON.
export function sumServerErrors(rows, column) {
  let total = 0;
  for (const r of rows || []) {
    const code = Number(r[column]);
    if (Number.isFinite(code) && code >= 500) total += Number(r.value) || 0;
  }
  return total;
}

// pickErrorColumn is the fallback rule for the status-code group_by: use the
// first column that actually produced rows.
export function pickErrorColumn(rows, columns) {
  const cs = columns || ERROR_COLUMNS;
  for (const c of cs) if ((rows || []).some((r) => r[c] !== null && r[c] !== undefined)) return c;
  return cs[cs.length - 1];
}

// --- small display helpers ---

function localTime(v) {
  const n = timeMs(v);
  return Number.isFinite(n) ? new Date(n).toLocaleString() : "";
}

function fmtNum(v) {
  const n = Number(v);
  if (!Number.isFinite(n)) return "0";
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}

function fmtRange() {
  if (inv.minMs === null && inv.maxMs === null) return "slowest";
  if (inv.minMs === null) return "< " + fmtMs(inv.maxMs);
  if (inv.maxMs === null) return "> " + fmtMs(inv.minMs);
  return fmtMs(inv.minMs) + " - " + fmtMs(inv.maxMs);
}

function joinFilter(f) {
  return f.key + (f.op === "!=" ? "!=" : "=") + f.value;
}

function appendFilters(p, filters) {
  for (const f of filters || []) p.append(f.op === "!=" ? "filter_not" : "filter", f.key + ":" + f.value);
}

function contextFilters() {
  return inv.filters.slice();
}

function metricFilters() {
  const out = contextFilters();
  if (inv.serviceName) out.push({ key: "service.name", value: inv.serviceName, op: "" });
  return out;
}

function syncHash() {
  setHash("investigate", paramsFromState(inv));
}

function setPanelHTML(el, html) {
  el.innerHTML = html;
}

function panelError(el, msg) {
  setPanelHTML(el, `<div class="empty inv-error">${escapeHtml(msg)}</div>`);
}

function selectTrace(traceId) {
  if (!traceId) return;
  inv.traceId = traceId;
  syncHash();
  renderTracePanel();
  renderLogsPanel();
}

function addContextFilter(key, value, op) {
  const next = inv.filters.filter((f) => !(f.key === key && f.op === op));
  next.push({ key, value, op: op === "!=" ? "!=" : "" });
  inv.filters = next;
}

// The compare result talks in stored column and resource-attribute names;
// a filter matches the bare attribute key on either scope.
function filterKeyForRow(key) {
  if (key.startsWith("resource.")) return key.slice("resource.".length);
  if (key.startsWith("attributes.")) return key.slice("attributes.".length);
  return key;
}

// --- metric choice and metadata ---

function updateMetricSelect() {
  const sel = document.getElementById("inv-metric");
  if (!sel) return;
  const names = inv.metricNames.slice();
  if (inv.metricName && !names.includes(inv.metricName)) names.unshift(inv.metricName);
  sel.innerHTML = `<option value="">none</option>` + names
    .map((n) => `<option value="${escapeHtml(n)}">${escapeHtml(n)}</option>`)
    .join("");
  sel.value = inv.metricName || "";
}

async function loadMetricNames() {
  if (inv.metricNamesTenant === inv.tenant) {
    updateMetricSelect();
    return;
  }
  const resp = await api("/api/metric-names?tenant=" + encodeURIComponent(inv.tenant));
  inv.metricNames = resp.rows.map((r) => r.metric_name).filter(Boolean);
  inv.metricNamesTenant = inv.tenant;
  updateMetricSelect();
}

async function loadServiceOptions() {
  const sel = document.getElementById("inv-service");
  if (!sel) return;
  if (inv.servicesTenant !== inv.tenant) {
    const resp = await api("/api/services?tenant=" + encodeURIComponent(inv.tenant));
    inv.services = resp.rows.map((r) => r.service_name).filter(Boolean);
    inv.servicesTenant = inv.tenant;
  }
  const names = (inv.services || []).slice();
  if (inv.serviceName && !names.includes(inv.serviceName)) names.unshift(inv.serviceName);
  sel.innerHTML = `<option value="">any</option>` + names
    .map((s) => `<option value="${escapeHtml(s)}">${escapeHtml(s)}</option>`)
    .join("");
  sel.value = inv.serviceName || "";
}

async function ensureMetricName() {
  try {
    await loadMetricNames();
  } catch (e) {
    inv.noLatency = true;
    inv.noLatencyReason = "could not list metric names: " + e.message;
    return;
  }
  if (!inv.metricName && !inv.metricManual) {
    inv.metricName = LATENCY_METRIC_NAMES.find((n) => inv.metricNames.includes(n)) || "";
  }
  updateMetricSelect();
}

async function prepareMetric() {
  const t = ticket("inv-prepare");
  const win = toApiWindow(inv.from, inv.to, new Date());
  inv.metricInfo = null;
  inv.noLatency = false;
  inv.noLatencyReason = "";
  if (!inv.metricName) {
    inv.noLatency = true;
    inv.noLatencyReason = "no latency metric for this service";
    return;
  }
  const filters = inv.serviceName ? [{ key: "service.name", value: inv.serviceName, op: "" }] : [];
  const base = new URLSearchParams({
    tenant: inv.tenant,
    metric_name: inv.metricName,
    since_minutes: String(win.sinceMinutes),
    until_minutes: String(win.untilMinutes),
  });
  const p = metricParams(base, "count", filters, { group_by: "metric_type,metric_unit" });
  let resp;
  try {
    resp = await api("/api/metrics?" + p.toString());
  } catch (e) {
    if (!current("inv-prepare", t)) return;
    inv.noLatency = true;
    inv.noLatencyReason = "latency metric lookup failed: " + e.message;
    return;
  }
  if (!current("inv-prepare", t)) return;
  const row = (resp && resp.rows && resp.rows[0]) || null;
  if (!row) {
    inv.noLatency = true;
    inv.noLatencyReason = "no latency metric for this service";
    return;
  }
  inv.metricInfo = { type: row.metric_type || "", unit: row.metric_unit || "" };
  if (inv.metricInfo.type !== "histogram") {
    inv.noLatency = true;
    inv.noLatencyReason = "no latency histogram for this service";
  }
}

function renderNoLatencyState() {
  const notice = document.getElementById("inv-no-latency");
  const time = document.getElementById("inv-panel-time");
  const dist = document.getElementById("inv-panel-dist");
  if (!notice || !time || !dist) return;
  notice.classList.toggle("hidden", !inv.noLatency);
  time.classList.toggle("hidden", inv.noLatency);
  dist.classList.toggle("hidden", inv.noLatency);
  if (inv.noLatency) notice.textContent = inv.noLatencyReason || "no latency metric for this service";
}

// --- panel 0: time ---

// apiTimeMs converts a step row's time column (Unix microseconds, as
// metric_query returns it) to the milliseconds Date and the chart use.
export function apiTimeMs(v) {
  return Number(v) / 1000;
}

function rowsSeries(rows, valueFn) {
  const out = [];
  for (const r of rows || []) {
    const t = apiTimeMs(r.time);
    const v = valueFn(r);
    if (Number.isFinite(t) && Number.isFinite(v)) out.push({ t, v });
  }
  out.sort((a, b) => a.t - b.t);
  return out;
}

function errorSeries(rows, column) {
  const byTime = new Map();
  for (const r of rows || []) {
    const t = apiTimeMs(r.time);
    const code = Number(r[column]);
    if (!Number.isFinite(t) || !Number.isFinite(code) || code < 500) continue;
    byTime.set(t, (byTime.get(t) || 0) + (Number(r.value) || 0));
  }
  return [...byTime.entries()]
    .map(([t, v]) => ({ t, v }))
    .sort((a, b) => a.t - b.t);
}

// splitAtGaps cuts a series where points are missing: a step with no data
// is a gap, and joining across it would draw hours of silence as a steady
// line. The step is the smallest spacing between points; a jump over more
// than 1.5 steps starts a new segment.
export function splitAtGaps(series) {
  if (series.length < 2) return series.length ? [series] : [];
  let step = Infinity;
  for (let i = 1; i < series.length; i++) step = Math.min(step, series[i].t - series[i - 1].t);
  const out = [[series[0]]];
  for (let i = 1; i < series.length; i++) {
    if (series[i].t - series[i - 1].t > step * 1.5) out.push([]);
    out[out.length - 1].push(series[i]);
  }
  return out;
}

function chartSVG(series, stroke) {
  const { w, h, left, right, top, bottom } = CHART;
  const fromMs = timeMs(inv.from);
  const toMs = timeMs(inv.to);
  const span = Math.max(toMs - fromMs, 1);
  const max = Math.max(1, ...series.map((p) => p.v));
  const x = (t) => left + ((t - fromMs) / span) * (w - left - right);
  const y = (v) => h - bottom - (Math.max(0, Math.min(v, max)) / max) * (h - top - bottom);
  // A lone point between gaps has no line to draw, so it gets a dot.
  const lines = splitAtGaps(series).map((seg) => seg.length === 1
    ? `<circle cx="${x(seg[0].t).toFixed(1)}" cy="${y(seg[0].v).toFixed(1)}" r="1.6" fill="${stroke}"></circle>`
    : `<polyline points="${seg.map((p) => `${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(" ")}" fill="none" stroke="${stroke}" stroke-width="1.6" class="inv-line"></polyline>`,
  ).join("");
  return `<svg class="inv-chart" viewBox="0 0 ${w} ${h}" data-inv-chart="1" aria-label="time series">
    <line x1="${left}" y1="${h - bottom}" x2="${w - right}" y2="${h - bottom}" class="inv-axis-line"></line>
    <text x="4" y="${top + 8}" class="inv-axis-text">${escapeHtml(fmtNum(max))}</text>
    <text x="4" y="${h - bottom}" class="inv-axis-text">0</text>
    ${lines}
    <rect class="inv-sel-rect" x="0" y="${top}" width="0" height="${h - top - bottom}"></rect>
  </svg>`;
}

function chartFraction(svg, ev) {
  const rect = svg.getBoundingClientRect();
  const f = rect.width > 0 ? (ev.clientX - rect.left) / rect.width : 0;
  const left = CHART.left / CHART.w;
  const right = CHART.right / CHART.w;
  return Math.max(0, Math.min(1, (f - left) / (1 - left - right)));
}

function paintSelection(a, b) {
  const { w, left, right } = CHART;
  const x1 = left + Math.min(a, b) * (w - left - right);
  const x2 = left + Math.max(a, b) * (w - left - right);
  document.querySelectorAll("#inv-charts .inv-sel-rect").forEach((r) => {
    r.setAttribute("x", x1.toFixed(1));
    r.setAttribute("width", Math.max(x2 - x1, 0).toFixed(1));
  });
}

function commitRange(a, b) {
  if (Math.abs(b - a) < 0.01) return;
  const fromMs = timeMs(inv.from);
  const toMs = timeMs(inv.to);
  const span = toMs - fromMs;
  const lo = Math.min(a, b);
  const hi = Math.max(a, b);
  inv.from = new Date(fromMs + lo * span).toISOString();
  inv.to = new Date(fromMs + hi * span).toISOString();
  syncHash();
  refreshInvestigate();
}

function bindChartDrag() {
  document.querySelectorAll("#inv-charts svg[data-inv-chart]").forEach((svg) => {
    svg.addEventListener("pointerdown", (ev) => {
      ev.preventDefault();
      const start = chartFraction(svg, ev);
      const move = (e) => paintSelection(start, chartFraction(svg, e));
      const up = (e) => {
        svg.removeEventListener("pointermove", move);
        svg.removeEventListener("pointerup", up);
        svg.removeEventListener("pointercancel", up);
        const end = chartFraction(svg, e);
        paintSelection(0, 0);
        if (Math.abs(end - start) >= 0.01) commitRange(start, end);
      };
      svg.addEventListener("pointermove", move);
      svg.addEventListener("pointerup", up);
      svg.addEventListener("pointercancel", up);
    });
  });
}

function renderTimePanel() {
  const panel = document.getElementById("inv-panel-time");
  const charts = document.getElementById("inv-charts");
  const reset = document.getElementById("inv-time-reset");
  if (!panel || !charts) return;
  if (inv.noLatency) {
    panel.classList.add("hidden");
    return;
  }
  panel.classList.remove("hidden");
  charts.innerHTML = `<div class="empty">loading...</div>`;
  if (reset) {
    const now = Date.now();
    const from = now - (inv.presetMinutes || 60) * 60000;
    const differs = Math.abs(timeMs(inv.from) - from) > 90000 || Math.abs(timeMs(inv.to) - now) > 90000;
    reset.classList.toggle("hidden", !differs);
  }

  const t = ticket("inv-time");
  const win = toApiWindow(inv.from, inv.to, new Date());
  const step = chooseStep(inv.from, inv.to);
  const filters = metricFilters();
  const base = new URLSearchParams({
    tenant: inv.tenant,
    metric_name: inv.metricName,
    since_minutes: String(win.sinceMinutes),
    until_minutes: String(win.untilMinutes),
  });
  const ask = (agg, extra) => api("/api/metrics?" + metricParams(base, agg, filters, Object.assign({ step }, extra || {})).toString());

  return (async () => {
    let req, lat, errs;
    try {
      [req, lat, errs] = await Promise.all([
        ask("count"),
        ask("p95"),
        ask("count", { group_by: ERROR_COLUMNS[0] }),
      ]);
    } catch (e) {
      if (current("inv-time", t)) panelError(charts, e.message);
      return;
    }
    if (!current("inv-time", t)) return;
    let errCol = ERROR_COLUMNS[0];
    if (!errs.rows.length) {
      try {
        errs = await ask("count", { group_by: ERROR_COLUMNS[1] });
        errCol = ERROR_COLUMNS[1];
      } catch (e) {
        if (current("inv-time", t)) panelError(charts, e.message);
        return;
      }
      if (!current("inv-time", t)) return;
    }

    const reqSeries = rowsSeries(req.rows, (r) => Number(r.value));
    const latSeries = rowsSeries(lat.rows, (r) => Number(r.value));
    const errSeries = errorSeries(errs.rows, errCol);
    if (!reqSeries.length && !latSeries.length) {
      charts.innerHTML = `<div class="empty">no points in this window</div>`;
      return;
    }
    const box = (title, series, color) => `<div class="inv-chart-box">
      <div class="inv-chart-title">${escapeHtml(title)}</div>
      ${chartSVG(series, color)}
    </div>`;
    const mid = new Date((timeMs(inv.from) + timeMs(inv.to)) / 2).toISOString();
    charts.innerHTML = `<div class="inv-charts-grid">
        ${box("requests", reqSeries, "#f2c14e")}
        ${box("errors (5xx)", errSeries, "#f85149")}
        ${box("latency p95", latSeries, "#6aa6f8")}
      </div>
      <div class="inv-axis-labels">
        <span>${escapeHtml(localTime(inv.from))}</span>
        <span>${escapeHtml(localTime(mid))}</span>
        <span>${escapeHtml(localTime(inv.to))}</span>
      </div>`;
    bindChartDrag();
  })();
}

// --- panel 1: distribution ---

function boundMatches(ms, raw, unit) {
  if (ms === null || ms === undefined) return raw === null || raw === undefined;
  if (raw === null || raw === undefined) return false;
  const v = toMs(raw, unit);
  return v !== null && Math.abs(v - ms) <= Math.max(0.001, Math.abs(ms) * 1e-6);
}

function selectBucket(lo, le, unit) {
  const min = lo === null || lo === undefined ? null : toMs(lo, unit);
  const max = le === null || le === undefined ? null : toMs(le, unit);
  if (min === null && max === null) return;
  if ((min !== null && !Number.isFinite(min)) || (max !== null && !Number.isFinite(max))) {
    toast("cannot convert metric unit " + (unit || "(none)") + " to ms", true);
    return;
  }
  inv.minMs = min === null ? null : Math.round(min * 1000) / 1000;
  inv.maxMs = max === null ? null : Math.round(max * 1000) / 1000;
  syncHash();
  renderDistributionPanel();
  renderRequestsPanel();
  renderWhyPanel();
}

function renderDistributionPanel() {
  const panel = document.getElementById("inv-panel-dist");
  const body = document.getElementById("inv-dist-body");
  if (!panel || !body) return;
  if (inv.noLatency) {
    panel.classList.add("hidden");
    return;
  }
  panel.classList.remove("hidden");
  body.innerHTML = `<div class="empty">loading...</div>`;
  const win = toApiWindow(inv.from, inv.to, new Date());
  const unit = (inv.metricInfo && inv.metricInfo.unit) || "";
  const base = new URLSearchParams({
    tenant: inv.tenant,
    metric_name: inv.metricName,
    since_minutes: String(win.sinceMinutes),
    until_minutes: String(win.untilMinutes),
  });
  const label = inv.spanName || inv.serviceName || "all";
  return renderDistributionInto(body, label, metricFilters(), base, {
    ticketKey: "inv-dist",
    unit,
    onBucket: (lo, le, u) => selectBucket(lo, le, u || unit),
    selectedFn: (lo, le) => boundMatches(inv.minMs, lo, unit) && boundMatches(inv.maxMs, le, unit),
    onError: (msg) => panelError(body, msg),
  });
}

// --- panel 2: requests ---

function requestSlowParams(win) {
  const base = new URLSearchParams({
    tenant: inv.tenant,
    since_minutes: String(win.sinceMinutes),
    until_minutes: String(win.untilMinutes),
  });
  const extra = { limit: "20" };
  if (inv.serviceName) extra.service_name = inv.serviceName;
  if (inv.noLatency && inv.spanName) extra.span_name = inv.spanName;
  if (inv.noLatency) {
    base.delete("metric_name");
    if (inv.minMs !== null) extra.min_value = inv.minMs;
    if (inv.maxMs !== null) extra.max_value = inv.maxMs;
  } else {
    base.set("metric_name", inv.metricName);
    const unit = (inv.metricInfo && inv.metricInfo.unit) || "";
    if (inv.minMs !== null) extra.min_value = fromMs(inv.minMs, unit);
    if (inv.maxMs !== null) extra.max_value = fromMs(inv.maxMs, unit);
  }
  return slowParams(base, contextFilters(), extra);
}

function renderRequestsPanel() {
  const body = document.getElementById("inv-requests-body");
  const head = document.getElementById("inv-requests-head");
  if (!body || !head) return;
  const t = ticket("inv-requests");
  body.innerHTML = `<div class="empty">loading...</div>`;
  head.innerHTML = `<b>requests</b> · ${escapeHtml(fmtRange())}`;
  const win = toApiWindow(inv.from, inv.to, new Date());
  const params = requestSlowParams(win);

  return (async () => {
    let rows;
    try {
      rows = await api("/api/slow?" + params.toString());
    } catch (e) {
      if (current("inv-requests", t)) panelError(body, e.message);
      return;
    }
    if (!current("inv-requests", t)) return;
    rows = Array.isArray(rows) ? rows : [];
    head.innerHTML = `<b>requests</b> · ${escapeHtml(fmtRange())} · ${rows.length} row${rows.length === 1 ? "" : "s"}` +
      (inv.minMs !== null || inv.maxMs !== null
        ? ` · <button type="button" class="secondary inv-link-btn" data-all-spans="1">all spans in this range</button>`
        : "");
    // The first rows answer the question; the rest stay one click away so
    // the table does not push the trace and logs off the screen.
    const shown = rows.length > REQUEST_ROWS ? rows.slice(0, REQUEST_ROWS) : rows;
    body.innerHTML = "";
    body.appendChild(slowTable(shown, String(win.sinceMinutes), { onTraceClick: selectTrace }));
    if (shown.length < rows.length) {
      const more = document.createElement("button");
      more.type = "button";
      more.className = "secondary inv-link-btn inv-more";
      more.textContent = `show all ${rows.length}`;
      more.addEventListener("click", () => {
        body.innerHTML = "";
        body.appendChild(slowTable(rows, String(win.sinceMinutes), { onTraceClick: selectTrace }));
      });
      body.appendChild(more);
    }
    const link = head.querySelector("button[data-all-spans]");
    if (link) link.addEventListener("click", () => renderAllSpans(win));
  })();
}

async function renderAllSpans(win) {
  const body = document.getElementById("inv-requests-body");
  const head = document.getElementById("inv-requests-head");
  if (!body || !head) return;
  const t = ticket("inv-requests");
  body.innerHTML = `<div class="empty">loading...</div>`;
  const p = new URLSearchParams({
    tenant: inv.tenant,
    since_minutes: String(win.sinceMinutes),
    until_minutes: String(win.untilMinutes),
    limit: "100",
  });
  if (inv.serviceName) p.set("service_name", inv.serviceName);
  if (inv.minMs !== null) p.set("min_ms", String(inv.minMs));
  if (inv.maxMs !== null) p.set("max_ms", String(inv.maxMs));
  appendFilters(p, contextFilters());
  let resp;
  try {
    resp = await api("/api/traces?" + p.toString());
  } catch (e) {
    if (current("inv-requests", t)) panelError(body, e.message);
    return;
  }
  if (!current("inv-requests", t)) return;
  let rows = resp.rows || [];
  // /api/traces has no span_name parameter; narrow the small result here so
  // the context's span name still means something.
  if (inv.spanName) rows = rows.filter((r) => r.span_name === inv.spanName);
  head.innerHTML = `<b>all spans</b> · ${escapeHtml(fmtRange())} · ${rows.length} span${rows.length === 1 ? "" : "s"}`;
  if (!rows.length) {
    body.innerHTML = `<div class="empty">no spans in this range</div>`;
    return;
  }
  const table = document.createElement("table");
  table.className = "data-table";
  table.innerHTML = `<thead><tr><th>Start</th><th>Service</th><th>Span</th><th>Duration</th><th>Status</th><th>Trace</th></tr></thead><tbody></tbody>`;
  const tbody = table.querySelector("tbody");
  rows.forEach((r) => {
    const tr = document.createElement("tr");
    tr.className = "clickable";
    tr.innerHTML = `<td>${escapeHtml(fmtTime(r.start_time))}</td>
      <td>${escapeHtml(r.service_name || "")}</td>
      <td>${escapeHtml(r.span_name || "")}</td>
      <td>${escapeHtml(fmtMs(r.duration_ms))}</td>
      <td>${escapeHtml((r.status_code || "").replace("STATUS_CODE_", ""))}</td>
      <td class="mono">${escapeHtml(String(r.trace_id || "").slice(0, 12))}</td>`;
    tr.addEventListener("click", () => selectTrace(r.trace_id));
    tbody.appendChild(tr);
  });
  body.innerHTML = "";
  body.appendChild(table);
}

// --- panel 3: trace ---

function openInTracesHref() {
  const win = toApiWindow(inv.from, inv.to, new Date());
  const p = new URLSearchParams();
  p.set("tenant", inv.tenant);
  p.set("since_minutes", String(win.sinceMinutes));
  p.set("trace_id", inv.traceId);
  return "#traces?" + p.toString();
}

function renderTracePanel() {
  const body = document.getElementById("inv-trace-body");
  const head = document.getElementById("inv-trace-head");
  if (!body || !head) return;
  head.innerHTML = "";
  if (!inv.traceId) {
    body.innerHTML = `<div class="empty">no trace selected - click a request above</div>`;
    return;
  }
  head.innerHTML = `<span class="mono">${escapeHtml(inv.traceId)}</span>
    · <a href="${escapeHtml(openInTracesHref())}">Open in Traces</a>`;
  body.innerHTML = `<div class="empty">loading...</div>`;
  const t = ticket("inv-trace");
  const tenant = encodeURIComponent(inv.tenant);
  const id = encodeURIComponent(inv.traceId);
  const spansP = api("/api/traces/" + id + "?tenant=" + tenant);
  const logsP = api("/api/logs?tenant=" + tenant + "&trace_id=" + id + "&limit=1000")
    .then((r) => r.rows || [])
    .catch(() => []);
  const summaryP = api("/api/traces/" + id + "/summary?tenant=" + tenant).catch(() => null);

  return (async () => {
    let spans;
    try {
      spans = await spansP;
    } catch (e) {
      if (current("inv-trace", t)) panelError(body, e.message);
      return;
    }
    const [logs, summary] = await Promise.all([logsP, summaryP]);
    if (!current("inv-trace", t)) return;
    if (!spans.rows || !spans.rows.length) {
      panelError(body, "no spans for this trace - it may be outside retention");
      return;
    }
    body.innerHTML = `<div class="waterfall"></div>`;
    renderWaterfall(body.firstElementChild, inv.traceId, spans.rows, logs, summary);
  })();
}

// --- panel 4: logs of that trace ---

function renderLogsPanel() {
  const body = document.getElementById("inv-logs-body");
  if (!body) return;
  if (!inv.traceId) {
    body.innerHTML = `<div class="empty">no trace selected</div>`;
    return;
  }
  const t = ticket("inv-logs");
  body.innerHTML = `<div class="empty">loading...</div>`;
  const p = new URLSearchParams({
    tenant: inv.tenant,
    trace_id: inv.traceId,
    limit: "1000",
  });
  return (async () => {
    let resp;
    try {
      resp = await api("/api/logs?" + p.toString());
    } catch (e) {
      if (current("inv-logs", t)) panelError(body, e.message);
      return;
    }
    if (!current("inv-logs", t)) return;
    const rows = resp.rows || [];
    if (!rows.length) {
      body.innerHTML = `<div class="empty">no logs carry this trace id</div>`;
      return;
    }
    body.innerHTML = `<div class="wf-logs">${rows.map(logLineMarkup).join("")}</div>`;
  })();
}

// --- panel 5: why ---

function signed(v) {
  const n = Number(v) || 0;
  return (n > 0 ? "+" : "") + n.toFixed(1);
}

function pctWidth(v) {
  const n = Number(v) || 0;
  return Math.max(0, Math.min(100, n)).toFixed(1) + "%";
}

function thresholdText(cmp) {
  if (cmp.threshold_ms !== null && cmp.threshold_ms !== undefined) return fmtMs(cmp.threshold_ms) + " (p95)";
  if (cmp.min_ms !== null && cmp.min_ms !== undefined && cmp.max_ms !== null && cmp.max_ms !== undefined) {
    return fmtMs(cmp.min_ms) + " - " + fmtMs(cmp.max_ms);
  }
  if (cmp.min_ms !== null && cmp.min_ms !== undefined) return ">= " + fmtMs(cmp.min_ms);
  if (cmp.max_ms !== null && cmp.max_ms !== undefined) return "<= " + fmtMs(cmp.max_ms);
  return "p95";
}

// whyActions says which row buttons make sense. span_name is a column, not
// an attribute: focus narrows the context's span name and there is no way to
// exclude it. status_code and span_kind are columns no filter can reach.
export function whyActions(key) {
  if (key === "span_name") return { focus: true, exclude: false };
  if (key === "status_code" || key === "span_kind") return { focus: false, exclude: false };
  return { focus: true, exclude: true };
}

function whyRowMarkup(row, i) {
  const also = Array.isArray(row.also) ? row.also : [];
  const act = whyActions(row.key);
  const kv = `${row.key}=${row.value}`;
  // One line per value: the full text and the other forms of the same fact
  // live in the tooltip, so long values (user agents, routes) cannot push the
  // bars onto extra lines.
  const tip = also.length ? `${kv}\nalso: ${also.join(", ")}` : kv;
  const diffClass = Number(row.diff) >= 0 ? "inv-why-diff" : "inv-why-diff neg";
  return `<div class="inv-why-row">
    <span class="inv-why-kv mono" title="${escapeHtml(tip)}">${escapeHtml(kv)}${also.length ? ` <span class="inv-why-also">+${also.length}</span>` : ""}</span>
    <span class="inv-why-bar" title="slow ${escapeHtml(fmtNum(row.slow_pct))}%"><span class="inv-why-track"><span class="inv-why-fill" style="width:${pctWidth(row.slow_pct)}"></span></span><span class="inv-why-pct">${escapeHtml(fmtNum(row.slow_pct))}%</span></span>
    <span class="inv-why-bar" title="rest ${escapeHtml(fmtNum(row.rest_pct))}%"><span class="inv-why-track"><span class="inv-why-fill rest" style="width:${pctWidth(row.rest_pct)}"></span></span><span class="inv-why-pct">${escapeHtml(fmtNum(row.rest_pct))}%</span></span>
    <span class="${diffClass}">${escapeHtml(signed(row.diff))}</span>
    <span class="inv-why-actions">
      ${act.focus ? `<button type="button" class="secondary" data-why-act="focus" data-why-i="${i}" title="only spans with this value">focus</button>` : ""}
      ${act.exclude ? `<button type="button" class="secondary" data-why-act="exclude" data-why-i="${i}" title="drop spans with this value">exclude</button>` : ""}
    </span>
  </div>`;
}

function renderWhyPanel() {
  const body = document.getElementById("inv-why-body");
  if (!body) return;
  const t = ticket("inv-why");
  body.innerHTML = `<div class="empty">loading...</div>`;
  const win = toApiWindow(inv.from, inv.to, new Date());
  const p = new URLSearchParams({
    tenant: inv.tenant,
    since_minutes: String(win.sinceMinutes),
    until_minutes: String(win.untilMinutes),
  });
  if (inv.serviceName) p.set("service_name", inv.serviceName);
  if (inv.spanName) p.set("span_name", inv.spanName);
  if (inv.spanKind) p.set("span_kind", inv.spanKind);
  if (inv.limit) p.set("limit", String(inv.limit));
  if (inv.minMs !== null) p.set("min_ms", String(inv.minMs));
  if (inv.maxMs !== null) p.set("max_ms", String(inv.maxMs));
  appendFilters(p, contextFilters());

  return (async () => {
    let cmp;
    try {
      cmp = await api("/api/compare?" + p.toString());
    } catch (e) {
      if (current("inv-why", t)) panelError(body, e.message);
      return;
    }
    if (!current("inv-why", t)) return;
    const rows = Array.isArray(cmp.rows) ? cmp.rows : [];
    const slowN = Number(cmp.slow_n) || 0;
    const restN = Number(cmp.rest_n) || 0;
    const bits = [
      `${slowN} slow`,
      `${restN} rest`,
      `threshold ${thresholdText(cmp)}`,
      `kind ${cmp.span_kind || "any"}`,
      `sampled ${cmp.sampled ? "yes" : "no"}`,
    ];
    let html = `<div class="dist-head"><b>slow vs rest</b> · ${escapeHtml(bits.join(" · "))}</div>`;
    html += `<div class="inv-why-legend"><span><i></i>share among slow</span><span><i class="rest"></i>share among the rest</span><span>diff = slow - rest, points</span></div>`;
    if (!slowN || !restN) {
      body.innerHTML = html + `<div class="empty">nothing to compare: one group is empty</div>`;
      return;
    }
    const top = rows.length ? Math.abs(Number(rows[0].diff) || 0) : 0;
    if (top < 5) html += `<div class="empty inv-standout">nothing stands out between slow and the rest</div>`;
    if (!rows.length) {
      body.innerHTML = html + `<div class="empty">no differing attribute values</div>`;
      return;
    }
    html += rows.map(whyRowMarkup).join("");
    body.innerHTML = html;
    body.querySelectorAll("button[data-why-act]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const row = rows[Number(btn.dataset.whyI)];
        if (!row) return;
        if (row.key === "span_name") {
          inv.spanName = row.value;
        } else {
          const op = btn.dataset.whyAct === "exclude" ? "!=" : "";
          addContextFilter(filterKeyForRow(row.key), row.value, op);
        }
        refreshInvestigate();
      });
    });
  })();
}

// --- coordination ---

export async function refreshInvestigate() {
  if (!inv.ready) return;
  const gen = ticket("inv-refresh");
  if (dashState.tenant && dashState.tenant !== inv.tenant) {
    inv.tenant = dashState.tenant;
    inv.metricNamesTenant = null;
    inv.servicesTenant = null;
    if (!inv.metricManual) inv.metricName = "";
    await ensureMetricName();
  }
  try {
    await loadServiceOptions();
  } catch (e) {
    toast(e.message, true);
  }
  syncContextInputs();
  await prepareMetric();
  if (!current("inv-refresh", gen)) return;
  syncHash();
  syncContextInputs();
  renderNoLatencyState();
  await Promise.all([
    renderTimePanel(),
    renderDistributionPanel(),
    renderRequestsPanel(),
    renderWhyPanel(),
    renderTracePanel(),
    renderLogsPanel(),
  ]);
}

function setWindowFromPreset(minutes) {
  const now = Date.now();
  inv.presetMinutes = minutes;
  inv.to = new Date(now).toISOString();
  inv.from = new Date(now - minutes * 60000).toISOString();
  syncHash();
  refreshInvestigate();
}

export async function applyInvestigateView(params) {
  Object.assign(inv, stateFromParams(params, new Date()), {
    ready: true,
    metricManual: params.has("metric_name"),
  });
  if (!inv.tenant) inv.tenant = dashState.tenant || "";
  await ensureMetricName();
  await refreshInvestigate();
}

export async function enterInvestigate() {
  if (!inv.ready) {
    const p = new URLSearchParams();
    if (dashState.tenant) p.set("tenant", dashState.tenant);
    await applyInvestigateView(p);
    return;
  }
  // The tab was put back on screen without a new link: keep the same context
  // and make sure its panels agree with the current tenant.
  await refreshInvestigate();
}

function renderFilterChips() {
  const el = document.getElementById("inv-filters");
  if (!el) return;
  el.innerHTML = inv.filters.map((f, i) =>
    `<span class="chip">${escapeHtml(joinFilter(f))} <button type="button" data-filter-i="${i}" title="remove">×</button></span>`
  ).join("");
}

function syncContextInputs() {
  const service = document.getElementById("inv-service");
  if (service) service.value = inv.serviceName || "";
  const span = document.getElementById("inv-span-name");
  if (span) span.value = inv.spanName || "";
  const metric = document.getElementById("inv-metric");
  if (metric) metric.value = inv.metricName || "";
  const range = document.getElementById("inv-range");
  if (range) range.textContent = localTime(inv.from) + " - " + localTime(inv.to);
  const presets = document.getElementById("inv-presets");
  if (presets) {
    const length = Math.round((timeMs(inv.to) - timeMs(inv.from)) / 60000);
    presets.querySelectorAll("button[data-minutes]").forEach((b) => {
      b.classList.toggle("active", Number(b.dataset.minutes) === length);
    });
  }
  renderFilterChips();
}

export function initInvestigate() {
  if (inv.inited) return;
  inv.inited = true;

  const service = document.getElementById("inv-service");
  if (service) {
    service.addEventListener("change", () => {
      inv.serviceName = service.value;
      syncHash();
      refreshInvestigate();
    });
  }

  const span = document.getElementById("inv-span-name");
  if (span) {
    span.addEventListener("change", () => {
      inv.spanName = span.value.trim();
      syncHash();
      refreshInvestigate();
    });
    span.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        span.dispatchEvent(new Event("change"));
      }
    });
  }

  const metric = document.getElementById("inv-metric");
  if (metric) {
    metric.addEventListener("change", () => {
      inv.metricName = metric.value;
      inv.metricManual = true;
      syncHash();
      refreshInvestigate();
    });
  }

  const presets = document.getElementById("inv-presets");
  if (presets) {
    presets.querySelectorAll("button[data-minutes]").forEach((b) => {
      b.addEventListener("click", () => setWindowFromPreset(Number(b.dataset.minutes)));
    });
  }

  const reset = document.getElementById("inv-time-reset");
  if (reset) {
    reset.addEventListener("click", (e) => {
      e.preventDefault();
      setWindowFromPreset(inv.presetMinutes || 60);
    });
  }

  // Chips are rebuilt on every state change, so delegate their clicks.
  const filters = document.getElementById("inv-filters");
  if (filters) {
    filters.addEventListener("click", (e) => {
      const b = e.target.closest("button[data-filter-i]");
      if (!b) return;
      inv.filters = inv.filters.filter((_, j) => j !== Number(b.dataset.filterI));
      syncHash();
      refreshInvestigate();
    });
  }
}

export { renderFilterChips, inv };
