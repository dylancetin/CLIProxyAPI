// Run with: node --test internal/managementasset/usage.test.mjs
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const script = readFileSync(new URL("usage.js", import.meta.url), "utf8");
// Each call is a fresh page load with no state carried over in memory.
const load = () => {
  const harness = {};
  vm.runInNewContext(script, { __cpaUsageHarness: harness, TextEncoder, TextDecoder, atob, URL });
  return harness;
};
const h = load();
// Values built inside the sandbox carry its prototypes; compare their JSON shape.
const plain = (value) => JSON.parse(JSON.stringify(value));
const HOUR = 3600e3;

test("the harness exposes helpers without touching the DOM", () => {
  assert.equal(typeof h.parseHash, "function");
  assert.equal(typeof h.densify, "function");
});

test("parses the host route and its query", () => {
  const { path, params } = h.parseHash("#/dashboard?cpa-page=usage&cpa-range=24h&x=a%20b&x=second&flag");
  assert.equal(path, "/dashboard");
  assert.deepEqual(plain(params), { "cpa-page": "usage", "cpa-range": "24h", x: "a b", flag: "" });
  assert.deepEqual(plain(h.parseHash("#/logs")), { path: "/logs", params: {} });
  assert.deepEqual(plain(h.parseHash("")), { path: "/", params: {} });
  assert.equal(h.parseHash("#/?cpa-page=usage").params["cpa-page"], "usage");
  assert.equal(h.parseHash("#/x?bad=%E0%A4%A").params.bad, "%E0%A4%A");
});

test("builds a hash that round-trips and drops empty values", () => {
  const hash = h.buildHash("/dashboard", { "cpa-page": "usage", "cpa-range": "custom", "cpa-from": 1, "cpa-to": "", note: "a&b" });
  assert.equal(hash, "#/dashboard?cpa-page=usage&cpa-range=custom&cpa-from=1&note=a%26b");
  assert.equal(h.parseHash(hash).params.note, "a&b");
  assert.equal(h.buildHash("/", {}), "#/");
});

function obfuscate(json, host) {
  const salt = new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|${host}`);
  const bytes = new TextEncoder().encode(json);
  return `enc::v2::${Buffer.from(bytes.map((byte, i) => byte ^ salt[i % salt.length])).toString("base64")}`;
}

test("decodes the panel's enc::v2 session", () => {
  const stored = obfuscate(JSON.stringify({ state: { apiBase: "http://127.0.0.1:8317", managementKey: "sëcret", rememberPassword: true }, version: 0 }), "127.0.0.1:8317");
  assert.deepEqual(plain(h.decodeAuth(stored, "127.0.0.1:8317")), { apiBase: "http://127.0.0.1:8317", key: "sëcret" });
  assert.equal(h.decodeAuth(stored, "other-host:8317"), null);
});

test("ignores sessions without a stored key or in retired formats", () => {
  const withoutKey = obfuscate(JSON.stringify({ state: { apiBase: "", rememberPassword: false } }), "h");
  assert.equal(h.decodeAuth(withoutKey, "h"), null);
  assert.equal(h.decodeAuth(`enc::v1::${btoa("{}")}`, "h"), null);
  assert.equal(h.decodeAuth('{"state":{"managementKey":"plain"}}', "h"), null);
  assert.equal(h.decodeAuth("enc::v2::not base64!", "h"), null);
  assert.equal(h.decodeAuth(null, "h"), null);
});

test("SHA-256 matches node:crypto across padding boundaries and UTF-8", () => {
  const sha = (text) => createHash("sha256").update(text, "utf8").digest("hex");
  for (const text of ["", "abc", "a".repeat(55), "a".repeat(56), "a".repeat(64), "b".repeat(119), "sk-ü键🔑", "x".repeat(1000)]) {
    assert.equal(h.sha256Hex(text), sha(text), `length ${text.length}`);
  }
});

test("decodes stored panel values other than the session", () => {
  const names = { ["a".repeat(64)]: "Laptop" };
  assert.deepEqual(plain(h.decodeStored(obfuscate(JSON.stringify(names), "h"), "h")), names);
  assert.equal(h.decodeStored(JSON.stringify(names), "h"), null);
});

test("maps usage key hashes to the names the panel stored", () => {
  const sha = (text) => createHash("sha256").update(text, "utf8").digest("hex");
  const apiBase = "http://127.0.0.1:8317";
  // The panel hashes the key together with its API base.
  const nameHash = (key) => sha(JSON.stringify(["api-key-name", apiBase, key]));
  const names = { [nameHash("sk-laptop")]: " Laptop ", [nameHash("sk-blank")]: "  ", [nameHash("sk-gone")]: "Removed" };
  const got = h.matchKeyNames(names, apiBase, ["sk-laptop", " sk-blank ", "sk-unnamed", 7, ""]);
  assert.deepEqual(plain([...got]), [[sha("sk-laptop"), "Laptop"]]);
  assert.equal(h.matchKeyNames(names, "http://other:8317", ["sk-laptop"]).size, 0);
  assert.equal(h.matchKeyNames(null, apiBase, ["sk-laptop"]).size, 0);
  assert.equal(h.matchKeyNames(names, apiBase, { keys: [] }).size, 0);
});

test("normalizes the management base like the panel does", () => {
  assert.equal(h.managementBase("", "https://panel.example"), "https://panel.example/v8/management");
  assert.equal(h.managementBase("10.0.0.2:8317", "x"), "http://10.0.0.2:8317/v8/management");
  assert.equal(h.managementBase(" https://api.example/v8/management/ ", "x"), "https://api.example/v8/management");
});

test("captures the base and key only from management requests", () => {
  const href = "http://127.0.0.1:8317/management.html#/";
  assert.equal(h.baseFromUrl("/v8/management/config", href), "http://127.0.0.1:8317/v8/management");
  assert.equal(h.baseFromUrl("https://remote.example/proxy/v8/management", href), "https://remote.example/proxy/v8/management");
  assert.equal(h.baseFromUrl("/v8/managementx/config", href), "");
  assert.equal(h.baseFromUrl("/v1/models", href), "");
  assert.equal(h.baseFromUrl(undefined, href), "");
  assert.equal(h.keyFromHeader("Authorization", "Bearer abc 123 "), "abc 123");
  assert.equal(h.keyFromHeader("authorization", "bearer k"), "k");
  assert.equal(h.keyFromHeader("Authorization", "Basic abc"), "");
  assert.equal(h.keyFromHeader("Authorization", "Bearer "), "");
  assert.equal(h.keyFromHeader("X-Management-Key", " k2 "), "k2");
  assert.equal(h.keyFromHeader("Content-Type", "application/json"), "");
});

const point = (requests, total = requests * 10, extra = {}) => ({ requests, failures: 0, tokens: { total, input_uncached: total / 2, output_other: total / 2 }, latency_ms_sum: requests * 100, ...extra });

test("densifies index-aligned series and zero-fills missing fields", () => {
  const result = h.densify({
    bucket: "hour",
    buckets: ["2026-11-01T00:00:00Z", "2026-11-01T01:00:00Z", "2026-11-01T02:00:00Z"],
    series: [{ key: { provider: "claude" }, points: [point(1), {}, point(3)] }],
    total: point(4),
  });
  assert.equal(result.step, HOUR);
  assert.deepEqual(plain(result.buckets), [Date.UTC(2026, 10, 1, 0), Date.UTC(2026, 10, 1, 1), Date.UTC(2026, 10, 1, 2)]);
  const [series] = result.series;
  assert.deepEqual(plain(series.points.map((p) => p.requests)), [1, 0, 3]);
  assert.equal(series.points[1].tokens.output_reasoning, 0);
  assert.equal(series.total.requests, 4);
  assert.equal(series.total.tokens.total, 40);
  assert.equal(result.total.requests, 4);
});

test("places sparse points by bucket start and derives buckets from the range", () => {
  const from = Date.UTC(2026, 10, 1, 0, 30) / 1000;
  const result = h.densify({
    bucket: "hour", from, to: "2026-11-01T04:00:00Z",
    series: [{ key: {}, points: [{ ...point(2), bucket_start: "2026-11-01T02:00:00Z" }, { ...point(5), bucket_start: Date.UTC(2026, 10, 1, 0) / 1000 }, { ...point(9), bucket_start: "2026-11-02T00:00:00Z" }] }],
  });
  assert.equal(result.buckets.length, 4);
  assert.equal(result.buckets[0], Date.UTC(2026, 10, 1, 0));
  assert.deepEqual(plain(result.series[0].points.map((p) => p.requests)), [5, 0, 2, 0]);
  assert.equal(result.series[0].total.requests, 7);
});

test("day buckets use UTC days", () => {
  const result = h.densify({ bucket: "day", from: "2026-11-01T05:00:00Z", to: "2026-11-03T00:00:00Z", series: [] });
  assert.deepEqual(plain(result.buckets), [Date.UTC(2026, 10, 1), Date.UTC(2026, 10, 2)]);
});

test("ranges cover whole buckets including the current one", () => {
  const now = Date.UTC(2026, 10, 2, 10, 17);
  assert.deepEqual(plain(h.bucketRange(now, 24 * HOUR, HOUR)), { from: Date.UTC(2026, 10, 1, 10), to: Date.UTC(2026, 10, 2, 11) });
  assert.deepEqual(plain(h.bucketRange(now, 7 * 24 * HOUR, 24 * HOUR)), { from: Date.UTC(2026, 9, 26), to: Date.UTC(2026, 10, 3) });
});

test("linear scale maps and clamps to the range", () => {
  const y = h.linearScale(0, 100, 250, 10);
  assert.equal(y(0), 250);
  assert.equal(y(100), 10);
  assert.equal(y(50), 130);
  assert.equal(y(200), 10);
  assert.equal(y(-5), 250);
  assert.equal(h.linearScale(0, 0, 250, 10)(7), 250);
});

test("log scale spaces decades evenly and puts non-positive values on the baseline", () => {
  const y = h.logScale(1, 1000, 300, 0);
  assert.equal(y(1), 300);
  assert.equal(y(10), 200);
  assert.equal(y(100), 100);
  assert.equal(y(1000), 0);
  assert.equal(y(0), 300);
  assert.equal(y(-3), 300);
  assert.equal(y(0.01), 300);
});

test("nice ticks start at zero and cover the maximum", () => {
  assert.deepEqual(plain(h.niceTicks(100)), [0, 20, 40, 60, 80, 100]);
  assert.deepEqual(plain(h.niceTicks(7)), [0, 2, 4, 6, 8]);
  assert.deepEqual(plain(h.niceTicks(1.3)), [0, 0.5, 1, 1.5]);
  assert.deepEqual(plain(h.niceTicks(3, 5, true)), [0, 1, 2, 3]);
  assert.deepEqual(plain(h.niceTicks(0)), [0, 1]);
  const ticks = h.niceTicks(123456);
  assert.ok(ticks.at(-1) >= 123456 && ticks.length <= 7);
});

test("log ticks are whole decades around the data", () => {
  assert.deepEqual(plain(h.logTicks(3, 4200)), [1, 10, 100, 1000, 10000]);
  assert.deepEqual(plain(h.logTicks(0.02, 0.5)), [0.01, 0.1, 1]);
  assert.deepEqual(plain(h.logTicks(Infinity, 0)), [1, 10]);
  assert.deepEqual(plain(h.logTicks(5, 5)), [1, 10]);
});

test("line paths break at gaps", () => {
  assert.equal(h.linePath([0, 10, 20, 30, 40], [5, 6, null, 8.125, 9]), "M0,5L10,6M30,8.13L40,9");
  assert.equal(h.linePath([0, 1], [null, NaN]), "");
});

test("area paths close along the lower edge", () => {
  assert.equal(h.areaPath([0, 10], [5, 6], [20, 20]), "M0,5L10,6L10,20L0,20Z");
  assert.equal(h.areaPath([], [], []), "");
});

test("bar paths round the data end and stay square at the baseline", () => {
  assert.equal(h.barPath(10, 20, 100, 40, 4), "M10,100V44Q10,40 14,40H26Q30,40 30,44V100Z");
  assert.equal(h.barPath(0, 2, 10, 9, 4), "M0,10V10Q0,9 1,9H1Q2,9 2,10V10Z");
  assert.equal(h.barPath(0, 10, 50, 50), "");
  assert.equal(h.barPath(0, 0, 50, 10), "");
});

test("stacks series cumulatively and ignores empty values", () => {
  assert.deepEqual(plain(h.stack([[1, 2, null], [3, 0, 4]])), [[[0, 1], [0, 2], [0, 0]], [[1, 4], [2, 2], [0, 4]]]);
});

test("formats compact numbers", () => {
  const cases = [[0, "0"], [7, "7"], [999, "999"], [999.6, "1k"], [1234, "1.2k"], [12345, "12.3k"], [123456, "123k"],
    [999499, "999k"], [999500, "1M"], [4.5e6, "4.5M"], [2.25e9, "2.3B"], [1.5e12, "1.5T"], [3.456, "3.46"], [-1500, "-1.5k"], [null, "—"], [NaN, "—"]];
  for (const [value, expected] of cases) assert.equal(h.compact(value), expected, String(value));
});

test("formats durations and costs", () => {
  assert.equal(h.duration(850.4), "850 ms");
  assert.equal(h.duration(1240), "1.24 s");
  assert.equal(h.duration(12345), "12.3 s");
  assert.equal(h.duration(90000), "1.5 min");
  assert.equal(h.duration(null), "—");
  assert.equal(h.usd(0), "$0.00");
  assert.equal(h.usd(12.345), "$12.35");
  assert.equal(h.usd(0.0042), "$0.0042");
  assert.equal(h.usd(0.00001), "<$0.0001");
  assert.equal(h.usd(12500), "$12.5k");
  assert.equal(h.formatValue(1500, "ms"), "1.5 s");
  assert.equal(h.formatValue(1500), "1.5k");
});

test("metrics derive input, output and averages from a point", () => {
  const p = { requests: 4, ttft_count: 0, latency_ms_sum: 1000, cost_usd: 1.5, tokens: { input_uncached: 10, input_cache_read: 20, input_cache_write: 5, output_reasoning: 7, output_other: 3, total: 45 } };
  assert.equal(h.METRICS.input.get(p), 35);
  assert.equal(h.METRICS.output.get(p), 10);
  assert.equal(h.METRICS.latency.get(p), 250);
  assert.equal(h.METRICS.ttft.get(p), null);
  assert.equal(h.METRICS.latency.get({ ...p, requests: 0 }), null);
});

test("CSV quotes separators and neutralizes formulas", () => {
  const csv = h.buildCSV([["bucket", "model, alias", 'say "hi"'], ["2026-11-01T00:00:00.000Z", 12, "=SUM(A1)"], [null, NaN, "-1+2"]]);
  assert.equal(csv, 'bucket,"model, alias","say ""hi"""\r\n2026-11-01T00:00:00.000Z,12,\'=SUM(A1)\r\n,,\'-1+2\r\n');
  assert.equal(h.csvCell("line\nbreak"), '"line\nbreak"');
});

const named = (count, dim = "model") => Array.from({ length: count }, (_, i) => ({ key: { [dim]: `m${i}` }, points: [point(i + 1)], total: point(i + 1) }));

test("folds the tail beyond the top N into other", () => {
  const series = named(10);
  series.push({ key: { model: "other" }, points: [point(100)], total: point(100) });
  const folded = h.foldSeries(series, ["model"], 8);
  assert.equal(folded.length, 9);
  assert.deepEqual(plain(folded.slice(0, 8).map((entry) => entry.id)), ["m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7"].map((m) => `model=${m}`));
  assert.equal(folded[8].other, true);
  assert.equal(folded[8].points[0].requests, 9 + 10 + 100);
  assert.equal(folded[8].total.requests, 119);
  assert.equal(h.foldSeries([{ key: {}, points: [point(1)], total: point(1) }], [])[0].id, "total");
  assert.equal(h.seriesId({ provider: "claude", model: "" }, ["provider", "model"]), "provider=claude\u001fmodel=");
  assert.notEqual(h.seriesId({ provider: "x" }, ["provider"]), h.seriesId({ model: "x" }, ["model"]));
});

const styled = (list, remembered) => {
  const { slots, memory } = h.assignColors(list.filter((entry) => !entry.other).map((entry) => entry.id), remembered);
  return { styles: list.map((entry) => h.slotStyle(entry.other ? -1 : slots.get(entry.id))), slots, memory };
};

test("top 10 of 12 series keeps 10 named series with distinct styles plus other", () => {
  const folded = h.foldSeries(named(12), ["model"], 10);
  assert.equal(folded.length, 11);
  assert.equal(folded.filter((entry) => !entry.other).length, 10);
  assert.equal(folded.at(-1).other, true);
  assert.equal(folded.at(-1).total.requests, 11 + 12);
  const { styles } = styled(folded, []);
  assert.equal(new Set(styles.map((style) => style.token)).size, 11);
  assert.equal(styles.at(-1).color, "var(--cpa-series-other)");
});

test("top 20 shows twenty distinguishable styles from eight hues", () => {
  const folded = h.foldSeries(named(25), ["model"], 20);
  assert.equal(folded.filter((entry) => !entry.other).length, 20);
  const { styles } = styled(folded, []);
  const tokens = styles.slice(0, 20).map((style) => style.token);
  assert.equal(new Set(tokens).size, 20);
  for (const style of styles.slice(0, 20)) {
    assert.match(style.color, /^var\(--cpa-series-[1-8]\)$/);
    assert.ok([0, 1, 2].includes(style.pattern));
  }
  // Solid slots fill before hatched ones.
  assert.equal(styles.slice(0, 20).filter((style) => style.pattern === 0).length, 8);
  assert.equal(styles.slice(0, 20).filter((style) => style.pattern === 1).length, 8);
});

test("eight or fewer series are all solid", () => {
  const { styles } = styled(h.foldSeries(named(8), ["model"], 10), []);
  assert.ok(styles.every((style) => style.pattern === 0));
  assert.equal(new Set(styles.map((style) => style.color)).size, 8);
});

test("other is always the neutral grey", () => {
  for (const slot of [-1, undefined, null, h.SLOTS, 1.5, "3"]) assert.equal(h.slotStyle(slot).color, "var(--cpa-series-other)");
  const folded = h.foldSeries([...named(3), { key: { model: "other" }, points: [point(1)], total: point(1) }], ["model"], 10);
  const { styles } = styled(folded, [[folded.at(-1).id, 0], ["model=other", 1]]);
  assert.equal(styles.at(-1).color, "var(--cpa-series-other)");
  assert.ok(styles.slice(0, -1).every((style) => style.color !== "var(--cpa-series-other)"));
});

test("the same key gets the same color in two fresh runs with the same stored state", () => {
  const stored = JSON.stringify({ colors: [["provider=gemini", 5]] });
  const ids = ["provider=claude", "provider=codex", "provider=gemini", "provider=openai"];
  const [a, b] = [load(), load()].map((run) => run.assignColors(ids, run.sanitizeView(JSON.parse(stored)).colors));
  assert.deepEqual(plain([...a.slots]), plain([...b.slots]));
  assert.equal(a.slots.get("provider=gemini"), 5);
  // Without stored state the key's hash alone decides, whatever else is on screen.
  const alone = load().assignColors(["provider=codex"]).slots.get("provider=codex");
  assert.equal(alone, h.hashKey("provider=codex") % 8);
  assert.equal(load().assignColors(["provider=codex", "provider=zzz"]).slots.get("provider=codex"), alone);
});

test("collisions probe the next hue, solid before hatched", () => {
  const start = h.hashKey("provider=x") % 8;
  const taken = Array.from({ length: 8 }, (_, i) => [`k${i}`, i]).filter(([, slot]) => slot !== (start + 1) % 8);
  const { slots } = h.assignColors(["provider=x", ...taken.map(([id]) => id)], taken);
  assert.equal(slots.get("provider=x"), (start + 1) % 8);
  const full = Array.from({ length: 8 }, (_, i) => [`k${i}`, i]);
  assert.equal(h.assignColors(["provider=x", ...full.map(([id]) => id)], full).slots.get("provider=x"), 8 + start);
});

test("a stored assignment survives a reload of state", () => {
  const first = h.assignColors(["provider=claude", "provider=codex", "provider=gemini"]);
  const view = h.sanitizeView({ ...h.sanitizeView(null), colors: first.memory });
  const reloaded = load().sanitizeView(JSON.parse(JSON.stringify(view)));
  assert.deepEqual(plain(reloaded.colors), plain(first.memory));
  // A different set and order on screen keeps each remembered slot.
  const next = load().assignColors(["provider=new", "provider=gemini", "provider=claude"], reloaded.colors);
  assert.equal(next.slots.get("provider=gemini"), first.slots.get("provider=gemini"));
  assert.equal(next.slots.get("provider=claude"), first.slots.get("provider=claude"));
  // A remembered slot overrides the hash.
  const moved = (h.hashKey("provider=claude") + 3) % 8;
  assert.equal(h.assignColors(["provider=claude"], [["provider=claude", moved]]).slots.get("provider=claude"), moved);
});

test("color memory keeps the most recently shown keys within its bound", () => {
  const old = Array.from({ length: h.MEMORY }, (_, i) => [`k${i}`, i % h.SLOTS]);
  const { memory } = h.assignColors(["k0", "fresh"], old);
  assert.equal(memory.length, h.MEMORY);
  assert.deepEqual(plain(memory.slice(-2).map(([id]) => id)), ["k0", "fresh"]);
  assert.equal(memory.some(([id]) => id === "k1"), false);
  const view = h.sanitizeView({ colors: [...old, ["k0", 23], ["bad", 24], ["neg", -1], [7, 1], "x", ["frac", 1.5]] });
  assert.equal(view.colors.length, h.MEMORY);
  assert.deepEqual(plain(view.colors.at(-1)), ["k0", 23]);
  assert.equal(view.colors.some(([id]) => ["bad", "neg", "frac"].includes(id)), false);
});

test("x ticks align to local hours and UTC days", () => {
  const buckets = Array.from({ length: 24 }, (_, i) => Date.UTC(2026, 10, 1, i));
  assert.deepEqual(plain(h.tickIndexes(buckets, HOUR, 24 * 20, () => 0)), [0, 6, 12, 18]);
  assert.deepEqual(plain(h.tickIndexes(buckets, HOUR, 24 * 20, () => 2 * HOUR)), [4, 10, 16, 22]);
  assert.deepEqual(plain(h.tickIndexes(buckets, HOUR, 24 * 100, () => 0)), buckets.map((_, i) => i));
  assert.deepEqual(plain(h.tickIndexes([], HOUR, 100, () => 0)), []);
});

test("hides the host page while keeping it rendered", () => {
  // The panel stops running animations with commitStyles(), which throws inside display:none.
  const css = readFileSync(new URL("usage.css", import.meta.url), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
  const rules = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(([, selector]) => selector.includes(".main-content >"));
  assert.ok(rules.length > 0);
  for (const [, selector, body] of rules) {
    assert.doesNotMatch(body, /display\s*:\s*none/, selector.trim());
    assert.match(body, /position\s*:\s*absolute/);
    assert.match(body, /visibility\s*:\s*hidden/);
    assert.match(body, /clip-path\s*:\s*inset\(50%\)/);
  }
});

test("labels the UTC offset", () => {
  assert.equal(h.utcOffsetLabel(480), "UTC+08:00");
  assert.equal(h.utcOffsetLabel(-210), "UTC−03:30");
  assert.equal(h.utcOffsetLabel(0), "UTC+00:00");
});

const en = (key, ...args) => h.translate("en", key, ...args);
const zh = (key, ...args) => h.translate("zh-CN", key, ...args);
const priceMap = { "claude-x": { input: 0, cache_read: 0, cache_write: 0, output: 0 }, "gpt-y": { input: 1, output: 4 } };

test("a model is priced by an exact entry or a default entry", () => {
  assert.equal(h.isPriced(priceMap, "claude-x"), true);
  assert.equal(h.isPriced(priceMap, "gpt-y"), true);
  assert.equal(h.isPriced(priceMap, "glm-z"), false);
  assert.equal(h.isPriced(priceMap, "toString"), false);
  assert.equal(h.isPriced({}, "claude-x"), false);
  assert.equal(h.isPriced({ default: { input: 1 } }, "glm-z"), true);
});

test("breakdown cost cells mark unpriced models and claim nothing without a price map", () => {
  const rows = [{ value: "claude-x", point: { cost_usd: 0 } }, { value: "glm-z", point: { cost_usd: 0 } }, { value: "", point: {} }];
  const cell = (prices, row, tr = en) => h.unpricedLabel(prices, row.value, tr) || h.usd(row.point.cost_usd ?? 0);
  assert.deepEqual(rows.map((row) => cell(priceMap, row)), ["$0.00", "unpriced", "unpriced"]);
  assert.equal(cell(priceMap, rows[1], zh), "未定价");
  assert.deepEqual(rows.map((row) => cell({ ...priceMap, default: {} }, row)), ["$0.00", "$0.00", "$0.00"]);
  assert.deepEqual(rows.map((row) => cell(null, row)), ["$0.00", "$0.00", "$0.00"]);
});

test("the cost tile counts unpriced models in the summary", () => {
  const summary = { by: { model: [{ value: "claude-x" }, { value: "glm-z" }, { value: "kimi-k" }, null] } };
  assert.equal(h.costSub(priceMap, summary, en), "2 models unpriced");
  assert.equal(h.costSub(priceMap, summary, zh), "2 个模型未定价");
  assert.equal(h.costSub(priceMap, { by: { model: [{ value: "glm-z" }] } }, en), "1 model unpriced");
  assert.equal(h.costSub(priceMap, { by: { model: [{ value: "gpt-y" }] } }, en), "");
  assert.equal(h.costSub({ default: {} }, summary, en), "");
  assert.equal(h.costSub({}, summary, en), "3 models unpriced");
  assert.equal(h.costSub({}, {}, en), "No prices set");
  assert.equal(h.costSub(null, summary, en), "");
});

test("restores a saved view and rejects unknown values", () => {
  const view = h.sanitizeView({ range: "7d", bucket: "day", metric: "cost", group: ["model", "account", "provider", "bogus"], type: "lines", scale: "log", top: 20, filters: { provider: ["claude", 3, "claude"], nope: ["x"] }, table: true, auto: true, tab: "api_key" });
  assert.deepEqual(plain(view), { range: "7d", from: 0, to: 0, bucket: "day", metric: "cost", group: ["model", "account"], type: "lines", scale: "log", top: 20, filters: { provider: ["claude"] }, table: true, auto: true, tab: "api_key", colors: [] });
  const fallback = h.sanitizeView({ range: "toString", metric: "constructor", type: "pie", top: 7, group: "provider", filters: null, from: 5, to: 1 });
  assert.deepEqual(plain(fallback), plain(h.sanitizeView(null)));
  assert.equal(h.sanitizeView({ range: "custom" }).range, "24h");
  assert.deepEqual(plain(h.sanitizeView({ range: "custom", from: 1000, to: 2000 })).range, "custom");
  assert.deepEqual(plain(h.sanitizeView({ group: [] }).group), []);
});
