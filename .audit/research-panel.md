# Research map: CPAMC panel and the appearance overlay

Produced by an Opus 5.5 xhigh research delegate on 2026-11-02. Read-only. Inspected: worktree at be19925d, the live panel at `~/.config/cliproxyapi/static/management.html` (CPAMC v1.27.2, same sha256 as the latest release asset), and a CPAMC source clone at f154fa9. Copies in `/tmp/cpamc-inspect.ZyOW` and `/tmp/cpamc-src.wIoW/repo` (may be gone; re-download from the GitHub releases API if needed).

## Hard constraints

1. A plain `#/usage` URL does not work. CPAMC's route table sends unknown paths to the dashboard (`src/router/MainRoutes.tsx:48`).
2. The management key is often not stored. CPAMC persists it only when "remember password" is on. Read it from the app's own outgoing requests instead.
3. The queue endpoint `/v8/management/observability/usage/queue` is destructive (PopOldest). The UI must never call it.

## 1. Current overlay

- Go: `server_routes.go:56` GET /management.html -> `serveManagementControlPanel` (`internal/api/server_management.go:314-351`) serves `managementasset.WithAppearance(page)` (`:350`). Disk file never changes.
- `appearance.go:22` patches only pages containing `cli-proxy-theme` and lacking `id="cpa-appearance"` (idempotent). Pages without `<head>...</head>` returned unchanged (`:26-30`). Script `<script id="cpa-appearance">` inserted before the first `<script` in head (`:32-38,42-44`); style `<style id="cpa-appearance-style">` before `</head>` (`:46-48`). Both `go:embed` (`:11-15`).
- CSP: none in HTML or server. Inline JS/CSS/SVG all work. Avoid CDNs anyway.
- `appearance.js`: theme bootstrap (`:7-28`) reads zustand `cli-proxy-theme`, defaults dark, sets `html[data-theme]` before React. Quota layout in `localStorage["cpa-quota-view"]` mirrored to `html[data-cpa-quota-view]` (`:30-33`). `part(component, name)` -> `[class*="Component-module__name__"]` (`:37`). One MutationObserver on `#root` (subtree, childList, characterData, attributes style/title), throttled per rAF (`:166-185`). Mutations inside `[data-cpa-owned]` are ignored (`:179`), so every overlay node must carry `data-cpa-owned`. Second observer on `html[lang]` (`:181`); i18n table keyed by `root.lang` (`:41-48`), lacks `tr`.
- No router hooks, no API calls, no key handling exist today.
- Tests: `appearance_test.go:9-40` ordering/idempotency, `:42-55` unknown panels; `internal/api/server_management_appearance_test.go:15-53` GET/HEAD with `MANAGEMENT_STATIC_PATH`. Node: `node --test internal/managementasset/appearance.test.mjs`, `node:vm` with hand-written document/localStorage/window stubs (`:9-32`). No jsdom, no package.json. 12/12 pass on Node v24.12.0.
- Theme vars (CPAMC `src/styles/themes.scss`; `:root` light `:6`, `[data-theme='white']` `:96`, `[data-theme='dark']` `:166`): `--bg-primary/secondary/tertiary/hover/quinary`, `--floating-surface`, `--floating-shadow`, `--text-primary/secondary/tertiary/quaternary/muted`, `--border-color/primary/secondary/hover`, `--primary-color/hover/active/contrast`, `--primary-8/10/30`, `--success-color`, `--warning-color`, `--error-color`, `--danger-color`, `--info-color`, `--quota-medium-color`, `--viz-success`, `--viz-failure`, `--radius-md`, `--shadow`, `--shadow-lg`, `--ease-out-strong`, `--dur-hover`. Overlay replaces dark palette (`appearance.css:2-27`), sets `--sidebar-panel-width: 240px`. No multi-series chart palette exists; define `--cpa-series-1..8` per theme.

## 2. The panel (v1.27.2, single 3.72 MB file)

- Head: `<html lang="zh-CN" translate="no" class="notranslate">`, charset meta, viewport, then `<script type="module" crossorigin>`, a classic `<script>`, `<style rel="stylesheet" crossorigin>`. The app is a module script, so any classic inline script in head runs first.
- React 19.3.1 + react-router (hash router, `createHashRouter` in `src/App.tsx:2`) + zustand + axios + i18next. Mounts into `#root`, sets `document.documentElement.lang`.
- Routes: `/login`, `/*` -> main layout with `/`, `/dashboard` (alias, no nav link), `/quick-start`, `/ai-providers`, `/auth-files`, `/oauth`, `/routing`, `/quota`, `/logs`, `/config`, `/plugins`, `/plugin-store`, `/system`, `*` -> Navigate to `/`. `/plugin-pages/:pluginId/:menuIndex` only when plugin support is on.
- Sidebar (global classes, source `src/components/layout/MainLayout.tsx:1175-1185`): `div.nav-section > div.nav-group*` with `div.nav-group-label` or `div.nav-group-divider`, then items. Groups: operate (`/`, `/quick-start`), gateway, observe (`/quota`, `/logs`), control, plugin-pages. Group divs have no id/data attributes.
- NavLink DOM: `<a class="nav-item active" href="#/logs" aria-current="page">` containing `<span class="nav-icon">` and, only when labels are shown, `<span class="nav-text"><span class="nav-label">`. Collapsed sidebar omits `.nav-text`. Tooltips are React state.
- Mobile drawer: `div.sidebar-backdrop.visible` click closes it.
- Page container (source `:1211`): `div.content` (scroll container) > `main.main-content` > transition wrapper > page.
- Charts: no chart library in the bundle. CPAMC's own charts are hand-built DOM (`ThroughputChart`, `Sparkline`, `QuotaTimeline`) using `--viz-success`/`--viz-failure`, with tooltip and table toggle.
- Reusable global classes: `.card`, `.card-header`, `.btn`, `.btn-primary`, `.btn-secondary`, `.btn-sm`, `.input`, `.pill`, `.empty-state`, `.error-box`, `.loading-spinner`, `.status-badge`.
- API client (`src/services/api/client.ts:124-131`, `src/utils/constants.ts:17,32`): base `${apiBase}/v8/management`, header `Authorization: Bearer ${managementKey}`, axios adapter order xhr, http, fetch (browser uses XHR). Login form allows a custom apiBase on another origin.
- Key storage (`src/utils/encryption.ts`, `src/stores/useAuthStore.ts:253-255`): localStorage `cli-proxy-auth` = `enc::v2::` + base64(xor(utf8(JSON), utf8(`cli-proxy-api-webui::secure-storage|${location.host}`))). `enc::v1::` adds `|${navigator.userAgent}`. JSON is zustand `{state:{apiBase, managementKey?, rememberPassword, ...}}`; `managementKey` present only with rememberPassword.
- Server auth accepts `Authorization: Bearer` or `X-Management-Key` (`handlers/management/handler.go:301-312`).
- Panel already calls `GET /observability/usage/api-keys`.

## 3. Upstream CPAMC history

- A Usage page existed until v1.9.3, removed in commit 632be0b (2026-05-02). `git show 632be0b^:src/pages/UsagePage.tsx` (426 lines) plus `src/components/usage/*`. Used chart.js 4 + react-chartjs-2, old `GET /usage`. Features: stat cards, `ChartLineSelector` (MAX_CHART_LINES = 9), ranges 7h/24h/7d/all, hour/day toggle, token breakdown and cost trend charts, API/model/credential tables, price settings. Prefs in localStorage `cli-proxy-usage-chart-lines-v1`, `cli-proxy-usage-time-range-v1`.
- Header refresh button handler is a module variable (`src/hooks/useHeaderRefresh.ts:5-10`); the overlay cannot hook it. The Usage page needs its own refresh control.

## 4. Backend reality

- `/observability/usage/api-keys` returns per provider/key success/failed counts plus `recent_requests` (20 x 10-minute in-memory buckets). Not enough for hourly charts.
- Queue records (`internal/redisqueue/plugin.go:179-215`) carry provider, model, alias, auth_index, source, auth_type, api_key, latency_ms, ttft_ms, token_breakdown.
- This host also runs `~/.local/lib/cliproxyapi/usage-collector` writing `~/.local/share/cliproxyapi/usage/usage-*.jsonl` from the queue. Leave it alone.

## 5. Recommendations

A. Injection: new `go:embed` files `usage.js`, `usage.css` with their own ids, injected by `WithAppearance` or a sibling, same `cli-proxy-theme` gate and idempotency. The request-capture hook must be a classic inline script in head so it runs before the module app. Extend `appearance_test.go`.

B. Nav entry: anchor `document.querySelector('.sidebar .nav-section a.nav-item[href$="/logs"]')`, fallback `[href$="/quota"]`, insert with `anchor.after(link)`. Fail closed if no anchor. Markup `<a class="nav-item" href="#/dashboard?cpa-page=usage" data-cpa-owned data-cpa-nav="usage" title aria-label>` with `span.nav-icon` (inline 18px SVG) and `span.nav-text > span.nav-label` only when the anchor has one. Manage `.active`/`aria-current` on our link only; dim real NavLinks via CSS scoped to `html[data-cpa-page]`, never touch React node classes. On mobile, click `.sidebar-backdrop.visible` after navigating.

C. Routing and mount: host route `#/dashboard?cpa-page=usage` (React keeps the query, no NavLink active, dashboard fetches once on mount, no polling). Detect on `hashchange`, `popstate`, and each observer tick; set `html[data-cpa-page="usage"]`. Mount `<section data-cpa-owned data-cpa-page-root>` as last child of `main.main-content`; hide React children with `html[data-cpa-page="usage"] .main-content > :not([data-cpa-page-root]) { display: none }`. Never move or remove React nodes. Every ensure step is a no-op when the node exists. If `/dashboard` alias disappears, fall back to `#/?cpa-page=usage`.

D. API access: wrap `XMLHttpRequest.prototype.open` and `setRequestHeader` plus `window.fetch`; when a request URL contains `/v8/management` and carries `Authorization: Bearer`, keep `{base, key}` in closure memory only. Fallback: decode `cli-proxy-auth` (`enc::v2::`). Else show a "sign in" empty state. On 401 show a message, do not dispatch CPAMC's `unauthorized` event.

E. Charts: hand-rolled SVG, vendor nothing (chart.js ~200 KB is too heavy for every page load; uPlot ~50 KB only if pan/zoom becomes required). Target 10-15 KB: hourly/daily x axis, stacked bars or multi-line, top-N plus Other, legend toggles, metric switch, group-by, filters, range presets, crosshair tooltip, data table toggle.

F. Minimal dependencies: `#root`; `.sidebar .nav-section`, `a.nav-item[href$="/logs"|"/quota"]`, `.nav-icon`, `.nav-text`, `.nav-label`; `main.main-content`; `.sidebar-backdrop.visible`; `html[lang]`, `html[data-theme]`; the `#/path?query` hash format and `/dashboard` alias; `/v8/management` prefix with Bearer over XHR or fetch. No CSS-module selectors.

Fragility: CPAMC auto-updates outside our releases, so fail closed everywhere. `attributeFilter` covers only style/title, so use the explicit `hashchange` listener for active state.

G. Testing: keep pure helpers loadable by the `vm` harness (hash parsing, storage decode, bucketing, scales, SVG paths). Verify DOM behavior with Playwright against a local server with `MANAGEMENT_STATIC_PATH` pointing at a copy of the v1.27.2 release.
