package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const (
	e2eManagementSecret = "e2e-management-secret-value"
	e2eClaudeProvider   = "claude"
	e2eCompatProvider   = "e2e-compat"
	e2eProbePluginName  = "e2e-usage-probe"
	e2eEvidenceEnv      = "USAGE_TRACKER_EVIDENCE_DIR"
)

// Client keys are long enough that the stored hint masks four runes per side.
var e2eClientKeys = []string{
	"sk-e2e-client-alpha-7f3a9c2e5b1d4f60",
	"sk-e2e-client-bravo-2c8e4a6f0d9b3e71",
}

// e2eCredential is one upstream credential. IDs use the relative file names
// that file-backed credentials get, but the harness registers them directly
// with a base_url because file auths cannot point claude or openai-compat
// executors at a fake upstream.
type e2eCredential struct {
	ID          string
	Provider    string
	UpstreamKey string
}

var e2eCredentials = []e2eCredential{
	{ID: "claude-alpha.json", Provider: e2eClaudeProvider, UpstreamKey: "e2e-upstream-claude-alpha-secret"},
	{ID: "claude-bravo.json", Provider: e2eClaudeProvider, UpstreamKey: "e2e-upstream-claude-bravo-secret"},
	{ID: "e2e-compat-main.json", Provider: e2eCompatProvider, UpstreamKey: "e2e-upstream-compat-main-secret"},
}

var e2eProviderModels = map[string][]string{
	e2eClaudeProvider: {"e2e-claude-opus", "e2e-claude-haiku"},
	e2eCompatProvider: {"e2e-compat-large", "e2e-compat-small"},
}

// e2eRequest is one scripted client request. Status is what the fake upstream
// answers for every attempt of the request; 200 means success.
type e2eRequest struct {
	Provider string
	Model    string
	Key      int
	Stream   bool
	Status   int
}

// e2eTokens is the token usage the fake upstream reports for a successful
// request, already in the tracker's non-overlapping buckets.
type e2eTokens struct {
	InputUncached   int64 `json:"input_uncached"`
	InputCacheRead  int64 `json:"input_cache_read"`
	InputCacheWrite int64 `json:"input_cache_write"`
	OutputReasoning int64 `json:"output_reasoning"`
	OutputOther     int64 `json:"output_other"`
	Unclassified    int64 `json:"unclassified"`
	Total           int64 `json:"total"`
}

func (a e2eTokens) add(b e2eTokens) e2eTokens {
	return e2eTokens{
		InputUncached:   a.InputUncached + b.InputUncached,
		InputCacheRead:  a.InputCacheRead + b.InputCacheRead,
		InputCacheWrite: a.InputCacheWrite + b.InputCacheWrite,
		OutputReasoning: a.OutputReasoning + b.OutputReasoning,
		OutputOther:     a.OutputOther + b.OutputOther,
		Unclassified:    a.Unclassified + b.Unclassified,
		Total:           a.Total + b.Total,
	}
}

// e2eTokensFor derives deterministic usage from the request index. Claude
// reports cache writes, the OpenAI-compatible upstream reports reasoning.
func e2eTokensFor(index int, provider string) e2eTokens {
	tokens := e2eTokens{
		InputUncached:  int64(100 + 13*index),
		InputCacheRead: int64(20 * (index % 3)),
		OutputOther:    int64(30 + 7*index),
	}
	if provider == e2eClaudeProvider {
		tokens.InputCacheWrite = int64(15 * (1 + index%2))
	} else {
		tokens.OutputReasoning = int64(5 * (1 + index%4))
	}
	tokens.Total = tokens.InputUncached + tokens.InputCacheRead + tokens.InputCacheWrite + tokens.OutputReasoning + tokens.OutputOther
	return tokens
}

// e2eAttempt is one upstream attempt as the fake upstream observed it.
type e2eAttempt struct {
	Request  int    `json:"request"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Account  string `json:"account"`
	Status   int    `json:"status"`
}

// e2eLedgerRow is the expected tracker view of one attempt.
type e2eLedgerRow struct {
	Request    int       `json:"request"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Account    string    `json:"account"`
	APIKeyHash string    `json:"api_key_hash"`
	APIKeyHint string    `json:"api_key_hint"`
	Stream     bool      `json:"stream"`
	Failed     bool      `json:"failed"`
	StatusCode int       `json:"status_code"`
	Tokens     e2eTokens `json:"tokens"`
}

type e2eTotals struct {
	Requests int64     `json:"requests"`
	Failures int64     `json:"failures"`
	Tokens   e2eTokens `json:"tokens"`
}

func (t e2eTotals) addRow(row e2eLedgerRow) e2eTotals {
	t.Requests++
	if row.Failed {
		t.Failures++
	}
	t.Tokens = t.Tokens.add(row.Tokens)
	return t
}

// e2eLedger is the Go-side record of what the harness sent and the upstream saw.
type e2eLedger struct {
	Script []e2eRequest   `json:"script"`
	Rows   []e2eLedgerRow `json:"rows"`
	Seen   map[string]int `json:"attempts_per_request"`
}

func (l e2eLedger) total() e2eTotals {
	var total e2eTotals
	for _, row := range l.Rows {
		total = total.addRow(row)
	}
	return total
}

// by groups ledger rows by the joined values of key.
func (l e2eLedger) by(key func(e2eLedgerRow) string) map[string]e2eTotals {
	out := map[string]e2eTotals{}
	for _, row := range l.Rows {
		k := key(row)
		out[k] = out[k].addRow(row)
	}
	return out
}

func e2eHashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func e2eKeyHint(key string) string {
	return key[:4] + "…" + key[len(key)-4:]
}

var e2eRequestTag = regexp.MustCompile(`e2e-req-(\d+)`)

// e2eUpstream serves both fake providers and records every attempt.
type e2eUpstream struct {
	t      *testing.T
	script []e2eRequest

	mu       sync.Mutex
	attempts []e2eAttempt
}

func (u *e2eUpstream) record(attempt e2eAttempt) {
	u.mu.Lock()
	u.attempts = append(u.attempts, attempt)
	u.mu.Unlock()
}

func (u *e2eUpstream) snapshot() []e2eAttempt {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.attempts)
}

func (u *e2eUpstream) reset() {
	u.mu.Lock()
	u.attempts = nil
	u.mu.Unlock()
}

// parse resolves the scripted request and the credential from an upstream call.
func (u *e2eUpstream) parse(r *http.Request, provider string, body []byte) (int, e2eRequest, string, bool) {
	match := e2eRequestTag.FindSubmatch(body)
	if match == nil {
		u.t.Errorf("%s upstream: request body carries no e2e-req tag: %s", provider, body)
		return 0, e2eRequest{}, "", false
	}
	index, errAtoi := strconv.Atoi(string(match[1]))
	if errAtoi != nil || index < 0 || index >= len(u.script) {
		u.t.Errorf("%s upstream: bad request tag %q", provider, match[0])
		return 0, e2eRequest{}, "", false
	}
	key := r.Header.Get("x-api-key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	account := ""
	for _, cred := range e2eCredentials {
		if cred.Provider == provider && cred.UpstreamKey == key {
			account = cred.ID
		}
	}
	if account == "" {
		u.t.Errorf("%s upstream: unknown credential key %q", provider, key)
		return 0, e2eRequest{}, "", false
	}
	return index, u.script[index], account, true
}

func (u *e2eUpstream) claudeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		u.t.Errorf("claude upstream: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	index, req, account, ok := u.parse(r, e2eClaudeProvider, body)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u.record(e2eAttempt{Request: index, Provider: e2eClaudeProvider, Model: req.Model, Account: account, Status: req.Status})
	if req.Status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(req.Status)
		_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"e2e_scripted_error","message":"scripted status %d"}}`, req.Status)
		return
	}
	tokens := e2eTokensFor(index, e2eClaudeProvider)
	usageJSON := fmt.Sprintf(`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":%d}`,
		tokens.InputUncached, tokens.InputCacheRead, tokens.InputCacheWrite, tokens.OutputOther)
	if !gjson.GetBytes(body, "stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_e2e_%d","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":%s}`,
			index, req.Model, usageJSON)
		return
	}
	startUsage := fmt.Sprintf(`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":1}`,
		tokens.InputUncached, tokens.InputCacheRead, tokens.InputCacheWrite)
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		fmt.Sprintf(`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_e2e_%d","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":%s}}`, index, req.Model, startUsage),
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":%s}`, usageJSON),
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
	for _, event := range events {
		_, _ = io.WriteString(w, event+"\n\n")
	}
}

func (u *e2eUpstream) compatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		u.t.Errorf("compat upstream: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	index, req, account, ok := u.parse(r, e2eCompatProvider, body)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u.record(e2eAttempt{Request: index, Provider: e2eCompatProvider, Model: req.Model, Account: account, Status: req.Status})
	if req.Status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(req.Status)
		_, _ = fmt.Fprintf(w, `{"error":{"type":"e2e_scripted_error","message":"scripted status %d"}}`, req.Status)
		return
	}
	tokens := e2eTokensFor(index, e2eCompatProvider)
	// OpenAI usage nests cached input in prompt_tokens and reasoning in completion_tokens.
	prompt := tokens.InputUncached + tokens.InputCacheRead
	completion := tokens.OutputOther + tokens.OutputReasoning
	usageJSON := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d},"completion_tokens_details":{"reasoning_tokens":%d}}`,
		prompt, completion, prompt+completion, tokens.InputCacheRead, tokens.OutputReasoning)
	if !gjson.GetBytes(body, "stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-e2e-%d","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":%s}`,
			index, req.Model, usageJSON)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunks := []string{
		fmt.Sprintf(`{"id":"chatcmpl-e2e-%d","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`, index, req.Model),
		fmt.Sprintf(`{"id":"chatcmpl-e2e-%d","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, index, req.Model),
		fmt.Sprintf(`{"id":"chatcmpl-e2e-%d","object":"chat.completion.chunk","created":1,"model":%q,"choices":[],"usage":%s}`, index, req.Model, usageJSON),
		`[DONE]`,
	}
	for _, chunk := range chunks {
		_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
	}
}

// e2eProbe counts records the usage dispatcher delivers. Plugins run serially
// per record, so once the probe has seen a record every earlier plugin
// (including the tracker) has handled it.
type e2eProbe struct {
	mu      sync.Mutex
	records []usage.Record
}

func (p *e2eProbe) HandleUsage(_ context.Context, record usage.Record) {
	p.mu.Lock()
	p.records = append(p.records, record)
	p.mu.Unlock()
}

func (p *e2eProbe) snapshot() []usage.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.records)
}

type e2eHarnessOptions struct {
	trackerEnabled bool
	homeEnabled    bool
	script         []e2eRequest
}

type e2eHarness struct {
	t          *testing.T
	dir        string
	authDir    string
	configPath string
	server     *api.Server
	proxy      *httptest.Server
	upstream   *e2eUpstream
	probe      *e2eProbe
	reloads    chan error

	bodiesMu sync.Mutex
	bodies   map[string][]byte
}

// newE2EHarness starts the proxy (api.Server) on a real listener with a temp
// auth dir, a management secret, two client keys, and fake claude and
// OpenAI-compatible upstreams. It avoids cliproxy.Service because
// Service.Shutdown calls usage.StopDefault, which closes the process-wide
// usage dispatcher for every later test in this binary.
func newE2EHarness(t *testing.T, opts e2eHarnessOptions) *e2eHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	h := &e2eHarness{
		t:          t,
		dir:        dir,
		authDir:    filepath.Join(dir, "auths"),
		configPath: filepath.Join(dir, "config.yaml"),
		upstream:   &e2eUpstream{t: t, script: opts.script},
		probe:      &e2eProbe{},
		reloads:    make(chan error, 8),
		bodies:     map[string][]byte{},
	}
	if errMkdir := os.MkdirAll(h.authDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}

	claudeUpstream := httptest.NewServer(http.HandlerFunc(h.upstream.claudeHandler))
	t.Cleanup(claudeUpstream.Close)
	compatUpstream := httptest.NewServer(http.HandlerFunc(h.upstream.compatHandler))
	t.Cleanup(compatUpstream.Close)

	h.writeConfig(opts.trackerEnabled)
	cfg, errLoad := config.LoadConfig(h.configPath)
	if errLoad != nil {
		t.Fatalf("load config: %v", errLoad)
	}
	cfg.Home.Enabled = opts.homeEnabled

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor(e2eCompatProvider, cfg))
	baseURLs := map[string]string{e2eClaudeProvider: claudeUpstream.URL, e2eCompatProvider: compatUpstream.URL + "/v1"}
	for _, cred := range e2eCredentials {
		models := make([]*registry.ModelInfo, 0, len(e2eProviderModels[cred.Provider]))
		for _, model := range e2eProviderModels[cred.Provider] {
			models = append(models, &registry.ModelInfo{ID: model, OwnedBy: cred.Provider})
		}
		registry.GetGlobalRegistry().RegisterClient(cred.ID, cred.Provider, models)
		id := cred.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
			ID:         cred.ID,
			FileName:   cred.ID,
			Provider:   cred.Provider,
			Status:     cliproxyauth.StatusActive,
			Attributes: map[string]string{"api_key": cred.UpstreamKey, "base_url": baseURLs[cred.Provider]},
			Metadata:   map[string]any{"disable_cooling": true},
		}); errRegister != nil {
			t.Fatalf("register credential %s: %v", cred.ID, errRegister)
		}
	}

	var server *api.Server
	// Like the Service hook, a management save reloads config.yaml from disk.
	reloadHook := api.WithConfigReloadHook(func(ctx context.Context, _ *config.Config) {
		loaded, errReload := config.LoadConfig(h.configPath)
		if errReload == nil {
			loaded.Home.Enabled = opts.homeEnabled
			server.UpdateClientsContext(ctx, loaded)
		}
		h.reloads <- errReload
	})
	server = api.NewServer(cfg, manager, sdkaccess.NewManager(), h.configPath, reloadHook)
	h.server = server
	server.StartUsageTracker(context.Background())
	t.Cleanup(server.CloseUsageTracker)

	usage.RegisterNamedPlugin(e2eProbePluginName, h.probe)
	t.Cleanup(func() { usage.RegisterNamedPlugin(e2eProbePluginName, &e2eProbe{}) })

	h.proxy = httptest.NewServer(server.Handler())
	t.Cleanup(h.proxy.Close)
	return h
}

func (h *e2eHarness) writeConfig(trackerEnabled bool) {
	h.t.Helper()
	hashed, errHash := bcrypt.GenerateFromPassword([]byte(e2eManagementSecret), bcrypt.MinCost)
	if errHash != nil {
		h.t.Fatal(errHash)
	}
	raw := fmt.Sprintf(`config-version: 8
server:
  port: 0
management:
  secret-key: %q
access:
  api-keys:
    - %q
    - %q
oauth:
  auth-dir: %q
routing:
  retry:
    request-retry: 0
    max-retry-interval: 0
  cooldown:
    disable-cooling: true
observability:
  usage:
    usage-statistics-enabled: true
    tracker:
      enabled: %t
`, string(hashed), e2eClientKeys[0], e2eClientKeys[1], filepath.ToSlash(h.authDir), trackerEnabled)
	if errWrite := os.WriteFile(h.configPath, []byte(raw), 0o600); errWrite != nil {
		h.t.Fatal(errWrite)
	}
}

func (h *e2eHarness) dbPath() string {
	return filepath.Join(h.authDir, config.DefaultUsageTrackerFileName)
}

// chat sends one scripted request through the OpenAI-compatible endpoint and
// returns the HTTP status after reading the whole body.
func (h *e2eHarness) chat(index int, req e2eRequest) (int, []byte) {
	h.t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"model":    req.Model,
		"stream":   req.Stream,
		"messages": []map[string]any{{"role": "user", "content": fmt.Sprintf("hello e2e-req-%d", index)}},
	})
	httpReq, errReq := http.NewRequest(http.MethodPost, h.proxy.URL+"/v1/chat/completions", bytes.NewReader(payload))
	if errReq != nil {
		h.t.Fatal(errReq)
	}
	httpReq.Header.Set("Authorization", "Bearer "+e2eClientKeys[req.Key])
	httpReq.Header.Set("Content-Type", "application/json")
	resp, errDo := http.DefaultClient.Do(httpReq)
	if errDo != nil {
		h.t.Fatalf("request %d: %v", index, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			h.t.Errorf("close response body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		h.t.Fatalf("request %d: read body: %v", index, errRead)
	}
	return resp.StatusCode, body
}

// management calls a v8 management endpoint and keeps the body for the
// raw-key scan.
func (h *e2eHarness) management(method, path string, body any) (int, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			h.t.Fatal(errMarshal)
		}
		reader = bytes.NewReader(encoded)
	}
	req, errReq := http.NewRequest(method, h.proxy.URL+"/v8/management/observability/usage/"+path, reader)
	if errReq != nil {
		h.t.Fatal(errReq)
	}
	req.Header.Set("Authorization", "Bearer "+e2eManagementSecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		h.t.Fatalf("%s %s: %v", method, path, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			h.t.Errorf("close response body: %v", errClose)
		}
	}()
	out, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		h.t.Fatalf("%s %s: read body: %v", method, path, errRead)
	}
	h.bodiesMu.Lock()
	h.bodies[method+" "+path] = out
	h.bodiesMu.Unlock()
	return resp.StatusCode, out
}

// getJSON calls a management GET, requires 200, and decodes into out.
func (h *e2eHarness) getJSON(path string, out any) []byte {
	h.t.Helper()
	status, body := h.management(http.MethodGet, path, nil)
	if status != http.StatusOK {
		h.t.Fatalf("GET %s: status %d, body %s", path, status, body)
	}
	if errDecode := json.Unmarshal(body, out); errDecode != nil {
		h.t.Fatalf("GET %s: decode: %v; body %s", path, errDecode, body)
	}
	return body
}

// waitFor polls cond over a short real clock. This is an integration test
// against the tracker's one-second batch ticker, so a bounded poll is the flush.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The two rollup queries below are copied from internal/usagestore/helpers_test.go.
const e2eHourlyFromEventsSQL = `SELECT hour_start, provider, model, requested_model, account, api_key_hash,
    MAX(api_key_hint), reasoning_effort, service_tier, stream,
    COUNT(*), SUM(failed), SUM(input_uncached), SUM(input_cache_read), SUM(input_cache_write),
    SUM(output_reasoning), SUM(output_other), SUM(unclassified), SUM(total_tokens),
    SUM(latency_ms), MAX(latency_ms), SUM(ttft_ms), MAX(ttft_ms), SUM(ttft_ms > 0)
FROM usage_events
GROUP BY hour_start, provider, model, requested_model, account, api_key_hash, reasoning_effort, service_tier, stream`

const e2eHourlyStoredSQL = `SELECT hour_start, provider, model, requested_model, account, api_key_hash,
    api_key_hint, reasoning_effort, service_tier, stream,
    requests, failures, input_uncached, input_cache_read, input_cache_write,
    output_reasoning, output_other, unclassified, total_tokens,
    latency_ms_sum, latency_ms_max, ttft_ms_sum, ttft_ms_max, ttft_count
FROM usage_hourly`

func openE2EReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, errOpen := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if errOpen != nil {
		t.Fatalf("open %s read-only: %v", path, errOpen)
	}
	t.Cleanup(func() {
		if errClose := db.Close(); errClose != nil {
			t.Errorf("close read-only db: %v", errClose)
		}
	})
	return db
}

func e2eQueryInt(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if errScan := db.QueryRow(query).Scan(&n); errScan != nil {
		t.Fatalf("%s: %v", query, errScan)
	}
	return n
}

func e2eQueryRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, errQuery := db.Query(query)
	if errQuery != nil {
		t.Fatalf("%s: %v", query, errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			t.Errorf("rows.Close() error = %v", errClose)
		}
	}()
	cols, errCols := rows.Columns()
	if errCols != nil {
		t.Fatal(errCols)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if errScan := rows.Scan(dest...); errScan != nil {
			t.Fatal(errScan)
		}
		fields := make([]string, len(values))
		for i, value := range values {
			fields[i] = fmt.Sprint(value)
		}
		out = append(out, strings.Join(fields, " | "))
	}
	if errRows := rows.Err(); errRows != nil {
		t.Fatal(errRows)
	}
	return out
}

// assertE2ERollupsMatchEvents checks usage_hourly equals usage_events grouped by
// the hourly key, in both directions, for every counter, sum, and max column.
func assertE2ERollupsMatchEvents(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, diff := range []struct{ name, query string }{
		{"events not matched by hourly", e2eHourlyFromEventsSQL + " EXCEPT " + e2eHourlyStoredSQL},
		{"hourly not matched by events", e2eHourlyStoredSQL + " EXCEPT " + e2eHourlyFromEventsSQL},
	} {
		if rows := e2eQueryRows(t, db, diff.query); len(rows) > 0 {
			t.Fatalf("%s:\n%s", diff.name, strings.Join(rows, "\n"))
		}
	}
	grouped := e2eQueryInt(t, db, "SELECT COUNT(*) FROM ("+e2eHourlyFromEventsSQL+")")
	if stored := e2eQueryInt(t, db, "SELECT COUNT(*) FROM usage_hourly"); stored != grouped {
		t.Fatalf("usage_hourly has %d rows, events group into %d", stored, grouped)
	}
}

// writeE2EEvidence writes name under $USAGE_TRACKER_EVIDENCE_DIR when it is set.
func writeE2EEvidence(t *testing.T, name string, data []byte) {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv(e2eEvidenceEnv))
	if dir == "" {
		return
	}
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		t.Fatalf("create evidence dir: %v", errMkdir)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, name), data, 0o644); errWrite != nil {
		t.Fatalf("write evidence %s: %v", name, errWrite)
	}
}
