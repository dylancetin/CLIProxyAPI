feat(usage): add SQLite usage tracker with a Usage page in the control panel

## Why

The proxy stopped shipping built-in usage statistics in v6.10. The only data left is a short in-memory window per credential and a queue that external tools drain. This change records every upstream attempt in SQLite with hourly rollups and shows it in the control panel, so an operator can see tokens, requests, failures, latency, and estimated cost by hour, provider, model, account, and client key without a second service.

## What changed

- New `internal/usagestore` package. A usage plugin batches records into SQLite (pure Go driver, no cgo), keeps raw rows plus hourly rollups, applies retention, and answers bucketed queries.
- New `observability.usage.tracker` config block and v8 endpoints under `/v8/management/observability/usage/` for series, summary, dimensions, and an editable price table.
- The served control panel gets a Usage entry in the Observe group and a page with charts, breakdown tables, and a prices editor, injected by the existing appearance overlay. Also fixes an existing overlay bug where injection offsets were computed on a lowercased copy of the page.

## Scope

- Covers the tracker, its API, the panel page, and an end-to-end test that drives a real server with fake upstreams.
- Leaves the usage queue and its consumers untouched. The tracker runs beside the queue.
- Leaves the TUI unchanged. It shows only the old config fields.

## Tradeoffs

- Enabled by default, writing `usage.sqlite` next to the auth directory, or in a `usage` directory beside a remote store's root. Turn it off with `observability.usage.tracker.enabled: false`.
- Charts are hand-written SVG, about 78 KB of JS and CSS added to every panel load. A chart library would have cost two to three times that.
- Cost is computed at query time from the current price table, so a price edit reprices history.

## Blast Radius

The plugin enqueues without blocking and drops with a warning when its buffer is full, so the request path and the queue plugin are not slowed. The overlay fails closed when the panel markup it expects is missing. Remote token stores never see the database because it lives outside their directories.

## Verification

- `go test ./test/ -run UsageTracker -race`: a real server with two fake providers, 16 requests, 18 attempts after failover. Raw rows, rollups, every endpoint, price persistence through reload, disabled mode, and queue parity all match the request ledger.
- Real CPAMC v1.27.2 panel against a real store seeded with 3,000 records: 80 Playwright checks pass, tiles and tables equal fresh queries, no console errors. Screenshots and JSON under `.audit/evidence/`.
- `CGO_ENABLED=0 go build ./...` and `go build ./...` pass. `go test ./...` fails only in three packages that fail the same way on main on this host.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
