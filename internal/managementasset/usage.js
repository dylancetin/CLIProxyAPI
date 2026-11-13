(() => {
  "use strict";

  const HOUR = 3600e3;
  const DAY = 24 * HOUR;
  const DIMS = ["provider", "model", "requested_model", "account", "api_key", "reasoning_effort", "service_tier", "stream"];
  const RANGES = { "1h": HOUR, "6h": 6 * HOUR, "24h": DAY, "7d": 7 * DAY, "30d": 30 * DAY };
  const TYPES = ["stacked", "grouped", "lines", "area"];
  const TOPS = [5, 10, 20];
  // More than eight hues stop being distinguishable, so later slots reuse the
  // hues with a 45° and then a 135° hatch. The 24 slots cover the largest top N.
  const HUES = 8;
  const SLOTS = HUES * 3;
  const MEMORY = 64;
  const TOKEN_FIELDS = ["input_uncached", "input_cache_read", "input_cache_write", "output_reasoning", "output_other", "unclassified", "total"];
  const SUM_FIELDS = ["requests", "failures", "latency_ms_sum", "ttft_ms_sum", "ttft_count", "cost_usd"];
  const MAX_FIELDS = ["latency_ms_max", "ttft_ms_max"];
  const PRICE_FIELDS = ["input", "cache_read", "cache_write", "output"];
  const SEP = "\u001f";

  const num = (value) => (typeof value === "number" ? (Number.isFinite(value) ? value : 0) : Number(value) || 0);
  const round = (value, digits) => +value.toFixed(digits);
  const clamp = (value, low, high) => Math.min(high, Math.max(low, value));

  function normPoint(point) {
    const out = { tokens: {} };
    for (const field of SUM_FIELDS.concat(MAX_FIELDS)) out[field] = num(point?.[field]);
    for (const field of TOKEN_FIELDS) out.tokens[field] = num(point?.tokens?.[field]);
    return out;
  }

  function addPoints(a, b) {
    const out = normPoint(a);
    for (const field of SUM_FIELDS) out[field] += b[field];
    for (const field of MAX_FIELDS) out[field] = Math.max(out[field], b[field]);
    for (const field of TOKEN_FIELDS) out.tokens[field] += b.tokens[field];
    return out;
  }

  const METRICS = {
    requests: { get: (p) => p.requests },
    failures: { get: (p) => p.failures },
    tokens: { get: (p) => p.tokens.total },
    input: { get: (p) => p.tokens.input_uncached + p.tokens.input_cache_read + p.tokens.input_cache_write },
    output: { get: (p) => p.tokens.output_reasoning + p.tokens.output_other },
    cache_read: { get: (p) => p.tokens.input_cache_read },
    cache_write: { get: (p) => p.tokens.input_cache_write },
    reasoning: { get: (p) => p.tokens.output_reasoning },
    latency: { get: (p) => (p.requests ? p.latency_ms_sum / p.requests : null), unit: "ms", avg: true },
    ttft: { get: (p) => (p.ttft_count ? p.ttft_ms_sum / p.ttft_count : null), unit: "ms", avg: true },
    cost: { get: (p) => p.cost_usd, unit: "usd" },
  };

  function compact(value) {
    if (value == null || !Number.isFinite(value)) return "—";
    const sign = value < 0 ? "-" : "";
    let size = Math.abs(value);
    if (size < 999.5) return sign + round(size, size >= 100 || Number.isInteger(size) ? 0 : size >= 10 ? 1 : 2);
    let unit = 0;
    while (size >= 999.5 && unit < 4) {
      size /= 1000;
      unit++;
    }
    return sign + round(size, size >= 100 ? 0 : 1) + ["", "k", "M", "B", "T"][unit];
  }

  function duration(ms) {
    if (ms == null || !Number.isFinite(ms)) return "—";
    if (ms < 999.5) return `${Math.round(ms)} ms`;
    if (ms < 59950) return `${round(ms / 1000, ms < 9995 ? 2 : 1)} s`;
    return `${round(ms / 60000, 1)} min`;
  }

  function usd(value) {
    if (value == null || !Number.isFinite(value)) return "—";
    if (value >= 999.5) return `$${compact(value)}`;
    if (value === 0 || value >= 0.01) return `$${value.toFixed(2)}`;
    return value < 0.0001 ? "<$0.0001" : `$${+value.toPrecision(2)}`;
  }

  const formatValue = (value, unit) => (unit === "ms" ? duration(value) : unit === "usd" ? usd(value) : compact(value));

  function parseTime(value) {
    if (typeof value === "number") return value < 1e12 ? value * 1000 : value;
    if (typeof value === "string" && /^\d+$/.test(value)) return parseTime(Number(value));
    const ms = Date.parse(value);
    return Number.isFinite(ms) ? ms : NaN;
  }

  const floorTo = (ms, step) => Math.floor(ms / step) * step;

  // Ranges cover whole buckets so the first and the current bucket are never cut.
  const bucketRange = (now, span, step) => ({ from: floorTo(now - span, step), to: floorTo(now, step) + step });

  function utcOffsetLabel(minutesEast) {
    const size = Math.abs(minutesEast);
    const pad = (value) => String(value).padStart(2, "0");
    return `UTC${minutesEast < 0 ? "−" : "+"}${pad(Math.floor(size / 60))}:${pad(size % 60)}`;
  }

  function tickIndexes(buckets, step, width, offsetOf, minGap = 72) {
    if (!buckets.length) return [];
    const per = width / buckets.length;
    const choices = step === DAY ? [1, 2, 7, 14, 28, 56] : [1, 2, 3, 6, 12, 24, 48, 96, 168];
    const every = choices.find((choice) => choice * per >= minGap) || Math.ceil(minGap / per);
    return buckets.flatMap((at, index) => (Math.floor((at + offsetOf(at)) / step) % every === 0 ? [index] : []));
  }

  function linearScale(d0, d1, r0, r1) {
    const k = d1 === d0 ? 0 : (r1 - r0) / (d1 - d0);
    return (value) => clamp(r0 + (value - d0) * k, Math.min(r0, r1), Math.max(r0, r1));
  }

  // Non-positive values have no logarithm; they sit on the baseline.
  function logScale(d0, d1, r0, r1) {
    const l0 = Math.log10(d0);
    const k = d1 === d0 ? 0 : (r1 - r0) / (Math.log10(d1) - l0);
    return (value) => (value > 0 ? clamp(r0 + (Math.log10(value) - l0) * k, Math.min(r0, r1), Math.max(r0, r1)) : r0);
  }

  function niceTicks(max, count = 5, integer = false) {
    if (!(max > 0)) return [0, 1];
    const raw = max / count;
    const magnitude = 10 ** Math.floor(Math.log10(raw));
    let step = [1, 2, 5, 10].map((m) => m * magnitude).find((candidate) => candidate >= raw);
    if (integer) step = Math.max(1, Math.round(step));
    const steps = Math.ceil(max / step - 1e-9);
    return Array.from({ length: steps + 1 }, (_, i) => +(i * step).toPrecision(12));
  }

  function logTicks(min, max) {
    if (!(max > 0)) return [1, 10];
    const low = Math.floor(Math.log10(min > 0 && min <= max ? min : max));
    const high = Math.max(low + 1, Math.ceil(Math.log10(max)));
    return Array.from({ length: high - low + 1 }, (_, i) => +(10 ** (low + i)).toPrecision(12));
  }

  const fx = (value) => Math.round(value * 100) / 100;

  function linePath(xs, ys) {
    let path = "";
    let drawing = false;
    ys.forEach((y, i) => {
      if (y == null || !Number.isFinite(y)) {
        drawing = false;
        return;
      }
      path += `${drawing ? "L" : "M"}${fx(xs[i])},${fx(y)}`;
      drawing = true;
    });
    return path;
  }

  function areaPath(xs, tops, bottoms) {
    if (!xs.length) return "";
    const upper = xs.map((x, i) => `${fx(x)},${fx(tops[i])}`);
    const lower = xs.map((x, i) => `${fx(x)},${fx(bottoms[i])}`).reverse();
    return `M${upper.join("L")}L${lower.join("L")}Z`;
  }

  // Rounded data end, square at the baseline.
  function barPath(x, width, base, top, radius = 4) {
    const height = base - top;
    if (!(height > 0) || !(width > 0)) return "";
    const r = Math.min(radius, width / 2, height);
    return `M${fx(x)},${fx(base)}V${fx(top + r)}Q${fx(x)},${fx(top)} ${fx(x + r)},${fx(top)}`
      + `H${fx(x + width - r)}Q${fx(x + width)},${fx(top)} ${fx(x + width)},${fx(top + r)}V${fx(base)}Z`;
  }

  function stack(rows) {
    const sums = [];
    return rows.map((row) => row.map((value, i) => {
      const low = sums[i] || 0;
      sums[i] = low + (value > 0 ? value : 0);
      return [low, sums[i]];
    }));
  }

  function densify(response) {
    const step = response?.bucket === "day" ? DAY : HOUR;
    const buckets = (Array.isArray(response?.buckets) ? response.buckets : []).map(parseTime).filter(Number.isFinite);
    if (!buckets.length) {
      const from = parseTime(response?.from);
      const to = parseTime(response?.to);
      for (let at = floorTo(from, step); at < to && buckets.length < 10000; at += step) buckets.push(at);
    }
    const index = new Map(buckets.map((at, i) => [at, i]));
    const series = (Array.isArray(response?.series) ? response.series : []).map((entry) => {
      const raw = Array.isArray(entry?.points) ? entry.points : [];
      const points = buckets.map(() => normPoint());
      const aligned = raw.length === buckets.length && raw.every((point) => point?.bucket_start == null);
      raw.forEach((point, i) => {
        const at = point?.bucket_start;
        const slot = at != null ? index.get(floorTo(parseTime(at), step)) : aligned ? i : undefined;
        if (slot !== undefined) points[slot] = addPoints(points[slot], normPoint(point));
      });
      return {
        key: entry?.key && typeof entry.key === "object" ? entry.key : {},
        points,
        total: entry?.total ? normPoint(entry.total) : points.reduce((sum, point) => addPoints(sum, point), normPoint()),
      };
    });
    return { step, buckets, series, total: normPoint(response?.total) };
  }

  // Dimension names are part of the id so remembered colors never cross groupings.
  const seriesId = (key, group) => (group.length ? group.map((dim) => `${dim}=${key[dim] ?? ""}`).join(SEP) : "total");

  // The server already caps at top N; anything beyond it joins the server's "other".
  function foldSeries(series, group, max = TOPS.at(-1)) {
    const isOther = (entry) => group.length > 0 && group.every((dim) => entry.key[dim] === "other");
    const named = series.filter((entry) => !isOther(entry));
    const kept = named.slice(0, max).map((entry) => ({ ...entry, id: seriesId(entry.key, group), other: false }));
    const rest = named.slice(max).concat(series.filter(isOther));
    if (rest.length) {
      kept.push({
        key: {},
        id: `${SEP}other`,
        other: true,
        points: rest[0].points.map((_, i) => rest.reduce((sum, entry) => addPoints(sum, entry.points[i]), normPoint())),
        total: rest.reduce((sum, entry) => addPoints(sum, entry.total), normPoint()),
      });
    }
    return kept;
  }

  function hashKey(text) {
    let hash = 0x811c9dc5;
    for (let i = 0; i < text.length; i++) hash = Math.imul(hash ^ text.charCodeAt(i), 0x01000193);
    return hash >>> 0;
  }

  // Colors follow the entity: a remembered slot wins, otherwise the key's hash
  // picks a hue and collisions probe the next hue, solid slots before hatched.
  // Returns the slots and the updated memory, most recently shown last.
  function assignColors(ids, remembered = []) {
    const memory = new Map(remembered);
    const used = new Set();
    const out = new Map();
    for (const id of ids) {
      const slot = memory.get(id);
      if (slot !== undefined && !used.has(slot)) {
        out.set(id, slot);
        used.add(slot);
      }
    }
    for (const id of ids) {
      if (out.has(id)) continue;
      const start = hashKey(id) % HUES;
      for (let step = 0; step < SLOTS; step++) {
        const slot = step - (step % HUES) + ((start + step) % HUES);
        if (used.has(slot)) continue;
        out.set(id, slot);
        used.add(slot);
        break;
      }
    }
    for (const [id, slot] of out) {
      memory.delete(id);
      memory.set(id, slot);
    }
    return { slots: out, memory: [...memory].slice(-MEMORY) };
  }

  // Slot n is hue n % 8 with pattern 0 (solid), 1 (45° hatch, dashed line) or 2 (135° hatch, dotted line).
  function slotStyle(slot) {
    if (!(Number.isInteger(slot) && slot >= 0 && slot < SLOTS)) return { color: "var(--cpa-series-other)", pattern: 0, token: "other" };
    const hue = (slot % HUES) + 1, pattern = Math.floor(slot / HUES);
    return { color: `var(--cpa-series-${hue})`, pattern, token: `${hue}/${pattern}` };
  }

  function csvCell(value) {
    if (typeof value === "number") return Number.isFinite(value) ? String(value) : "";
    let text = value == null ? "" : String(value);
    if (/^[=+\-@\t\r]/.test(text)) text = `'${text}`;
    return /[",\r\n]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text;
  }

  const buildCSV = (rows) => `${rows.map((row) => row.map(csvCell).join(",")).join("\r\n")}\r\n`;

  const decode = (text) => {
    try {
      return decodeURIComponent(text.replace(/\+/g, " "));
    } catch {
      return text;
    }
  };

  function parseHash(hash) {
    const raw = String(hash || "").replace(/^#/, "");
    const at = raw.indexOf("?");
    const params = Object.create(null);
    if (at >= 0) {
      for (const part of raw.slice(at + 1).split("&")) {
        if (!part) continue;
        const eq = part.indexOf("=");
        const name = decode(eq < 0 ? part : part.slice(0, eq));
        if (!(name in params)) params[name] = eq < 0 ? "" : decode(part.slice(eq + 1));
      }
    }
    return { path: (at < 0 ? raw : raw.slice(0, at)) || "/", params };
  }

  function buildHash(path, params) {
    const query = Object.entries(params)
      .filter(([, value]) => value != null && value !== "")
      .map(([name, value]) => `${encodeURIComponent(name)}=${encodeURIComponent(value)}`)
      .join("&");
    return `#${path}${query ? `?${query}` : ""}`;
  }

  // CPAMC stores its session and key names as enc::v2:: + base64(utf8(JSON)
  // xor salt|host) and discards any other format, so only that format is read.
  function decodeStored(stored, host) {
    if (typeof stored !== "string" || !stored.startsWith("enc::v2::")) return null;
    try {
      const salt = new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|${host}`);
      const binary = atob(stored.slice(9));
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i) ^ salt[i % salt.length];
      return JSON.parse(new TextDecoder().decode(bytes));
    } catch {
      return null;
    }
  }

  function decodeAuth(stored, host) {
    const state = decodeStored(stored, host)?.state;
    const key = state?.managementKey;
    return typeof key === "string" && key ? { apiBase: typeof state.apiBase === "string" ? state.apiBase : "", key } : null;
  }

  const SHA_K = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
  ];

  // Plain SHA-256 because crypto.subtle is missing outside secure contexts,
  // such as the panel served over HTTP on a LAN or tailnet address.
  function sha256Hex(text) {
    const bytes = new TextEncoder().encode(text);
    const words = new Uint32Array(((bytes.length + 72) >> 6) << 4);
    bytes.forEach((byte, i) => { words[i >> 2] |= byte << (24 - (i % 4) * 8); });
    words[bytes.length >> 2] |= 0x80 << (24 - (bytes.length % 4) * 8);
    words[words.length - 1] = bytes.length * 8;
    const hash = [0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19];
    const rotr = (x, n) => (x >>> n) | (x << (32 - n));
    const w = new Uint32Array(64);
    for (let at = 0; at < words.length; at += 16) {
      for (let i = 0; i < 64; i++) {
        const a = w[i - 15], b = w[i - 2];
        w[i] = i < 16 ? words[at + i] : (rotr(a, 7) ^ rotr(a, 18) ^ (a >>> 3)) + (rotr(b, 17) ^ rotr(b, 19) ^ (b >>> 10)) + w[i - 7] + w[i - 16];
      }
      let [a, b, c, d, e, f, g, h] = hash;
      for (let i = 0; i < 64; i++) {
        const t1 = (h + (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) + ((e & f) ^ (~e & g)) + SHA_K[i] + w[i]) | 0;
        const t2 = ((rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) + ((a & b) ^ (a & c) ^ (b & c))) | 0;
        [h, g, f, e, d, c, b, a] = [g, f, e, (d + t1) | 0, c, b, a, (t1 + t2) | 0];
      }
      [a, b, c, d, e, f, g, h].forEach((value, i) => { hash[i] = (hash[i] + value) | 0; });
    }
    return hash.map((value) => (value >>> 0).toString(16).padStart(8, "0")).join("");
  }

  // CPAMC keeps API key names only in this browser, keyed by
  // sha256(JSON(["api-key-name", apiBase, key])). Usage rows carry sha256(key),
  // so both are derived from the configured keys to map one onto the other.
  function matchKeyNames(names, apiBase, keys) {
    const out = new Map();
    if (!names || typeof names !== "object" || !Array.isArray(keys)) return out;
    for (const key of keys) {
      const text = typeof key === "string" ? key.trim() : "";
      const name = text && names[sha256Hex(JSON.stringify(["api-key-name", apiBase, text]))];
      if (typeof name === "string" && name.trim()) out.set(sha256Hex(text), name.trim());
    }
    return out;
  }

  function managementBase(apiBase, origin) {
    let base = String(apiBase || "").trim().replace(/\/+$/, "").replace(/\/v8\/management$/i, "");
    if (!base) base = origin;
    else if (!/^https?:\/\//i.test(base)) base = `http://${base}`;
    return `${base}/v8/management`;
  }

  function baseFromUrl(url, href) {
    try {
      const parsed = new URL(String(url), href);
      const at = parsed.pathname.indexOf("/v8/management");
      const rest = at < 0 ? "" : parsed.pathname.slice(at + 14);
      return at < 0 || (rest && rest[0] !== "/") ? "" : parsed.origin + parsed.pathname.slice(0, at + 14);
    } catch {
      return "";
    }
  }

  function keyFromHeader(name, value) {
    const header = String(name || "").toLowerCase();
    const text = String(value ?? "").trim();
    if (header === "authorization") return /^Bearer\s+(\S.*)$/i.exec(text)?.[1].trim() || "";
    return header === "x-management-key" ? text : "";
  }

  const DEFAULT_VIEW = {
    range: "24h", from: 0, to: 0, bucket: "hour", metric: "tokens", group: ["provider"], type: "stacked",
    scale: "linear", top: 10, filters: {}, table: false, auto: false, tab: "provider", colors: [],
  };

  function sanitizeView(raw) {
    const view = { ...DEFAULT_VIEW, group: DEFAULT_VIEW.group.slice(), filters: {}, colors: [] };
    if (!raw || typeof raw !== "object") return view;
    if (Object.hasOwn(RANGES, raw.range) || raw.range === "custom") view.range = raw.range;
    if (Number.isFinite(raw.from) && Number.isFinite(raw.to) && raw.from < raw.to) Object.assign(view, { from: raw.from, to: raw.to });
    if (view.range === "custom" && !view.to) view.range = DEFAULT_VIEW.range;
    if (raw.bucket === "day") view.bucket = "day";
    if (Object.hasOwn(METRICS, raw.metric)) view.metric = raw.metric;
    if (Array.isArray(raw.group)) view.group = [...new Set(raw.group.filter((dim) => DIMS.includes(dim)))].slice(0, 2);
    if (TYPES.includes(raw.type)) view.type = raw.type;
    if (raw.scale === "log") view.scale = "log";
    if (TOPS.includes(raw.top)) view.top = raw.top;
    for (const dim of DIMS) {
      const values = Array.isArray(raw.filters?.[dim]) ? raw.filters[dim].filter((value) => typeof value === "string") : [];
      if (values.length) view.filters[dim] = [...new Set(values)];
    }
    view.table = raw.table === true;
    view.auto = raw.auto === true;
    if (DIMS.includes(raw.tab)) view.tab = raw.tab;
    if (Array.isArray(raw.colors)) {
      const colors = new Map();
      for (const pair of raw.colors) {
        if (!Array.isArray(pair) || typeof pair[0] !== "string" || !(Number.isInteger(pair[1]) && pair[1] >= 0 && pair[1] < SLOTS)) continue;
        colors.delete(pair[0]);
        colors.set(pair[0], pair[1]);
      }
      view.colors = [...colors].slice(-MEMORY);
    }
    return view;
  }

  const STRINGS = {
    en: {
      nav: "Usage", range: "Time range", custom: "Custom", from: "From", to: "To", apply: "Apply", bucket: "Bucket",
      hour: "Hourly", day: "Daily", refresh: "Refresh", auto: "Auto-refresh", filters: "Filters", clear: "Clear filters",
      removeFilter: "Remove filter {0}", m_requests: "Requests", m_failures: "Failures", m_tokens: "Total tokens",
      m_input: "Input tokens", m_output: "Output tokens", m_cache_read: "Cache read", m_cache_write: "Cache write",
      m_reasoning: "Reasoning tokens", m_latency: "Avg latency", m_ttft: "Avg TTFT", m_cost: "Cost",
      d_provider: "Provider", d_model: "Model", d_requested_model: "Requested model", d_account: "Account",
      d_api_key: "API key", d_reasoning_effort: "Reasoning effort", d_service_tier: "Service tier", d_stream: "Stream",
      metric: "Metric", groupBy: "Group by", thenBy: "then", none: "None", chart: "Chart", t_stacked: "Stacked bars",
      t_grouped: "Grouped bars", t_lines: "Lines", t_area: "Stacked area", scale: "Scale", linear: "Linear", log: "Log",
      top: "Top", table: "Table", csv: "Export CSV", noStack: "Averages can't be stacked",
      legendHint: "Click to hide or show. Double-click to show only this.", total: "Total", overall: "Overall",
      other: "Other", all: "All requests", empty: "(none)", noData: "No usage recorded in this range.",
      localTime: "Local time ({0})", utcDays: "Days (UTC)", time: "Time", chartBy: "{0} by {1}", inOut: "Input / output",
      failRate: "{0} of requests", cachedSub: "{0} cache read", reasoningSub: "{0} reasoning", maxSub: "Max {0}",
      ttftSub: "{0} measured", cost: "Estimated cost", noPrices: "No prices set", breakdown: "Breakdown", prices: "Prices",
      unpriced: "unpriced", unpricedModel: "1 model unpriced", unpricedModels: "{0} models unpriced",
      pricesHint: "USD per 1M tokens. Reasoning output uses the output price. A \"default\" row prices every other model.",
      model: "Model", p_input: "Input", p_cache_read: "Cache read", p_cache_write: "Cache write", p_output: "Output",
      addModel: "Add model", remove: "Remove", savePrices: "Save prices", saving: "Saving…", saved: "Prices saved.",
      priceInvalid: "Prices must be numbers of 0 or more.", priceModel: "Every row needs a model name.",
      priceDuplicate: "{0} is listed twice.",
      disabled: "Usage tracking is off. Set observability.usage.tracker.enabled to true in config.yaml, then refresh.",
      nokey: "Sign in to the panel first. This page uses the management key from your current session.",
      unauthorized: "The server rejected the management key. Sign in again.", network: "Couldn't reach the server.",
      http: "The request failed (HTTP {0}). {1}", retry: "Retry", loading: "Loading…", updated: "Updated {0}",
      chartLabel: "{0}, {1} buckets. Arrow keys read values.",
    },
    "zh-CN": {
      nav: "用量", range: "时间范围", custom: "自定义", from: "开始", to: "结束", apply: "应用", bucket: "时间粒度",
      hour: "按小时", day: "按天", refresh: "刷新", auto: "自动刷新", filters: "筛选", clear: "清除筛选",
      removeFilter: "移除筛选 {0}", m_requests: "请求数", m_failures: "失败数", m_tokens: "Token 总量",
      m_input: "输入 Token", m_output: "输出 Token", m_cache_read: "缓存读取", m_cache_write: "缓存写入",
      m_reasoning: "推理 Token", m_latency: "平均延迟", m_ttft: "平均首字延迟", m_cost: "费用",
      d_provider: "提供商", d_model: "模型", d_requested_model: "请求模型", d_account: "账号",
      d_api_key: "API 密钥", d_reasoning_effort: "推理强度", d_service_tier: "服务层级", d_stream: "流式",
      metric: "指标", groupBy: "分组", thenBy: "再按", none: "无", chart: "图表", t_stacked: "堆叠柱状",
      t_grouped: "分组柱状", t_lines: "折线", t_area: "堆叠面积", scale: "刻度", linear: "线性", log: "对数",
      top: "前", table: "表格", csv: "导出 CSV", noStack: "平均值不能堆叠",
      legendHint: "单击显示或隐藏，双击仅显示此项。", total: "合计", overall: "整体",
      other: "其他", all: "全部请求", empty: "（空）", noData: "该时间范围内没有用量记录。",
      localTime: "本地时间（{0}）", utcDays: "日期（UTC）", time: "时间", chartBy: "{0}（按{1}）", inOut: "输入 / 输出",
      failRate: "占请求的 {0}", cachedSub: "缓存读取 {0}", reasoningSub: "推理 {0}", maxSub: "最大 {0}",
      ttftSub: "已测量 {0}", cost: "预估费用", noPrices: "未设置价格", breakdown: "明细", prices: "价格",
      unpriced: "未定价", unpricedModel: "1 个模型未定价", unpricedModels: "{0} 个模型未定价",
      pricesHint: "单位：美元 / 百万 Token。推理输出按输出价格计费。\"default\" 行用于其余所有模型。",
      model: "模型", p_input: "输入", p_cache_read: "缓存读取", p_cache_write: "缓存写入", p_output: "输出",
      addModel: "添加模型", remove: "删除", savePrices: "保存价格", saving: "保存中…", saved: "价格已保存。",
      priceInvalid: "价格必须是大于或等于 0 的数字。", priceModel: "每一行都需要填写模型名称。",
      priceDuplicate: "{0} 重复出现。",
      disabled: "用量统计未启用。请在 config.yaml 中将 observability.usage.tracker.enabled 设为 true，然后刷新。",
      nokey: "请先登录面板。此页面使用当前会话的管理密钥。",
      unauthorized: "服务器拒绝了管理密钥，请重新登录。", network: "无法连接到服务器。",
      http: "请求失败（HTTP {0}）。{1}", retry: "重试", loading: "加载中…", updated: "更新于 {0}",
      chartLabel: "{0}，{1} 个时间段。方向键读取数值。",
    },
  };

  const translate = (lang, key, ...args) => ((STRINGS[lang] || STRINGS.en)[key] ?? STRINGS.en[key] ?? key)
    .replace(/\{(\d)\}/g, (_, i) => args[i] ?? "");

  // Same lookup as the server: an exact entry, else the "default" entry.
  const isPriced = (prices, model) => Object.hasOwn(prices, model) || Object.hasOwn(prices, "default");

  // cost_usd is 0 for both a zero price and a missing one, so only the price map
  // tells them apart. Without a loaded map (null) nothing is claimed either way.
  const unpricedLabel = (prices, model, tr) => (prices && !isPriced(prices, String(model ?? "")) ? tr("unpriced") : "");

  function costSub(prices, summary, tr) {
    if (!prices) return "";
    const rows = Array.isArray(summary?.by?.model) ? summary.by.model : [];
    const count = rows.filter((row) => row && typeof row === "object" && unpricedLabel(prices, row.value, tr)).length;
    if (count) return count === 1 ? tr("unpricedModel") : tr("unpricedModels", count);
    return Object.keys(prices).length ? "" : tr("noPrices");
  }

  const harness = globalThis.__cpaUsageHarness;
  if (harness && typeof harness === "object") {
    Object.assign(harness, {
      compact, duration, usd, formatValue, parseTime, bucketRange, utcOffsetLabel, tickIndexes, linearScale, logScale,
      niceTicks, logTicks, linePath, areaPath, barPath, stack, densify, foldSeries, seriesId, hashKey, assignColors,
      slotStyle, csvCell, buildCSV, parseHash, buildHash, decodeStored, decodeAuth, sha256Hex, matchKeyNames, managementBase, baseFromUrl, keyFromHeader,
      sanitizeView, translate, isPriced, unpricedLabel, costSub, METRICS, SLOTS, MEMORY,
    });
    return;
  }

  // The panel keeps its management key in memory unless "remember" is on, so
  // the key is read from the panel's own requests. It lives only in this closure.
  let captured = null;
  let onCapture = () => {};
  const nativeFetch = typeof window.fetch === "function" ? window.fetch.bind(window) : null;

  function remember(url, name, value) {
    const key = keyFromHeader(name, value);
    const base = key ? baseFromUrl(url, location.href) : "";
    if (!base || (captured && captured.key === key && captured.base === base)) return;
    captured = { base, key };
    onCapture();
  }

  try {
    const proto = window.XMLHttpRequest?.prototype;
    if (proto) {
      const urls = new WeakMap();
      const { open, setRequestHeader } = proto;
      proto.open = function (method, url) {
        try { urls.set(this, url); } catch { /* Never interfere with the request. */ }
        return open.apply(this, arguments);
      };
      proto.setRequestHeader = function (name, value) {
        try { remember(urls.get(this), name, value); } catch { /* Never interfere with the request. */ }
        return setRequestHeader.apply(this, arguments);
      };
    }
    if (nativeFetch) {
      window.fetch = function (input, init) {
        try {
          const url = typeof input === "string" || input instanceof URL ? String(input) : input?.url;
          const headers = new Headers(init?.headers ?? (input instanceof Request ? input.headers : undefined));
          for (const name of ["authorization", "x-management-key"]) remember(url, name, headers.get(name));
        } catch { /* Never interfere with the request. */ }
        return nativeFetch.apply(this, arguments);
      };
    }
  } catch { /* Without the hook the stored session is still tried. */ }

  const root = document.documentElement;
  const t = (key, ...args) => translate(root.lang, key, ...args);

  const hidden = new Set();
  const formats = {};
  let view = sanitizeView(readStored());
  let page = null, ui = null, data = null, error = null, loading = false, prices, priceRows = [];
  let loadSeq = 0, lastAttempt = 0, controller = null, autoTimer = 0, resizer = null;
  let pendingNav = 0, hostPath = "/dashboard", hoverIndex = -1;

  function readStored() {
    try { return JSON.parse(localStorage.getItem("cpa-usage-view")); } catch { return null; }
  }

  const saveView = () => {
    try { localStorage.setItem("cpa-usage-view", JSON.stringify(view)); } catch { /* Optional persistence. */ }
  };

  function el(tag, props, ...kids) {
    const node = document.createElement(tag);
    for (const [name, value] of Object.entries(props || {})) {
      if (value == null || value === false) continue;
      if (name === "class") node.className = value;
      else if (name === "text") node.textContent = value;
      else if (name.startsWith("on")) node.addEventListener(name.slice(2), value);
      else node.setAttribute(name, value === true ? "" : value);
    }
    node.append(...kids.flat().filter((kid) => kid != null && kid !== false));
    return node;
  }

  function svg(tag, attrs) {
    const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [name, value] of Object.entries(attrs || {})) if (value != null) node.setAttribute(name, value);
    return node;
  }

  const paint = (node, color) => (node.style.setProperty("--c", color), node);
  const button = (text, onclick, cls = "cpa-u-btn") => el("button", { type: "button", class: cls, text, onclick });
  const fmt = (name, options) => (formats[root.lang + name] ||= new Intl.DateTimeFormat(root.lang || undefined, options));
  const hm = (at) => fmt("hm", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(at);
  const md = (at, utc) => fmt(utc ? "mdu" : "md", { month: "short", day: "numeric", timeZone: utc ? "UTC" : undefined }).format(at);
  const toLocalInput = (ms) => new Date(ms - new Date(ms).getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  const bucketLabel = (at) => (data.series.step === DAY ? `${md(at, true)} (UTC)` : `${md(at)}, ${hm(at)}–${hm(at + HOUR)}`);

  function currentRange() {
    const step = view.bucket === "day" ? DAY : HOUR;
    if (view.range === "custom" && view.from < view.to) return { from: floorTo(view.from, step), to: Math.ceil(view.to / step) * step };
    return bucketRange(Date.now(), RANGES[view.range] || DAY, step);
  }

  function credentials() {
    if (captured) return captured;
    let stored = null;
    try { stored = decodeAuth(localStorage.getItem("cli-proxy-auth"), location.host); } catch { /* No stored session. */ }
    return stored && { base: managementBase(stored.apiBase, location.origin), key: stored.key };
  }

  async function request(path, { method = "GET", body, signal } = {}) {
    const auth = credentials();
    if (!auth || !nativeFetch) throw { kind: "nokey" };
    let response;
    try {
      response = await nativeFetch(`${auth.base}/${path}`, {
        method, signal, cache: "no-store", body: body && JSON.stringify(body),
        headers: { Authorization: `Bearer ${auth.key}`, ...(body && { "Content-Type": "application/json" }) },
      });
    } catch (cause) {
      throw cause?.name === "AbortError" ? cause : { kind: "network" };
    }
    if (response.status === 503) throw { kind: "disabled" };
    if (response.status === 401) throw { kind: "unauthorized" };
    const json = await response.json().catch(() => null);
    if (!response.ok) throw { kind: "http", status: response.status, detail: typeof json?.error === "string" ? json.error : "" };
    return json;
  }

  const api = (path, options) => request(`observability/usage/${path}`, options);

  // Names are optional: without any, the configured keys are not fetched, and
  // any failure falls back to the masked hints.
  async function loadKeyNames(signal) {
    try {
      const apiBase = decodeStored(localStorage.getItem("cli-proxy-auth"), location.host)?.state?.apiBase;
      if (typeof apiBase !== "string") return new Map();
      const names = decodeStored(localStorage.getItem(`api-key-names:v1:${apiBase}`), location.host);
      if (!names || typeof names !== "object" || !Object.keys(names).length) return new Map();
      return matchKeyNames(names, apiBase, await request("config/access/api-keys", { signal }));
    } catch {
      return new Map();
    }
  }

  function query(range, filtered) {
    const parts = [["from", range.from / 1000], ["to", range.to / 1000]];
    if (filtered) for (const dim of DIMS) for (const value of view.filters[dim] || []) parts.push([`filter.${dim}`, value]);
    return parts.map(([name, value]) => `${encodeURIComponent(name)}=${encodeURIComponent(value)}`).join("&");
  }

  async function load() {
    if (!page) return;
    const seq = ++loadSeq, range = currentRange(), group = view.group.slice(), bucket = view.bucket;
    lastAttempt = Date.now();
    controller?.abort();
    const { signal } = (controller = new AbortController());
    loading = true;
    renderState();
    try {
      const filtered = query(range, true), get = (path) => api(path, { signal });
      const [series, summary, dims, names] = await Promise.all([
        get(`series?${filtered}&bucket=${bucket}&limit=${view.top}${group.length ? `&group=${group}` : ""}`),
        get(`summary?${filtered}`), get(`dimensions?${query(range)}`), loadKeyNames(signal)]);
      if (seq !== loadSeq) return;
      const hints = new Map();
      for (const entry of [dims?.api_key, summary?.by?.api_key].flatMap((list) => (Array.isArray(list) ? list : []))) {
        if (entry?.value && entry.hint) hints.set(entry.value, entry.hint);
      }
      data = { series: densify({ bucket, ...range, ...series }), summary: summary || {}, dims: dims || {}, group, hints, names, at: Date.now() };
      error = null;
      if (prices === undefined) loadPrices();
    } catch (failure) {
      if (seq !== loadSeq || failure?.name === "AbortError") return;
      error = failure?.kind ? failure : { kind: "network" };
    }
    loading = false;
    render();
  }

  async function loadPrices() {
    prices = null;
    try {
      const body = await api("prices");
      if (body?.prices && typeof body.prices === "object") prices = body.prices;
    } catch { /* The editor stays hidden without a price map. */ }
    priceRows = Object.entries(prices || {}).map(([model, entry]) => ({
      model, ...Object.fromEntries(PRICE_FIELDS.map((key) => [key, String(num(entry?.[key]))])),
    }));
    renderPrices();
    if (ui && data) render();
  }

  const message = (failure) => (failure.kind === "http" ? t("http", failure.status, failure.detail) : t(failure.kind));

  function valueLabel(dim, value) {
    if (value == null || value === "") return t("empty");
    if (dim !== "api_key") return String(value);
    return data?.names.get(value) || data?.hints.get(value) || `${String(value).slice(0, 8)}…`;
  }

  // A name is local to this browser, so hovering it still shows the masked key.
  function valueTitle(dim, value) {
    const label = valueLabel(dim, value), hint = dim === "api_key" && data?.names.has(value) && data.hints.get(value);
    return hint ? `${label} (${hint})` : label;
  }

  const seriesLabel = (entry, group) => (entry.other || (group.length && group.every((dim) => entry.key[dim] === "other"))
    ? t("other") : group.length ? group.map((dim) => valueLabel(dim, entry.key[dim])).join(" / ") : t("all"));

  function set(patch, reload) {
    Object.assign(view, patch);
    saveView();
    mirrorHash();
    if (reload) load();
    render();
  }

  function mirrorHash() {
    const { path, params } = parseHash(location.hash);
    if (params["cpa-page"] !== "usage") return;
    const custom = view.range === "custom";
    const hash = buildHash(path, { ...params, "cpa-range": view.range,
      "cpa-from": custom ? Math.floor(view.from / 1000) : "", "cpa-to": custom ? Math.floor(view.to / 1000) : "" });
    if (hash !== location.hash) history.replaceState(history.state, "", hash);
  }

  function adoptHash(params) {
    const range = params["cpa-range"], from = params["cpa-from"] * 1000, to = params["cpa-to"] * 1000;
    const next = range === "custom" ? (from < to ? { range, from, to } : null) : Object.hasOwn(RANGES, range) ? { range } : null;
    if (!next || Object.keys(next).every((key) => view[key] === next[key])) return false;
    Object.assign(view, next);
    return true;
  }

  const segment = (label, items, onPick) => el("div", { class: "cpa-u-seg", role: "group", "aria-label": label },
    items.map(([value, text]) => el("button", { type: "button", "data-value": value, text, onclick: () => onPick(value) })));
  const syncSegment = (group, value) => {
    for (const node of group.children) node.setAttribute("aria-pressed", String(node.dataset.value === value));
  };
  const select = (items, onChange) => el("select", { onchange: (event) => onChange(event.target.value) },
    items.map(([value, text]) => el("option", { value, text })));
  const field = (label, control, tag = "label") => el(tag, { class: "cpa-u-field" }, el("span", { text: label, "aria-hidden": tag === "label" ? null : "true" }), control);

  function buildPage() {
    ui = {};
    ui.range = segment(t("range"), [...Object.keys(RANGES).map((range) => [range, range]), ["custom", t("custom")]], (range) => {
      if (range !== "custom") return set({ range }, true);
      const current = currentRange();
      set({ range, from: view.from || current.from, to: view.to || current.to });
      ui.from.focus();
    });
    ui.from = el("input", { type: "datetime-local" });
    ui.to = el("input", { type: "datetime-local" });
    ui.custom = el("div", { class: "cpa-u-custom" }, field(t("from"), ui.from), field(t("to"), ui.to), button(t("apply"), () => {
      const from = new Date(ui.from.value).getTime(), to = new Date(ui.to.value).getTime();
      if (from < to) set({ range: "custom", from, to }, true);
    }));
    ui.bucket = segment(t("bucket"), [["hour", t("hour")], ["day", t("day")]], (bucket) => set({ bucket }, true));
    ui.auto = el("input", { type: "checkbox", onchange: () => {
      view.auto = ui.auto.checked;
      saveView();
      syncAuto();
    } });
    ui.filters = el("div", { class: "cpa-u-filters", role: "group", "aria-label": t("filters") });
    ui.status = el("div", { class: "cpa-u-status", role: "status", "aria-live": "polite" });
    ui.tiles = el("div", { class: "cpa-u-tiles" });

    const dims = [["", t("none")], ...DIMS.map((dim) => [dim, t(`d_${dim}`)])];
    const setGroup = () => {
      hidden.clear();
      set({ group: [...new Set([ui.group1.value, ui.group1.value && ui.group2.value].filter(Boolean))] }, true);
    };
    ui.metric = select(Object.keys(METRICS).map((metric) => [metric, t(`m_${metric}`)]), (metric) => set({ metric }));
    ui.group1 = select(dims, setGroup);
    ui.group2 = select(dims, setGroup);
    ui.type = segment(t("chart"), TYPES.map((type) => [type, t(`t_${type}`)]), (type) => set({ type }));
    ui.scale = segment(t("scale"), [["linear", t("linear")], ["log", t("log")]], (scale) => set({ scale }));
    ui.top = select(TOPS.map((top) => [top, top]), (top) => set({ top: Number(top) }, true));
    ui.tableToggle = button(t("table"), () => set({ table: !view.table }));
    ui.chartTitle = el("h2", { id: "cpa-usage-chart-title" });
    ui.legend = el("div", { class: "cpa-u-legend" });
    ui.plot = el("div", { class: "cpa-u-plot" });
    ui.tip = el("div", { class: "cpa-u-tip", hidden: true });
    ui.table = el("div", { class: "cpa-u-tablewrap", tabindex: "0", role: "region", "aria-labelledby": "cpa-usage-chart-title" });
    ui.chartCard = el("section", { class: "cpa-u-card", "aria-labelledby": "cpa-usage-chart-title" }, ui.chartTitle,
      el("div", { class: "cpa-u-controls" }, field(t("metric"), ui.metric), field(t("groupBy"), ui.group1),
        field(t("thenBy"), ui.group2), field(t("chart"), ui.type, "div"), field(t("scale"), ui.scale, "div"),
        field(t("top"), ui.top), el("div", { class: "cpa-u-actions" }, ui.tableToggle, button(t("csv"), exportCSV))),
      ui.legend, ui.plot, ui.table);

    ui.tabs = el("div", { class: "cpa-u-tabs", role: "tablist", "aria-label": t("breakdown"), onkeydown: (event) => {
      const tabs = [...ui.tabs.children], at = tabs.indexOf(document.activeElement);
      const move = { ArrowLeft: -1, ArrowRight: 1 }[event.key];
      if (at < 0 || !move) return;
      event.preventDefault();
      tabs[(at + move + tabs.length) % tabs.length].click();
    } });
    ui.breakdown = el("div", { class: "cpa-u-tablewrap", role: "tabpanel", tabindex: "0" });
    ui.breakdownCard = el("section", { class: "cpa-u-card", "aria-labelledby": "cpa-usage-breakdown-title" },
      el("h2", { id: "cpa-usage-breakdown-title", text: t("breakdown") }), ui.tabs, ui.breakdown);

    ui.models = el("datalist", { id: "cpa-usage-models" });
    ui.priceBody = el("tbody");
    ui.priceStatus = el("span", { class: "cpa-u-muted", role: "status" });
    ui.pricesCard = el("section", { class: "cpa-u-card", "aria-labelledby": "cpa-usage-prices-title", hidden: true },
      el("h2", { id: "cpa-usage-prices-title", text: t("prices") }), el("p", { class: "cpa-u-muted", text: t("pricesHint") }),
      el("div", { class: "cpa-u-tablewrap" }, el("table", { class: "cpa-u-table cpa-u-prices" },
        el("thead", {}, el("tr", {}, ["model", ...PRICE_FIELDS.map((key) => `p_${key}`)].map((key) => el("th", { scope: "col", text: t(key) })),
          el("th", {}, el("span", { class: "cpa-u-sr", text: t("remove") })))), ui.priceBody)),
      el("div", { class: "cpa-u-actions" }, button(t("addModel"), () => {
        priceRows.push({ model: "", input: "0", cache_read: "0", cache_write: "0", output: "0" });
        renderPrices();
        ui.priceBody.lastElementChild?.querySelector("input")?.focus();
      }), button(t("savePrices"), savePrices, "cpa-u-btn cpa-u-primary"), ui.priceStatus), ui.models);

    let lastWidth = 0;
    resizer = new ResizeObserver(() => {
      const width = Math.round(ui?.plot.clientWidth || 0);
      if (width && width !== lastWidth && data && !view.table) renderChart();
      lastWidth = width || lastWidth;
    });
    resizer.observe(ui.plot);
    return el("section", { class: "cpa-u", "data-cpa-owned": true, "data-cpa-page-root": true, "data-lang": root.lang, "aria-labelledby": "cpa-usage-title" },
      el("header", { class: "cpa-u-head" }, el("h1", { id: "cpa-usage-title", text: t("nav") }),
        el("div", { class: "cpa-u-toolbar" }, ui.range, ui.custom, ui.bucket, button(t("refresh"), () => load()),
          el("label", { class: "cpa-u-check" }, ui.auto, el("span", { text: t("auto") })))),
      ui.filters, ui.status, ui.tiles, ui.chartCard, ui.breakdownCard, ui.pricesCard);
  }

  const effectiveType = (metric) => (!metric.avg ? view.type : { stacked: "grouped", area: "lines" }[view.type] || view.type);

  function render() {
    if (!ui) return;
    renderControls();
    renderState();
    if (!data) return;
    renderFilters();
    renderTiles();
    renderChart();
    renderBreakdown();
    const models = (Array.isArray(data.dims.model) ? data.dims.model : []).filter((model) => typeof model === "string");
    if (ui.models.dataset.models !== models.join(SEP)) {
      ui.models.dataset.models = models.join(SEP);
      ui.models.replaceChildren(...models.map((value) => el("option", { value })));
    }
  }

  function renderControls() {
    const metric = METRICS[view.metric];
    syncSegment(ui.range, view.range);
    ui.custom.hidden = view.range !== "custom";
    for (const [input, value] of [[ui.from, view.from], [ui.to, view.to]]) {
      if (view.range === "custom" && value && document.activeElement !== input) input.value = toLocalInput(value);
    }
    syncSegment(ui.bucket, view.bucket);
    syncSegment(ui.type, effectiveType(metric));
    syncSegment(ui.scale, view.scale);
    for (const node of ui.type.children) {
      node.disabled = !!metric.avg && (node.dataset.value === "stacked" || node.dataset.value === "area");
      node.title = node.disabled ? t("noStack") : "";
    }
    ui.auto.checked = view.auto;
    ui.metric.value = view.metric;
    ui.group1.value = view.group[0] || "";
    ui.group2.value = view.group[1] || "";
    ui.group2.disabled = !view.group[0];
    ui.top.value = String(view.top);
    ui.tableToggle.setAttribute("aria-pressed", String(view.table));
    const groups = (data?.group || view.group).map((dim) => t(`d_${dim}`)).join(" / ");
    ui.chartTitle.textContent = groups ? t("chartBy", t(`m_${view.metric}`), groups) : t(`m_${view.metric}`);
  }

  function renderState() {
    if (!ui) return;
    ui.plot.classList.toggle("is-loading", loading);
    const blocking = !!error && ["disabled", "nokey", "unauthorized"].includes(error.kind);
    const text = error ? message(error) : loading && !data ? t("loading") : data ? t("updated", fmt("time", { timeStyle: "medium" }).format(data.at)) : "";
    ui.status.replaceChildren(el("p", { class: error && !blocking ? "cpa-u-error" : blocking ? "" : "cpa-u-muted", text }),
      ...(error && !blocking ? [button(t("retry"), () => load())] : []));
    ui.status.classList.toggle("is-blocking", blocking);
    for (const node of [ui.filters, ui.tiles, ui.chartCard, ui.breakdownCard]) node.hidden = !data || blocking;
    ui.pricesCard.hidden = !data || blocking || !prices;
  }

  function setFilter(dim, value, on) {
    const values = new Set(view.filters[dim]);
    values[on ? "add" : "delete"](value);
    const filters = { ...view.filters, [dim]: [...values] };
    if (!values.size) delete filters[dim];
    set({ filters }, true);
  }

  function renderFilters() {
    const open = new Set([...ui.filters.querySelectorAll("details[open]")].map((node) => node.dataset.dim));
    const focus = ui.filters.contains(document.activeElement) && document.activeElement.dataset.focus;
    const nodes = [];
    for (const dim of DIMS) {
      const listed = (Array.isArray(data.dims[dim]) ? data.dims[dim] : []).map((entry) => (typeof entry === "string" ? entry : entry?.value));
      const chosen = view.filters[dim] || [];
      const values = [...new Set([...listed.filter((value) => typeof value === "string"), ...chosen])];
      if (!values.length) continue;
      nodes.push(el("details", { class: "cpa-u-filter", "data-dim": dim, open: open.has(dim) },
        el("summary", { "data-focus": dim, text: `${t(`d_${dim}`)}${chosen.length ? ` (${chosen.length})` : ""}` }),
        el("div", { class: "cpa-u-menu" }, values.map((value) => {
          const box = el("input", { type: "checkbox", "data-focus": dim + SEP + value, onchange: () => setFilter(dim, value, box.checked) });
          box.checked = chosen.includes(value);
          return el("label", { title: valueTitle(dim, value) }, box, el("span", { text: valueLabel(dim, value) }));
        }))));
    }
    const chips = DIMS.flatMap((dim) => (view.filters[dim] || []).map((value) => {
      const label = `${t(`d_${dim}`)}: ${valueLabel(dim, value)}`;
      return el("button", { type: "button", class: "cpa-u-chip", "aria-label": t("removeFilter", label), onclick: () => setFilter(dim, value, false) },
        el("span", { text: label }), el("span", { "aria-hidden": "true", text: "×" }));
    }));
    if (chips.length) nodes.push(...chips, button(t("clear"), () => set({ filters: {} }, true), "cpa-u-link"));
    ui.filters.replaceChildren(...(nodes.length ? [el("span", { class: "cpa-u-muted", text: t("filters") }), ...nodes] : []));
    if (focus) [...ui.filters.querySelectorAll("[data-focus]")].find((node) => node.dataset.focus === focus)?.focus();
  }

  function renderTiles() {
    const p = normPoint(data.summary.total);
    const tile = (label, value, sub) => el("div", { class: "cpa-u-tile" }, el("span", { text: label }), el("strong", { text: value }), sub ? el("small", { text: sub }) : null);
    ui.tiles.replaceChildren(
      tile(t("m_requests"), compact(p.requests)),
      tile(t("m_failures"), compact(p.failures), t("failRate", p.requests ? `${round((p.failures / p.requests) * 100, 1)}%` : "—")),
      tile(t("m_tokens"), compact(p.tokens.total), t("cachedSub", compact(p.tokens.input_cache_read))),
      tile(t("inOut"), `${compact(METRICS.input.get(p))} / ${compact(METRICS.output.get(p))}`, t("reasoningSub", compact(p.tokens.output_reasoning))),
      tile(t("m_latency"), duration(METRICS.latency.get(p)), p.requests && t("maxSub", duration(p.latency_ms_max))),
      tile(t("m_ttft"), duration(METRICS.ttft.get(p)), p.ttft_count && t("ttftSub", compact(p.ttft_count))),
      tile(t("cost"), usd(p.cost_usd), costSub(prices, data.summary, t)));
  }

  function renderChart() {
    const metric = METRICS[view.metric], group = data.group, list = foldSeries(data.series.series, group, view.top);
    const { slots, memory } = assignColors(list.filter((entry) => !entry.other).map((entry) => entry.id), view.colors);
    if (JSON.stringify(memory) !== JSON.stringify(view.colors)) {
      view.colors = memory;
      saveView();
    }
    const costByModel = view.metric === "cost" && group.includes("model");
    for (const entry of list) {
      Object.assign(entry, slotStyle(entry.other ? -1 : slots.get(entry.id)));
      entry.label = seriesLabel(entry, group);
      entry.note = costByModel && !entry.other ? unpricedLabel(prices, entry.key.model, t) : "";
      entry.values = entry.points.map(metric.get);
    }
    const model = { metric, list, type: effectiveType(metric), visible: list.filter((entry) => !hidden.has(entry.id)) };
    renderLegend(model);
    ui.plot.hidden = view.table;
    ui.table.hidden = !view.table;
    if (view.table) renderTable(metric);
    else drawChart(model);
  }

  function renderLegend({ list, type }) {
    const signature = type + list.map((entry) => SEP + entry.id + entry.token + entry.label + entry.note).join("");
    if (ui.legend.dataset.signature !== signature) {
      ui.legend.dataset.signature = signature;
      ui.legend.replaceChildren(...list.map((entry) => paint(el("button", {
        type: "button", class: "cpa-u-key", "data-id": entry.id, title: t("legendHint"),
        onclick: (event) => {
          if (event.detail > 1) return;
          if (event.shiftKey || event.altKey) solo(entry.id, list);
          else if (!hidden.delete(entry.id)) hidden.add(entry.id);
          renderChart();
        },
        ondblclick: () => {
          solo(entry.id, list);
          renderChart();
        },
      }, el("i", { class: [type === "lines" && "is-line", entry.pattern && `is-p${entry.pattern}`].filter(Boolean).join(" ") || null }),
      el("span", { text: entry.label }), entry.note && el("small", { class: "cpa-u-unpriced", text: entry.note })), entry.color)));
    }
    ui.legend.hidden = list.length < 2 || view.table;
    for (const node of ui.legend.children) node.setAttribute("aria-pressed", String(!hidden.has(node.dataset.id)));
  }

  function solo(id, list) {
    const others = list.filter((entry) => entry.id !== id);
    const alone = !hidden.has(id) && others.every((entry) => hidden.has(entry.id));
    // The first click of a double-click has already hidden the target.
    const none = list.every((entry) => hidden.has(entry.id));
    hidden.clear();
    if (!alone && !none) for (const entry of others) hidden.add(entry.id);
  }

  function drawChart({ metric, list, visible, type }) {
    const { buckets, step } = data.series, n = buckets.length, refocus = ui.plot.contains(document.activeElement);
    if (!n || list.every((entry) => entry.total.requests === 0)) return ui.plot.replaceChildren(el("p", { class: "cpa-u-empty", text: t("noData") }));
    const width = Math.max(320, Math.round(ui.plot.clientWidth || 720)), height = width < 560 ? 240 : 300;
    const day = step === DAY, log = view.scale === "log", bars = type === "stacked" || type === "grouped";
    const stacked = type === "stacked" || type === "area";
    const layers = stack(visible.map((entry) => entry.values));
    const values = stacked ? layers.at(-1)?.map(([, high]) => high) || [] : visible.flatMap((entry) => entry.values);
    const positive = (stacked ? values.concat(visible.flatMap((entry) => entry.values)) : values).filter((value) => value > 0);
    const max = Math.max(0, ...values.filter((value) => value > 0));
    const ticks = log ? logTicks(Math.min(...positive), max) : niceTicks(max, 5, !metric.unit);
    const labels = ticks.map((value) => formatValue(value, metric.unit));
    const left = Math.min(100, 16 + Math.max(...labels.map((label) => label.length)) * 6.5);
    const right = width - 12, top = 12, base = height - 50, band = (right - left) / n;
    const y = (log ? logScale : linearScale)(log ? ticks[0] : 0, ticks.at(-1), base, top);
    const cx = (i) => left + band * (i + 0.5);
    const xs = buckets.map((_, i) => cx(i));
    const text = (x, yy, content, anchor = "middle", cls) => {
      const node = svg("text", { x: fx(x), y: fx(yy), "text-anchor": anchor, class: cls });
      node.textContent = content;
      return node;
    };
    const chart = svg("svg", { width, height, viewBox: `0 0 ${width} ${height}`, class: "cpa-u-svg", tabindex: "0", role: "img",
      "aria-label": t("chartLabel", ui.chartTitle.textContent, n) });
    const grid = svg("g", { class: "cpa-u-grid" });
    ticks.forEach((value, i) => {
      const yy = fx(y(value));
      if (i) grid.append(svg("line", { x1: left, x2: right, y1: yy, y2: yy }));
      if (ticks.length <= 7 || i % 2 === 0) grid.append(text(left - 8, yy + 4, labels[i], "end"));
    });
    grid.append(svg("line", { x1: left, x2: right, y1: base, y2: base, class: "cpa-u-base" }));
    let lastDate = "";
    for (const i of tickIndexes(buckets, step, right - left, (at) => (day ? 0 : -new Date(at).getTimezoneOffset() * 60000))) {
      const x = cx(i), at = buckets[i], date = day ? "" : md(at), midnight = !day && hm(at) === "00:00";
      const node = text(x, base + 17, day ? md(at, true) : midnight ? date : hm(at), x - left < 26 ? "start" : right - x < 26 ? "end" : "middle");
      if (date !== lastDate && !midnight) {
        const line = svg("tspan", { x: fx(x), dy: 14 });
        line.textContent = date;
        node.append(line);
      }
      lastDate = date;
      grid.append(node);
    }
    const offset = utcOffsetLabel(-new Date(buckets[n - 1]).getTimezoneOffset());
    grid.append(text(right, height - 3, day ? t("utcDays") : t("localTime", offset), "end", "cpa-u-axis-title"));

    const marks = svg("g"), defs = svg("defs"), hatches = new Set();
    const dot = (x, yy, color) => paint(svg("circle", { cx: fx(x), cy: fx(yy), r: 4, class: "cpa-u-dot" }), color);
    const hatch = (entry) => {
      const id = `cpa-u-hatch-${entry.token.replace("/", "-")}`;
      if (!hatches.has(id)) {
        hatches.add(id);
        const pattern = paint(svg("pattern", { id, class: "cpa-u-hatch", width: 6, height: 6, patternUnits: "userSpaceOnUse",
          patternTransform: `rotate(${entry.pattern === 1 ? 45 : -45})` }), entry.color);
        pattern.append(svg("rect", { width: 6, height: 6 }), svg("line", { x1: 1.5, x2: 1.5, y2: 6 }));
        defs.append(pattern);
      }
      return `url(#${id})`;
    };
    const path = (d, cls, entry) => {
      const node = paint(svg("path", { d, class: entry.pattern ? `${cls} is-p${entry.pattern}` : cls }), entry.color);
      if (entry.pattern && cls !== "cpa-u-line") node.style.fill = hatch(entry);
      marks.append(node);
    };
    const trace = (ys, entry) => {
      path(linePath(xs, ys), "cpa-u-line", entry);
      ys.forEach((yy, i) => yy != null && ys[i - 1] == null && ys[i + 1] == null && marks.append(dot(xs[i], yy, entry.color)));
    };
    if (type === "stacked") {
      const barWidth = Math.max(1, Math.min(24, band * 0.72));
      const topmost = buckets.map((_, i) => visible.reduce((found, entry, j) => (entry.values[i] > 0 ? j : found), -1));
      visible.forEach((entry, j) => path(layers[j].map(([low, high], i) => {
        if (!(high > low)) return "";
        const y0 = y(low), y1 = y(high), isTop = topmost[i] === j;
        // A 2px surface gap separates touching segments.
        return barPath(cx(i) - barWidth / 2, barWidth, y0, !isTop && y0 - y1 > 3 ? y1 + 2 : y1, isTop ? 4 : 0);
      }).join(""), "cpa-u-bar", entry));
    } else if (type === "grouped") {
      const k = Math.max(1, visible.length), groupWidth = Math.min(band * 0.8, k * 24 + (k - 1) * 2);
      const gap = groupWidth / k > 6 ? 2 : 0, barWidth = Math.max(0.5, (groupWidth - gap * (k - 1)) / k);
      visible.forEach((entry, j) => path(entry.values.map((value, i) => (value > 0
        ? barPath(cx(i) - groupWidth / 2 + j * (barWidth + gap), barWidth, base, y(value)) : "")).join(""), "cpa-u-bar", entry));
    } else if (type === "lines") {
      for (const entry of visible) trace(entry.values.map((value) => (value == null || (log && !(value > 0)) ? null : y(value))), entry);
    } else {
      visible.forEach((entry, j) => {
        const tops = layers[j].map(([, high]) => y(high));
        path(areaPath(xs, tops, layers[j].map(([low]) => y(low))), "cpa-u-area", entry);
        trace(tops, entry);
      });
    }

    const highlight = svg("rect", { y: top, height: base - top, width: fx(band), class: "cpa-u-band", display: "none" });
    const cross = svg("line", { y1: top, y2: base, class: "cpa-u-cross", display: "none" });
    const dots = svg("g");
    chart.append(defs, grid, highlight, cross, marks, dots);
    const show = (i) => {
      const x = cx(i);
      hoverIndex = i;
      highlight.setAttribute("x", fx(x - band / 2));
      highlight.setAttribute("display", bars ? "inline" : "none");
      for (const name of ["x1", "x2"]) cross.setAttribute(name, fx(x));
      cross.setAttribute("display", bars ? "none" : "inline");
      dots.replaceChildren(...(bars ? [] : visible.flatMap((entry, j) => {
        const value = stacked ? layers[j][i][1] : entry.values[i];
        return value == null || (log && !(value > 0)) ? [] : [dot(x, y(value), entry.color)];
      })));
      showTip(i, x, width, metric, list, visible);
    };
    const hide = () => {
      for (const node of [highlight, cross]) node.setAttribute("display", "none");
      dots.replaceChildren();
      ui.tip.hidden = true;
    };
    const indexAt = (event) => {
      const box = chart.getBoundingClientRect();
      return clamp(Math.floor(((event.clientX - box.left) * (width / box.width) - left) / band), 0, n - 1);
    };
    chart.addEventListener("pointermove", (event) => show(indexAt(event)));
    chart.addEventListener("pointerdown", (event) => show(indexAt(event)));
    chart.addEventListener("pointerleave", hide);
    chart.addEventListener("blur", hide);
    chart.addEventListener("focus", () => show(hoverIndex >= 0 && hoverIndex < n ? hoverIndex : n - 1));
    chart.addEventListener("keydown", (event) => {
      const move = { ArrowLeft: -1, ArrowRight: 1, Home: -n, End: n }[event.key];
      if (event.key === "Escape") hide();
      if (!move) return;
      event.preventDefault();
      show(clamp((hoverIndex < 0 ? n - 1 : hoverIndex) + move, 0, n - 1));
    });
    ui.tip.hidden = true;
    ui.plot.replaceChildren(chart, ui.tip);
    if (refocus) chart.focus();
  }

  function showTip(i, x, width, metric, list, visible) {
    const row = (color, value, name, cls = "", pattern = 0) => paint(el("div", { class: `cpa-u-tiprow ${cls}` },
      el("i", { class: pattern ? `is-p${pattern}` : null }), el("strong", { text: formatValue(value, metric.unit) }), el("span", { text: name })), color);
    const total = metric.get(list.reduce((sum, entry) => addPoints(sum, entry.points[i]), normPoint()));
    ui.tip.replaceChildren(el("div", { class: "cpa-u-tiptime", text: bucketLabel(data.series.buckets[i]) }),
      ...visible.filter((entry) => entry.values[i] != null).map((entry) => row(entry.color, entry.values[i], entry.label, "", entry.pattern)),
      ...(list.length > 1 ? [row("transparent", total, t(metric.avg ? "overall" : "total"), "is-total")] : []));
    ui.tip.hidden = false;
    const flip = x > width * 0.6;
    ui.tip.style.left = flip ? "auto" : `${x + 14}px`;
    ui.tip.style.right = flip ? `${width - x + 14}px` : "auto";
  }

  function fullSeries(metric) {
    const { series, buckets } = data.series;
    const out = series.map((entry) => ({ label: seriesLabel(entry, data.group), values: entry.points.map(metric.get) }));
    const totals = buckets.map((_, i) => metric.get(series.reduce((sum, entry) => addPoints(sum, entry.points[i]), normPoint())));
    return { series: out, totals: out.length > 1 ? totals : null };
  }

  const fullNumber = (value, unit) => (value == null ? "—" : unit ? formatValue(value, unit)
    : new Intl.NumberFormat(root.lang || undefined, { maximumFractionDigits: 0 }).format(value));

  function renderTable(metric) {
    const { series, totals } = fullSeries(metric);
    const columns = [t("time"), ...series.map((entry) => entry.label), ...(totals ? [t("total")] : [])];
    ui.table.replaceChildren(el("table", { class: "cpa-u-table" },
      el("thead", {}, el("tr", {}, columns.map((name) => el("th", { scope: "col", text: name })))),
      el("tbody", {}, data.series.buckets.map((at, i) => el("tr", {}, el("th", { scope: "row", text: bucketLabel(at) }),
        [...series.map((entry) => entry.values[i]), ...(totals ? [totals[i]] : [])].map((value) => el("td", { text: fullNumber(value, metric.unit) })))))));
  }

  function exportCSV() {
    if (!data) return;
    const metric = METRICS[view.metric], { series, totals } = fullSeries(metric);
    const digits = metric.unit === "usd" ? 6 : metric.avg ? 2 : 0;
    const rows = [["bucket_start_utc", ...series.map((entry) => entry.label), ...(totals ? ["total"] : [])],
      ...data.series.buckets.map((at, i) => [new Date(at).toISOString(),
        ...[...series.map((entry) => entry.values[i]), ...(totals ? [totals[i]] : [])].map((value) => (value == null ? "" : round(value, digits)))])];
    const url = URL.createObjectURL(new Blob([`\ufeff${buildCSV(rows)}`], { type: "text/csv;charset=utf-8" }));
    const stamp = new Date(data.series.buckets[0] || Date.now()).toISOString().slice(0, 13).replace(/[-:T]/g, "");
    const link = el("a", { href: url, download: `cpa-usage-${view.metric}-${data.group.join("-") || "total"}-${stamp}.csv`, hidden: true });
    page.append(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  function renderBreakdown() {
    const by = data.summary.by && typeof data.summary.by === "object" ? data.summary.by : {};
    const dims = DIMS.filter((dim) => Array.isArray(by[dim])), tab = dims.includes(view.tab) ? view.tab : dims[0];
    if (!tab) return (ui.breakdownCard.hidden = true);
    const refocus = ui.tabs.contains(document.activeElement);
    ui.tabs.replaceChildren(...dims.map((dim) => el("button", {
      type: "button", role: "tab", id: `cpa-usage-tab-${dim}`, "aria-selected": String(dim === tab), tabindex: dim === tab ? "0" : "-1",
      text: t(`d_${dim}`), onclick: () => {
        view.tab = dim;
        saveView();
        renderBreakdown();
      },
    })));
    if (refocus) ui.tabs.querySelector('[aria-selected="true"]').focus();
    ui.breakdown.setAttribute("aria-labelledby", `cpa-usage-tab-${tab}`);
    const columns = [["requests"], ["failures"], ["tokens"], ["input"], ["output"], ["cache_read"], ["latency", "ms"], ["cost", "usd"]];
    const rows = by[tab].filter((row) => row && typeof row === "object");
    ui.breakdown.replaceChildren(el("table", { class: "cpa-u-table" },
      el("thead", {}, el("tr", {}, [tab, ...columns.map(([key]) => key)].map((key, i) => el("th", { scope: "col", text: t(i ? `m_${key}` : `d_${key}`) })))),
      el("tbody", {}, rows.length ? rows.map((row) => {
        const point = normPoint(row.point);
        return el("tr", {}, el("th", { scope: "row", title: valueTitle(tab, row.value), text: valueLabel(tab, row.value) }), columns.map(([key, unit]) => {
          const note = key === "cost" && tab === "model" && unpricedLabel(prices, row.value, t);
          return el("td", { class: note ? "cpa-u-unpriced" : null, text: note || fullNumber(METRICS[key].get(point), unit) });
        }));
      }) : el("tr", {}, el("td", { colspan: columns.length + 1, class: "cpa-u-empty", text: t("noData") })))));
  }

  function renderPrices() {
    if (!ui) return;
    ui.pricesCard.hidden = !prices || !data || ui.chartCard.hidden;
    const input = (row, key, attrs) => el("input", { ...attrs, value: row[key], oninput: (event) => { row[key] = event.target.value; } });
    ui.priceBody.replaceChildren(...priceRows.map((row, index) => el("tr", {},
      el("td", {}, input(row, "model", { type: "text", list: "cpa-usage-models", "aria-label": t("model"), spellcheck: "false" })),
      PRICE_FIELDS.map((key) => el("td", {}, input(row, key, {
        type: "number", min: "0", step: "any", inputmode: "decimal", "aria-label": `${row.model || t("model")} ${t(`p_${key}`)}`,
      }))),
      el("td", {}, button(t("remove"), () => {
        priceRows.splice(index, 1);
        renderPrices();
      }, "cpa-u-link")))));
  }

  async function savePrices() {
    const entries = new Map();
    for (const row of priceRows) {
      const model = row.model.trim(), entry = {};
      if (!model || entries.has(model)) return priceStatus(model ? t("priceDuplicate", model) : t("priceModel"), true);
      for (const key of PRICE_FIELDS) {
        entry[key] = String(row[key]).trim() === "" ? 0 : Number(row[key]);
        if (!(entry[key] >= 0) || !Number.isFinite(entry[key])) return priceStatus(t("priceInvalid"), true);
      }
      entries.set(model, entry);
    }
    priceStatus(t("saving"));
    try {
      await api("prices", { method: "PUT", body: { prices: Object.fromEntries(entries) } });
      await loadPrices();
      priceStatus(t("saved"));
      load();
    } catch (failure) {
      priceStatus(message(failure?.kind ? failure : { kind: "network" }), true);
    }
  }

  function priceStatus(text, failed = false) {
    if (!ui) return;
    ui.priceStatus.textContent = text;
    ui.priceStatus.classList.toggle("cpa-u-error", failed);
  }

  function syncAuto() {
    if (page && view.auto && !autoTimer) {
      autoTimer = setInterval(() => page && !loading && document.visibilityState === "visible" && load(), 60000);
    } else if (!(page && view.auto) && autoTimer) {
      clearInterval(autoTimer);
      autoTimer = 0;
    }
  }

  function ensureNav(active) {
    const anchor = document.querySelector('.sidebar .nav-section a.nav-item[href$="/logs"]')
      || document.querySelector('.sidebar .nav-section a.nav-item[href$="/quota"]');
    if (!anchor) return;
    const link = document.querySelector('a[data-cpa-nav="usage"]') || el("a", {
      class: "nav-item", "data-cpa-owned": true, "data-cpa-nav": "usage", onclick: () => {
        pendingNav = Date.now();
        requestAnimationFrame(() => document.querySelector(".sidebar-backdrop.visible")?.click());
      },
    });
    if (anchor.nextElementSibling !== link) anchor.after(link);
    if (link.getAttribute("href") !== `#${hostPath}?cpa-page=usage`) link.setAttribute("href", `#${hostPath}?cpa-page=usage`);
    const labelled = !!anchor.querySelector(".nav-text"), name = t("nav");
    if (link.dataset.state !== labelled + name) {
      link.dataset.state = labelled + name;
      const icon = svg("svg", { viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", "stroke-width": "2", "stroke-linecap": "round", "aria-hidden": "true" });
      icon.append(svg("path", { d: "M4 20h16M7 16v-4M12 16V7M17 16v-7" }));
      link.replaceChildren(el("span", { class: "nav-icon" }, icon), ...(labelled ? [el("span", { class: "nav-text" }, el("span", { class: "nav-label", text: name }))] : []));
      link.setAttribute("aria-label", name);
      if (labelled) link.removeAttribute("title");
      else link.title = name;
    }
    link.classList.toggle("active", active);
    if (active) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }

  function mount(params) {
    const main = document.querySelector("main.main-content");
    if (!main) return unmount();
    const reload = adoptHash(params);
    if (!page || page.parentElement !== main || page.dataset.lang !== root.lang) {
      unmount();
      main.append((page = buildPage()));
      render();
      renderPrices();
    }
    root.dataset.cpaPage = "usage";
    syncAuto();
    if (reload) saveView();
    mirrorHash();
    if (reload || (!loading && Date.now() - lastAttempt > 60000)) load();
  }

  function unmount() {
    resizer?.disconnect();
    page?.remove();
    page = ui = null;
    delete root.dataset.cpaPage;
    syncAuto();
  }

  function tick() {
    const { path, params } = parseHash(location.hash), active = params["cpa-page"] === "usage";
    // Without the /dashboard alias the router redirects to "/" and drops the query.
    if (!active && pendingNav && Date.now() - pendingNav < 3000 && path === "/" && hostPath !== "/") {
      hostPath = "/";
      pendingNav = 0;
      return location.replace(buildHash("/", { "cpa-page": "usage" }));
    }
    if (active) pendingNav = 0;
    ensureNav(active && !!document.querySelector("main.main-content"));
    if (active) mount(params);
    else if (page || root.dataset.cpaPage) unmount();
  }

  function start() {
    const app = document.getElementById("root");
    if (!app) return;
    let pending = false;
    const schedule = () => {
      if (pending) return;
      pending = true;
      requestAnimationFrame(() => {
        pending = false;
        try { tick(); } catch { /* The panel must keep working without the overlay. */ }
      });
    };
    const owned = (node) => !!(node.nodeType === 1 ? node : node.parentElement)?.closest("[data-cpa-owned]");
    const ours = (mutation) => {
      const nodes = [...mutation.addedNodes, ...mutation.removedNodes];
      return owned(mutation.target) || (nodes.length > 0 && nodes.every((node) => node.nodeType === 1 && node.hasAttribute("data-cpa-owned")));
    };
    new MutationObserver((mutations) => mutations.some((mutation) => !ours(mutation)) && schedule())
      .observe(app, { childList: true, subtree: true, attributes: true, attributeFilter: ["class", "href", "aria-current"] });
    new MutationObserver(schedule).observe(root, { attributes: true, attributeFilter: ["lang"] });
    addEventListener("hashchange", schedule);
    addEventListener("popstate", schedule);
    document.addEventListener("pointerdown", (event) => {
      for (const details of page?.querySelectorAll(".cpa-u-filter[open]") || []) if (!details.contains(event.target)) details.open = false;
    }, true);
    onCapture = () => page && error && error.kind !== "disabled" && load();
    schedule();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start, { once: true });
  else start();
})();
