import test from "node:test";
import assert from "node:assert/strict";

import {
  parseFilter,
  resolveWindow,
  stateFromParams,
  paramsFromState,
  toApiWindow,
  chooseStep,
  unitToMs,
  toMs,
  fromMs,
  sumServerErrors,
  pickErrorColumn,
  matchingPreset,
  apiTimeMs,
  whyActions,
  splitAtGaps,
} from "./investigate.js";

const NOW = new Date("2026-01-01T12:00:00.000Z");

test("parseFilter splits on the first colon only", () => {
  assert.deepEqual(parseFilter("http.target:/a:b", ""), {
    key: "http.target",
    value: "/a:b",
    op: "",
  });
  assert.deepEqual(parseFilter("cache:miss", "!="), { key: "cache", value: "miss", op: "!=" });
  assert.deepEqual(parseFilter("bare", ""), { key: "bare", value: "", op: "" });
});

test("resolveWindow converts since/until to absolute UTC", () => {
  const p = new URLSearchParams({ since_minutes: "60", until_minutes: "15" });
  assert.deepEqual(resolveWindow(p, NOW), {
    from: "2026-01-01T11:00:00.000Z",
    to: "2026-01-01T11:45:00.000Z",
  });

  const absolute = new URLSearchParams({
    from: "2026-01-01T09:00:00Z",
    to: "2026-01-01T10:00:00Z",
    since_minutes: "120",
  });
  assert.deepEqual(resolveWindow(absolute, NOW), {
    from: "2026-01-01T09:00:00.000Z",
    to: "2026-01-01T10:00:00.000Z",
  });
});

test("resolveWindow defaults to the last hour", () => {
  assert.deepEqual(resolveWindow(new URLSearchParams(), NOW), {
    from: "2026-01-01T11:00:00.000Z",
    to: "2026-01-01T12:00:00.000Z",
  });
});

test("stateFromParams carries the MCP argument names", () => {
  const p = new URLSearchParams();
  p.set("tenant", "acme");
  p.set("service_name", "api");
  p.set("span_name", "GET /orders");
  p.append("filter", "http.route:/orders");
  p.append("filter_not", "cache:hit");
  p.set("metric_name", "http.server.request.duration");
  p.set("since_minutes", "360");
  p.set("min_ms", "250");
  p.set("max_ms", "500");
  p.set("trace_id", "trace-1");

  const s = stateFromParams(p, NOW);
  assert.equal(s.tenant, "acme");
  assert.equal(s.serviceName, "api");
  assert.equal(s.spanName, "GET /orders");
  assert.deepEqual(s.filters, [
    { key: "http.route", value: "/orders", op: "" },
    { key: "cache", value: "hit", op: "!=" },
  ]);
  assert.equal(s.metricName, "http.server.request.duration");
  assert.equal(s.minMs, 250);
  assert.equal(s.maxMs, 500);
  assert.equal(s.traceId, "trace-1");
  assert.equal(s.from, "2026-01-01T06:00:00.000Z");
  assert.equal(s.to, "2026-01-01T12:00:00.000Z");
  assert.equal(s.presetMinutes, 360);
});

test("paramsFromState writes absolute from/to and both filter forms", () => {
  const p = paramsFromState({
    tenant: "acme",
    serviceName: "api",
    spanName: "op",
    filters: [
      { key: "cache", value: "miss", op: "" },
      { key: "region", value: "eu", op: "!=" },
    ],
    metricName: "m",
    from: "2026-01-01T11:00:00.000Z",
    to: "2026-01-01T12:00:00.000Z",
    minMs: 10,
    maxMs: 20,
    traceId: "t1",
    limit: 15,
  });
  assert.equal(p.get("tenant"), "acme");
  assert.equal(p.get("service_name"), "api");
  assert.equal(p.get("span_name"), "op");
  assert.deepEqual(p.getAll("filter"), ["cache:miss"]);
  assert.deepEqual(p.getAll("filter_not"), ["region:eu"]);
  assert.equal(p.get("metric_name"), "m");
  assert.equal(p.get("from"), "2026-01-01T11:00:00.000Z");
  assert.equal(p.get("to"), "2026-01-01T12:00:00.000Z");
  assert.equal(p.get("min_ms"), "10");
  assert.equal(p.get("max_ms"), "20");
  assert.equal(p.get("trace_id"), "t1");
  assert.equal(p.get("limit"), "15");
});

test("state and params round trip", () => {
  const first = new URLSearchParams({
    tenant: "acme",
    service_name: "api",
    from: "2026-01-01T10:00:00.000Z",
    to: "2026-01-01T11:00:00.000Z",
    filter: "a:b",
    filter_not: "c:d",
  });
  const s = stateFromParams(first, NOW);
  const again = stateFromParams(paramsFromState(s), NOW);
  assert.deepEqual(again, s);
});

test("toApiWindow ceils since and floors until", () => {
  assert.deepEqual(toApiWindow("2026-01-01T11:00:30Z", "2026-01-01T11:59:30Z", NOW), {
    sinceMinutes: 60,
    untilMinutes: 0,
  });
  assert.deepEqual(toApiWindow("2026-01-01T10:00:00Z", "2026-01-01T10:30:00Z", NOW), {
    sinceMinutes: 120,
    untilMinutes: 90,
  });
  // A window ending slightly in the future cannot produce a negative until.
  assert.deepEqual(toApiWindow("2026-01-01T11:00:00Z", "2026-01-01T12:00:30Z", NOW), {
    sinceMinutes: 60,
    untilMinutes: 0,
  });
});

test("chooseStep rounds toward 120 points", () => {
  assert.equal(chooseStep("2026-01-01T11:00:00Z", "2026-01-01T12:00:00Z"), "1m");
  assert.equal(chooseStep("2026-01-01T06:00:00Z", "2026-01-01T12:00:00Z"), "5m");
  assert.equal(chooseStep("2025-12-31T12:00:00Z", "2026-01-01T12:00:00Z"), "15m");
  assert.equal(chooseStep("2025-12-25T12:00:00Z", "2026-01-01T12:00:00Z"), "1h");
});

test("metric units convert to and from milliseconds", () => {
  assert.equal(unitToMs("ms"), 1);
  assert.equal(unitToMs("s"), 1000);
  assert.equal(unitToMs("us"), 0.001);
  assert.equal(unitToMs("By"), null);
  assert.equal(toMs(1.5, "s"), 1500);
  assert.equal(toMs(250, "ms"), 250);
  assert.equal(fromMs(1500, "s"), 1.5);
  assert.equal(toMs(1, "bogus"), null);
});

test("sumServerErrors adds only 5xx counts", () => {
  const col = "attr.http.response.status_code";
  const rows = [
    { [col]: "500", value: 3 },
    { [col]: "503", value: 2 },
    { [col]: "404", value: 9 },
    { [col]: "200", value: 7 },
    { [col]: null, value: 11 },
  ];
  assert.equal(sumServerErrors(rows, col), 5);
  assert.equal(sumServerErrors([], col), 0);
});

test("pickErrorColumn falls back to the second status-code attribute", () => {
  const first = "attr.http.response.status_code";
  const second = "attr.http.status_code";
  assert.equal(pickErrorColumn([{ [first]: "500" }]), first);
  assert.equal(pickErrorColumn([{ [second]: "500" }]), second);
  assert.equal(pickErrorColumn([]), second);
});

test("matchingPreset recognises the four window presets", () => {
  assert.equal(matchingPreset("2026-01-01T11:00:00Z", "2026-01-01T12:00:00Z"), 60);
  assert.equal(matchingPreset("2026-01-01T06:00:00Z", "2026-01-01T12:00:00Z"), 360);
  assert.equal(matchingPreset("2025-12-31T12:00:00Z", "2026-01-01T12:00:00Z"), 1440);
  assert.equal(matchingPreset("2025-12-25T12:00:00Z", "2026-01-01T12:00:00Z"), 10080);
  assert.equal(matchingPreset("2026-01-01T11:30:00Z", "2026-01-01T12:00:00Z"), null);
});

test("apiTimeMs converts the API's Unix microseconds to milliseconds", () => {
  assert.equal(apiTimeMs(1762599600000000), 1762599600000);
  assert.equal(new Date(apiTimeMs("1762599600000000")).toISOString(), "2025-11-08T11:00:00.000Z");
});

test("whyActions offers only buttons that can narrow the scope", () => {
  assert.deepEqual(whyActions("cache"), { focus: true, exclude: true });
  assert.deepEqual(whyActions("resource.k8s.pod.name"), { focus: true, exclude: true });
  assert.deepEqual(whyActions("span_name"), { focus: true, exclude: false });
  assert.deepEqual(whyActions("status_code"), { focus: false, exclude: false });
  assert.deepEqual(whyActions("span_kind"), { focus: false, exclude: false });
});

test("splitAtGaps breaks a series where steps are missing", () => {
  const pts = [0, 60, 120, 600, 660].map((t) => ({ t, v: 1 }));
  assert.deepEqual(splitAtGaps(pts).map((seg) => seg.map((p) => p.t)), [[0, 60, 120], [600, 660]]);
  assert.deepEqual(splitAtGaps([{ t: 5, v: 1 }]).length, 1);
  assert.deepEqual(splitAtGaps([]), []);
});
