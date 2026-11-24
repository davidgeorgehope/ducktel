import test from "node:test";
import assert from "node:assert/strict";

const store = new Map();
globalThis.localStorage = {
  getItem: (k) => (store.has(k) ? store.get(k) : null),
  setItem: (k, v) => { store.set(k, String(v)); },
  removeItem: (k) => { store.delete(k); },
};

// document stays undefined so importing app.js does not boot the UI.
const { api } = await import("./app.js");

function jsonResponse(status, body) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

test("api retries without Bearer after a stored token is rejected", async () => {
  store.clear();
  localStorage.setItem("ducktel_token", "stale");
  const auths = [];
  globalThis.fetch = async (_path, opts) => {
    auths.push(opts.headers && opts.headers.Authorization);
    if (opts.headers && opts.headers.Authorization === "Bearer stale") {
      return jsonResponse(401, { error: "unauthorized" });
    }
    return jsonResponse(200, { columns: ["tenant"], rows: [{ tenant: "acme" }] });
  };

  const body = await api("/api/tenants");
  assert.deepEqual(auths, ["Bearer stale", undefined]);
  assert.equal(localStorage.getItem("ducktel_token"), null);
  assert.deepEqual(body.rows, [{ tenant: "acme" }]);
});

test("api shows the gate only when a retry also returns 401", async () => {
  store.clear();
  localStorage.setItem("ducktel_token", "stale");
  const messages = [];
  const classLists = { add() {}, remove() {} };
  globalThis.document = {
    getElementById: (id) => ({
      classList: classLists,
      textContent: "",
      set textContent(v) { if (id === "gate-error") messages.push(v); },
      get textContent() { return ""; },
    }),
  };
  globalThis.fetch = async () => jsonResponse(401, { error: "unauthorized" });

  await assert.rejects(() => api("/api/tenants"), /unauthorized/);
  assert.equal(localStorage.getItem("ducktel_token"), null);
  assert.ok(messages.some((m) => /Rejected/.test(m)));
});
