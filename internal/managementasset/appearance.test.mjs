// Run with: node --test internal/managementasset/appearance.test.mjs
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const script = readFileSync(new URL("appearance.js", import.meta.url), "utf8");

function initialize({ saved = null, systemDark = false, blocked = false, view = null } = {}) {
  const storage = new Map([["cli-proxy-theme", saved], ["cpa-quota-view", view]]);
  const attributes = new Map();
  const root = {
    dataset: {},
    setAttribute: (name, value) => attributes.set(name, value),
    removeAttribute: (name) => attributes.delete(name),
  };
  vm.runInNewContext(script, {
    document: { documentElement: root, readyState: "loading", addEventListener() {} },
    window: { matchMedia: () => ({ matches: systemDark }) },
    localStorage: {
      getItem(key) {
        if (blocked) throw new Error("Storage disabled");
        return storage.get(key) ?? null;
      },
      setItem(key, value) {
        if (blocked) throw new Error("Storage disabled");
        storage.set(key, value);
      },
    },
  });
  return { storage, attributes, root };
}

test("new browsers default to dark even with a light system theme", () => {
  const result = initialize();
  assert.equal(result.attributes.get("data-theme"), "dark");
  assert.deepEqual(JSON.parse(result.storage.get("cli-proxy-theme")), {
    state: { theme: "dark", resolvedTheme: "dark" }, version: 0,
  });
  assert.equal(result.root.dataset.cpaQuotaView, "ledger");
});

for (const theme of ["dark", "light", "white", "auto"]) {
  test(`preserves the saved ${theme} preference`, () => {
    const saved = JSON.stringify({ state: { theme }, version: 0 });
    const result = initialize({ saved });
    assert.equal(result.storage.get("cli-proxy-theme"), saved);
    assert.equal(result.attributes.get("data-theme"), theme === "auto" ? "white" : theme === "light" ? undefined : theme);
  });
}

test("saved automatic mode still follows a dark system theme", () => {
  const result = initialize({ saved: JSON.stringify({ state: { theme: "auto" } }), systemDark: true });
  assert.equal(result.attributes.get("data-theme"), "dark");
});

for (const saved of ["broken json", "null", "{}", '{"state":{"theme":"unknown"}}']) {
  test(`recovers an invalid preference: ${saved}`, () => {
    const result = initialize({ saved });
    assert.equal(JSON.parse(result.storage.get("cli-proxy-theme")).state.theme, "dark");
  });
}

test("unavailable storage does not prevent the application from starting", () => {
  assert.equal(initialize({ blocked: true }).attributes.get("data-theme"), "dark");
});

test("preserves the selected cards layout", () => {
  assert.equal(initialize({ view: "cards" }).root.dataset.cpaQuotaView, "cards");
  assert.equal(initialize({ view: "unknown" }).root.dataset.cpaQuotaView, "ledger");
});
