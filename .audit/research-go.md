# Research map: Go usage pipeline (worktree HEAD be19925d)

Produced by an Opus 5.5 xhigh research delegate on 2026-11-02. Read-only; no files changed.

## Corrections to the original brief

- No in-memory aggregator exists. `usage-statistics-enabled` (`Config.UsageStatisticsEnabled`) only gates the redisqueue plugin. Comments at `internal/config/config.go:74` and `config.example.yaml:1164` are stale.
- No `internal/usage/`. AGENTS.md lists it, but usage code lives in `sdk/cliproxy/usage/`.
- Config section is `observability.usage`, not `logging.usage`.
- Only per-auth success/failure counters exist (section 2). Leave them alone.

## 1. Record shapes and publishing

`sdk/cliproxy/usage/manager.go`
- `Record` (`:25-75`): `RequestID` (one per upstream attempt), `TraceID` (inbound HTTP request id), `Provider`, `BaseURL`, `ExecutorType`, `Model`, `Alias`, `APIKey` (raw client key), `IsNativeKey`, `AccessProvider`, `SessionID`, `ParentSessionID`, `AuthID`, `AuthIndex`, `AccessTokenSHA256`, `AuthType`, `Source`, `ReasoningEffort`, `ServiceTier`, `RequestServiceTier` (deprecated), `ResponseServiceTier`, `ResponseModel`, `Generate *bool`, `Stream`, `RequestedAt time.Time`, `Latency`, `TTFT time.Duration`, `Failed bool`, `Fail Failure`, `Detail Detail`, `ResponseHeaders http.Header`.
- `Failure` (`:78-81`): `StatusCode int`, `Body string`. No reason enum exists; derive from status buckets.
- `Detail` (`:84-94`): `InputTokens`, `OutputTokens`, `ReasoningTokens`, `CachedTokens`, `CacheReadTokens`, `CacheCreationTokens`, `TotalTokens`, `TokenBreakdown`, `ResponseServiceTier`.
- `TokenBreakdown` (`accounting.go:42-49`): `SchemaVersion` (=2), `Quality` (`complete|inconsistent|unclassified`), `TotalTokens`, `Input{TotalTokens, UncachedTokens, CacheReadTokens, CacheWriteTokens}`, `Output{TotalTokens, NonReasoningTokens, ReasoningTokens}`, `UnclassifiedTokens`. Non-overlapping. `Valid()` at `:52`. Normalize with `EnsureTokenBreakdownForProvider(detail, record.Provider, record.ExecutorType)` (`:253`); the reporter already applies it before publishing.

Context setters (all `manager.go`):
- `WithAPIKey`, `WithAccessProvider`, `WithIsNativeKey`: `internal/api/server_middleware.go:212-214`, `:274-276`.
- `WithRequestedModelAlias`, `WithReasoningEffort`, `WithServiceTier`, `WithGenerate`, `WithStream`: `contextWithRequestedModelAlias` at `sdk/cliproxy/auth/conductor_execution.go:1603-1619`, called per attempt at `conductor_execution.go:532,746,1065`, `conductor_stream.go:235`, `conductor_home_execution.go:133`, `conductor_home.go:1409`. `WithStream` also at `sdk/api/handlers/handlers_execution.go:200` and `handlers_stream.go:45`.
- `WithExecutionRequestID`, `WithTraceID`: never called in non-test code. `Publish` falls back to `internallogging.GetRequestID(ctx)` (`manager.go:511-517`).
- Service tier defaults `"auto"` for OpenAI handlers (`sdk/api/handlers/handlers.go:379-392`), `"default"` elsewhere (`manager.go:17,201-235`). `ReasoningEffort` is overwritten by `SetTranslatedReasoningEffort` (`usage_helpers.go:346`) with the translated upstream level.

Publishing (`internal/runtime/executor/helps/usage_helpers.go`):
- `NewExecutorUsageReporter(ctx, e, model, auth)` (`:77`); `NewUsageReporter` (`:87-151`) captures `requestedAt=time.Now()` (`:132`), `provider=executor.Identifier()`, `authID=auth.ID`, `authIndex=auth.EnsureIndex()`, `authType=auth.AuthKind()`, `source=resolveUsageSource` (`:823-860`).
- Emitted once via `once.Do` (`:541`, `:569`) through `Publish` / `PublishFailure` / `TrackFailure` / `EnsurePublished`. `buildRecordForModel` at `:629-670`. `publishRecord` (`:581-584`) adds `ResponseHeaders` and calls `usage.PublishRecord`. `PublishAdditionalModel` (`:338`) emits extra records with new `RequestID`.
- Plugin executors get a reporter in `sdk/api/handlers/handlers_execution.go:215-233` and `handlers_stream.go:66-163` with `auth=nil`.
- Retries: each credential retry is a separate record, new `RequestID`, same `TraceID`. Count requests by `TraceID`, attempts by `RequestID`.

Dispatch is async:
- `Manager.Publish` (`:500-545`) backfills `RequestID`, `TraceID`, `APIKey`, `AccessProvider`, `IsNativeKey` from context, appends to an unbounded slice (`buffer` arg ignored, `:427`). One worker goroutine (`run`, `:547`) calls every plugin in turn (`dispatch`, `:564`) with panic recovery. A slow plugin delays the redisqueue plugin, so the tracker must buffer/batch internally.
- `StopDefault` (`sdk/cliproxy/service_lifecycle.go:392`) does not drain and cannot restart (`once`). The tracker needs its own flush/close hook.
- Register with `usage.RegisterNamedPlugin(name, p)` (`:598`) or `Service.RegisterUsagePlugin` (`sdk/cliproxy/service_plugins.go:67`).

Credential identity on the record:
- `AuthID` is the relative file name for file auths (`sdk/auth/filestore.go:344-346`). `AuthIndex` is a stable hash (`sdk/cliproxy/auth/types.go:471`). `Provider` is the executor identifier, not `auth.Provider`; openai-compat routing name is in `auth.Attributes["compat_name"]`.
- `Source` can be the raw upstream API key (`usage_helpers.go:850-853`). Hash or omit before storing. `APIKey` is the raw client key; hash it.
- `auth.Label` and `auth.FileName` are not on the record; look up via `coreauth.Manager`.

## 2. Existing counters, toggle consumers, endpoints

- Per-auth `Auth.Success` / `Auth.Failed` (`sdk/cliproxy/auth/types.go:111-112`), `recentRequests` ring (`:121`, `:168-170`, `:283-325`), incremented in `conductor_cooldown.go:919,1417`. Read by `GetAPIKeyUsage` (`internal/api/handlers/management/api_key_usage.go:58-117`), `auth_files.go:681`, pluginhost `auth_callbacks.go:884`. Leave alone.
- Toggle consumers: `cmd/server/main.go:631-632`; Home forces on at `main.go:380`, `sdk/cliproxy/service_config.go:344`, `service_lifecycle.go:66`; reload `internal/api/server_reload.go:95-101`; handlers `config_basic.go:206-212`; TUI `internal/tui/dashboard.go:186-198`, `config_tab.go:342,372`; diff `internal/watcher/diff/config_diff.go:43-48`.
- Endpoints: v0 (deprecated, frozen) `usage-statistics-enabled`, `api-key-usage`, `usage-queue` (`server_management.go:66-68,94,95`). Current: `/v8/management/observability/usage/api-keys` and `/queue` (`server_management_v8.go:40-41`). Generic config path `/v8/management/config/observability/usage/usage-statistics-enabled` via `ConfigV8` (`server_management_v8.go:21-28`).
- Home mode: management routes 404 (`server_management.go:216-219`), usage forwarded via queue.

## 3. Redis queue feed (leave untouched)

- `internal/redisqueue/plugin.go:16-18` registers the unnamed plugin in `init()`. `HandleUsage` (`:21-176`) returns unless `Enabled() && UsageStatisticsEnabled()` (`:26`).
- `Enabled` set at `internal/api/server.go:250-252`, `server_reload.go:167`, `service_lifecycle.go:132`.
- Readers: `PopOldest` (management `usage.go:24-43`, RESP `redis_queue_protocol.go:239`, Home forwarder `service_home.go:364-375`), `SubscribeUsage` (`redis_queue_protocol.go:226`), `NotifyUsageRefresh` (`internal/watcher/clients.go:150,276,310`).
- Recommendation: `RegisterNamedPlugin("sqlite-usage", ...)` behind its own config flag. Do not reuse `UsageStatisticsEnabled`.

## 4. Config plumbing

- `Config` is flat; v8 YAML layout mapped at boundary (`internal/config/config_v8.go:15`, `buildV8Paths()` `:71`, e.g. `{"usage-statistics-enabled", "observability.usage.usage-statistics-enabled"}` at `:108-109`). A new nested field needs a prefix entry such as `{"usage-tracker", "observability.usage.tracker"}` or `commentUnknownV8Fields` (`:681`) comments it out; allowed set built from `v8Paths` (`:576`).
- Defaults before unmarshal in two aligned places: `config_load.go:193-205` and `parse.go:24-35`. Clamps at `config_load.go:278`, `parse.go:98`.
- `CloneForRuntime` (`clone.go:12`) deep-copies by reflection.
- Hot reload: `internal/watcher/config_reload.go:95-150`, diff lines in `config_diff.go` near `:43`, runtime side effects in `Server.UpdateClientsContext` (`server_reload.go:47+`), via `Service.updateServerClientsContext` (`service_config.go:271-283`). Management handler gets `SetConfig` (`handler.go:127`).
- Docs: `observability.usage:` block at `config.example.yaml:1144-1170`. `TestV8ExampleLoadsAndRoundTrips` (`config_v8_test.go:18`) and `config_load_test.go:183` read the template.
- Auth dir: `util.ResolveAuthDir` (`internal/util/util.go:77-95`), default `~/.cli-proxy-api` (`config_defaults.go:6`), resolved at `cmd/server/main.go:645-650`. Remote stores mirror to a local dir (`main.go:473,540,584`). Precedent for state next to auths: `resolveCooldownStateStore` (`sdk/cliproxy/service_auth.go:648-675`). `util.WritablePath()` (`util.go:118`).

## 5. Management API wiring and panel overlay

- `registerManagementRoutes` (`server_management.go:16-30`) runs once a secret exists (`server.go:250-254`; `server_reload.go:142,151`). Calls `registerManagementV8Routes()` (`server_management_v8.go:12-63`).
- Auth: `managementAvailabilityMiddleware` (`server_management.go:206-231`); `Handler.Middleware()` (`handlers/management/handler.go:291-322`) accepts `Authorization: Bearer` or `X-Management-Key`; `AuthenticateManagementKey` (`:326-425`). `server_middleware.go:86` exempts management paths from safe mode.
- Route contract test: `TestManagementV8IndependentContract` (`server_management_v8_test.go:64+`). Handler dependency setters pattern: `SetAuthManager` / `SetPluginHost` (`handler.go:156-177`).
- Panel serving: `GET /management.html` (`server_routes.go:56`) via `serveManagementControlPanel` (`server_management.go:314-350`), file at `managementasset.FilePath` (`updater.go:174`, `MANAGEMENT_STATIC_PATH`), wrapped by `WithAppearance` (`appearance.go:21-51`): only pages containing `cli-proxy-theme` without `id="cpa-appearance"`; injects script before first head script and style before `</head>`.
- `appearance.js` makes no API calls today; localStorage `cli-proxy-theme`, `cpa-quota-view`; MutationObserver over CSS-module class names.
- Panel API client: `Authorization: Bearer ${managementKey}` to `apiBase + "/v8/management"`. Auth persisted in localStorage `cli-proxy-auth` (zustand partialize: `apiBase`, `managementKey` only if `rememberPassword`). Obfuscated `enc::v2::` + base64(XOR(JSON, utf8("cli-proxy-api-webui::secure-storage|" + location.host))); `enc::v1::` also mixes `navigator.userAgent`.
- Overlay tests: `appearance_test.go`, `appearance.test.mjs` (`node --test`, vm sandbox).

## 6. Latency and TTFT

- `requestedAt` at reporter construction (`usage_helpers.go:132`). `Record.Latency = time.Since(requestedAt)` at publish (`:695-704`); streaming publish deferred to stream end.
- TTFT: `TrackHTTPClient` wraps transport with `usageTTFTRoundTripper` (`:745-758`), first body byte via `usageTTFTReadCloser` (`:766`). `TrackHTTPClientRoundTripOnly` for codex/openai-compat/meta streams, true TTFT via `ObserveTokenEvent` (`:447-482`) from `helps/*_ttft_helpers.go`. Websocket executors call `StartResponseTTFT` / `MarkFirstResponseByte`. `ttftDuration()` (`:724-737`) falls back to first packet. Measured from upstream send.
- Handler-level: `requestLifecycleTracker.completion.StartedAt` (`sdk/api/handlers/handlers_interceptors.go:111-137`). Response status via `internallogging.GetResponseStatus(ctx)` (used by `redisqueue/plugin.go:235-251`).

## 7. Pricing

No price data or logic anywhere. `internal/registry/models/models.json` has no prices. Build from scratch as a config map.

## 8. go.mod, cgo, CI

- `go 1.26.0`. `pgx/v5` via `database/sql` in `internal/store/postgresstore.go:16,91`. No sqlite module in go.sum.
- Dockerfile `CGO_ENABLED=1` (`:17`); release.yaml CGO builds (`:176`, `:305-316` glibc 2.17 check) plus `CGO_ENABLED=0` builds (`:429`, `:626`). pluginhost split on `cgo` tags. Driver must be pure Go.
- `pr-test-build.yml` builds only; `pr-path-guard.yml` rejects `internal/translator/**` changes.

## 9. Tests to keep green

- `sdk/cliproxy/usage`: `accounting_test.go`, `manager_test.go`, `stream_delivery_test.go`.
- `internal/redisqueue`: `plugin_test.go`, `queue_test.go`.
- `internal/runtime/executor/helps`: `usage_helpers_test.go`, `responses_usage_helpers_test.go`, `responses_ttft_helpers_test.go`, `plugin_executor_usage_test.go`.
- Management: `usage_test.go`, `api_key_usage_test.go`. `internal/api`: `server_test.go` (~`:1668`, `:638`), `server_management_v8_test.go`.
- Config: `config_v8_test.go`, `config_load_test.go`, `clone_test.go`, `internal/watcher/diff/config_diff_test.go:397-619`.
- SDK: `service_stale_state_test.go:78-125`, `service_executionregistry_test.go:2811,2850`. Integration: `test/usage_logging_test.go`. Overlay: `appearance_test.go`, `appearance.test.mjs`.

## Coordinate

- AGENTS.md lists non-existent `internal/usage/`.
- Home mode consumes the queue (`internal/home/client.go:1471`); tracker should stay off in Home mode.
