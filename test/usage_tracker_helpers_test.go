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
	s5ManagementSecret = "s5-management-secret-value"
	s5ClaudeProvider   = "claude"
	s5CompatProvider   = "s5-compat"
	s5ProbePluginName  = "s5-usage-probe"
	s5EvidenceEnv      = "USAGE_TRACKER_EVIDENCE_DIR"
)

// Client keys are long enough that the stored hint masks four runes per side.
var s5ClientKeys = []string{
	"sk-s5-client-alpha-7f3a9c2e5b1d4f60",
	"sk-s5-client-bravo-2c8e4a6f0d9b3e71",
}

// s5Credential is one upstream credential. IDs use the auth-file naming that
// file-backed credentials get (relative file name), but they are registered
// directly with a base_url: file auths cannot point claude or openai-compat
// executors at a fake upstream.
type s5Credential struct {
	ID          string
	Provider    string
	UpstreamKey string
}

var s5Credentials = []s5Credential{
	{ID: "claude-alpha.json", Provider: s5ClaudeProvider, UpstreamKey: "s5-upstream-claude-alpha-secret"},
	{ID: "claude-bravo.json", Provider: s5ClaudeProvider, UpstreamKey: "s5-upstream-claude-bravo-secret"},
	{ID: "s5-compat-main.json", Provider: s5CompatProvider, UpstreamKey: "s5-upstream-compat-main-secret"},
}

var s5ProviderModels = map[string][]string{
	s5ClaudeProvider: {"s5-claude-opus", "s5-claude-haiku"},
	s5CompatProvider: {"s5-compat-large", "s5-compat-small"},
}

// s5Request is one scripted client request. Status is what the fake upstream
// answers for every attempt of the request; 200 means success.
type s5Request struct {
	Provider string
	Model    string
	Key      int
	Stream   bool
	Status   int
}

// s5Tokens is the token usage the fake upstream reports for a successful
// request, already in the tracker's non-overlapping buckets.
type s5Tokens struct {
	InputUncached   int64 `json:"input_uncached"`
	InputCacheRead  int64 `json:"input_cache_read"`
	InputCacheWrite int64 `json:"input_cache_write"`
	OutputReasoning int64 `json:"output_reasoning"`
	OutputOther     int64 `json:"output_other"`
	Unclassified    int64 `json:"unclassified"`
	Total           int64 `json:"total"`
}

func (a s5Tokens) add(b s5Tokens) s5Tokens {
	return s5Tokens{
		InputUncached:   a.InputUncached + b.InputUncached,
		InputCacheRead:  a.InputCacheRead + b.InputCacheRead,
		InputCacheWrite: a.InputCacheWrite + b.InputCacheWrite,
		OutputReasoning: a.OutputReasoning + b.OutputReasoning,
		OutputOther:     a.OutputOther + b.OutputOther,
		Unclassified:    a.Unclassified + b.Unclassified,
		Total:           a.Total + b.Total,
	}
}

// s5TokensFor derives deterministic usage from the request index. Claude
// reports cache writes, the OpenAI-compatible upstream reports reasoning.
func s5TokensFor(index int, provider string) s5Tokens {
	tokens := s5Tokens{
		InputUncached:  int64(100 + 13*index),
		InputCacheRead: int64(20 * (index % 3)),
		OutputOther:    int64(30 + 7*index),
	}
	if provider == s5ClaudeProvider {
		tokens.InputCacheWrite = int64(15 * (1 + index%2))
	} else {
		tokens.OutputReasoning = int64(5 * (1 + index%4))
	}
	tokens.Total = tokens.InputUncached + tokens.InputCacheRead + tokens.InputCacheWrite + tokens.OutputReasoning + tokens.OutputOther
	return tokens
}

// s5Attempt is one upstream attempt as the fake upstream observed it.
type s5Attempt struct {
	Request  int    `json:"request"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Account  string `json:"account"`
	Status   int    `json:"status"`
}

// s5LedgerRow is the expected tracker view of one attempt.
type s5LedgerRow struct {
	Request    int      `json:"request"`
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	Account    string   `json:"account"`
	APIKeyHash string   `json:"api_key_hash"`
	APIKeyHint string   `json:"api_key_hint"`
	Stream     bool     `json:"stream"`
	Failed     bool     `json:"failed"`
	StatusCode int      `json:"status_code"`
	Tokens     s5Tokens `json:"tokens"`
}

type s5Totals struct {
	Requests int64    `json:"requests"`
	Failures int64    `json:"failures"`
	Tokens   s5Tokens `json:"tokens"`
}

func (t s5Totals) addRow(row s5LedgerRow) s5Totals {
	t.Requests++
	if row.Failed {
		t.Failures++
	}
	t.Tokens = t.Tokens.add(row.Tokens)
	return t
}

// s5Ledger is the Go-side record of what the harness sent and the upstream saw.
type s5Ledger struct {
	Script []s5Request    `json:"script"`
	Rows   []s5LedgerRow  `json:"rows"`
	Seen   map[string]int `json:"attempts_per_request"`
}

func (l s5Ledger) total() s5Totals {
	var total s5Totals
	for _, row := range l.Rows {
		total = total.addRow(row)
	}
	return total
}

// by groups ledger rows by the joined values of key.
func (l s5Ledger) by(key func(s5LedgerRow) string) map[string]s5Totals {
	out := map[string]s5Totals{}
	for _, row := range l.Rows {
		k := key(row)
		out[k] = out[k].addRow(row)
	}
	return out
}

func s5HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func s5KeyHint(key string) string {
	return key[:4] + "…" + key[len(key)-4:]
}

var s5RequestTag = regexp.MustCompile(`s5-req-(\d+)`)

// s5Upstream serves both fake providers and records every attempt.
type s5Upstream struct {
	t      *testing.T
	script []s5Request

	mu       sync.Mutex
	attempts []s5Attempt
}

func (u *s5Upstream) record(attempt s5Attempt) {
	u.mu.Lock()
	u.attempts = append(u.attempts, attempt)
	u.mu.Unlock()
}

func (u *s5Upstream) snapshot() []s5Attempt {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.attempts)
}

func (u *s5Upstream) reset() {
	u.mu.Lock()
	u.attempts = nil
	u.mu.Unlock()
}

// parse resolves the scripted request and the credential from an upstream call.
func (u *s5Upstream) parse(r *http.Request, provider string, body []byte) (int, s5Request, string, bool) {
	match := s5RequestTag.FindSubmatch(body)
	if match == nil {
		u.t.Errorf("%s upstream: request body carries no s5-req tag: %s", provider, body)
		return 0, s5Request{}, "", false
	}
	index, errAtoi := strconv.Atoi(string(match[1]))
	if errAtoi != nil || index < 0 || index >= len(u.script) {
		u.t.Errorf("%s upstream: bad request tag %q", provider, match[0])
		return 0, s5Request{}, "", false
	}
	key := r.Header.Get("x-api-key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	account := ""
	for _, cred := range s5Credentials {
		if cred.Provider == provider && cred.UpstreamKey == key {
			account = cred.ID
		}
	}
	if account == "" {
		u.t.Errorf("%s upstream: unknown credential key %q", provider, key)
		return 0, s5Request{}, "", false
	}
	return index, u.script[index], account, true
}

func (u *s5Upstream) claudeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		u.t.Errorf("claude upstream: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	index, req, account, ok := u.parse(r, s5ClaudeProvider, body)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u.record(s5Attempt{Request: index, Provider: s5ClaudeProvider, Model: req.Model, Account: account, Status: req.Status})
	if req.Status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(req.Status)
		_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"s5_scripted_error","message":"scripted status %d"}}`, req.Status)
		return
	}
	tokens := s5TokensFor(index, s5ClaudeProvider)
	usageJSON := fmt.Sprintf(`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":%d}`,
		tokens.InputUncached, tokens.InputCacheRead, tokens.InputCacheWrite, tokens.OutputOther)
	if !gjson.GetBytes(body, "stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_s5_%d","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":%s}`,
			index, req.Model, usageJSON)
		return
	}
	startUsage := fmt.Sprintf(`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":1}`,
		tokens.InputUncached, tokens.InputCacheRead, tokens.InputCacheWrite)
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		fmt.Sprintf(`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_s5_%d","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":%s}}`, index, req.Model, startUsage),
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

func (u *s5Upstream) compatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		u.t.Errorf("compat upstream: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	index, req, account, ok := u.parse(r, s5CompatProvider, body)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u.record(s5Attempt{Request: index, Provider: s5CompatProvider, Model: req.Model, Account: account, Status: req.Status})
	if req.Status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(req.Status)
		_, _ = fmt.Fprintf(w, `{"error":{"type":"s5_scripted_error","message":"scripted status %d"}}`, req.Status)
		return
	}
	tokens := s5TokensFor(index, s5CompatProvider)
	// OpenAI usage nests cached input in prompt_tokens and reasoning in completion_tokens.
	prompt := tokens.InputUncached + tokens.InputCacheRead
	completion := tokens.OutputOther + tokens.OutputReasoning
	usageJSON := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d},"completion_tokens_details":{"reasoning_tokens":%d}}`,
		prompt, completion, prompt+completion, tokens.InputCacheRead, tokens.OutputReasoning)
	if !gjson.GetBytes(body, "stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-s5-%d","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":%s}`,
			index, req.Model, usageJSON)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunks := []string{
		fmt.Sprintf(`{"id":"chatcmpl-s5-%d","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`, index, req.Model),
		fmt.Sprintf(`{"id":"chatcmpl-s5-%d","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, index, req.Model),
		fmt.Sprintf(`{"id":"chatcmpl-s5-%d","object":"chat.completion.chunk","created":1,"model":%q,"choices":[],"usage":%s}`, index, req.Model, usageJSON),
		`[DONE]`,
	}
	for _, chunk := range chunks {
		_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
	}
}

// s5Probe counts records the usage dispatcher delivers. Plugins run serially
// per record, so once the probe has seen a record every earlier plugin
// (including the tracker) has handled it.
type s5Probe struct {
	mu      sync.Mutex
	records []usage.Record
}

func (p *s5Probe) HandleUsage(_ context.Context, record usage.Record) {
	p.mu.Lock()
	p.records = append(p.records, record)
	p.mu.Unlock()
}

func (p *s5Probe) snapshot() []usage.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.records)
}

type s5HarnessOptions struct {
	trackerEnabled bool
	homeEnabled    bool
	script         []s5Request
}

type s5Harness struct {
	t          *testing.T
	dir        string
	authDir    string
	configPath string
	server     *api.Server
	proxy      *httptest.Server
	upstream   *s5Upstream
	probe      *s5Probe
	reloads    chan error

	bodiesMu sync.Mutex
	bodies   map[string][]byte
}

// newS5Harness starts the proxy (api.Server) on a real listener with a temp
// auth dir, a management secret, two client keys, and fake claude and
// OpenAI-compatible upstreams. It deliberately avoids cliproxy.Service:
// Service.Shutdown calls usage.StopDefault, which closes the process-wide
// usage dispatcher for every later test in this binary.
func newS5Harness(t *testing.T, opts s5HarnessOptions) *s5Harness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	h := &s5Harness{
		t:          t,
		dir:        dir,
		authDir:    filepath.Join(dir, "auths"),
		configPath: filepath.Join(dir, "config.yaml"),
		upstream:   &s5Upstream{t: t, script: opts.script},
		probe:      &s5Probe{},
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
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor(s5CompatProvider, cfg))
	baseURLs := map[string]string{s5ClaudeProvider: claudeUpstream.URL, s5CompatProvider: compatUpstream.URL + "/v1"}
	for _, cred := range s5Credentials {
		models := make([]*registry.ModelInfo, 0, len(s5ProviderModels[cred.Provider]))
		for _, model := range s5ProviderModels[cred.Provider] {
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
	// Mirrors the Service hook: a management save reloads config.yaml from disk.
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

	usage.RegisterNamedPlugin(s5ProbePluginName, h.probe)
	t.Cleanup(func() { usage.RegisterNamedPlugin(s5ProbePluginName, &s5Probe{}) })

	h.proxy = httptest.NewServer(server.Handler())
	t.Cleanup(h.proxy.Close)
	return h
}

func (h *s5Harness) writeConfig(trackerEnabled bool) {
	h.t.Helper()
	hashed, errHash := bcrypt.GenerateFromPassword([]byte(s5ManagementSecret), bcrypt.MinCost)
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
`, string(hashed), s5ClientKeys[0], s5ClientKeys[1], filepath.ToSlash(h.authDir), trackerEnabled)
	if errWrite := os.WriteFile(h.configPath, []byte(raw), 0o600); errWrite != nil {
		h.t.Fatal(errWrite)
	}
}

func (h *s5Harness) dbPath() string {
	return filepath.Join(h.authDir, config.DefaultUsageTrackerFileName)
}

// chat sends one scripted request through the OpenAI-compatible endpoint and
// returns the HTTP status after reading the whole body.
func (h *s5Harness) chat(index int, req s5Request) (int, []byte) {
	h.t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"model":    req.Model,
		"stream":   req.Stream,
		"messages": []map[string]any{{"role": "user", "content": fmt.Sprintf("hello s5-req-%d", index)}},
	})
	httpReq, errReq := http.NewRequest(http.MethodPost, h.proxy.URL+"/v1/chat/completions", bytes.NewReader(payload))
	if errReq != nil {
		h.t.Fatal(errReq)
	}
	httpReq.Header.Set("Authorization", "Bearer "+s5ClientKeys[req.Key])
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
func (h *s5Harness) management(method, path string, body any) (int, []byte) {
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
	req.Header.Set("Authorization", "Bearer "+s5ManagementSecret)
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
func (h *s5Harness) getJSON(path string, out any) []byte {
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
const s5HourlyFromEventsSQL = `SELECT hour_start, provider, model, requested_model, account, api_key_hash,
    MAX(api_key_hint), reasoning_effort, service_tier, stream,
    COUNT(*), SUM(failed), SUM(input_uncached), SUM(input_cache_read), SUM(input_cache_write),
    SUM(output_reasoning), SUM(output_other), SUM(unclassified), SUM(total_tokens),
    SUM(latency_ms), MAX(latency_ms), SUM(ttft_ms), MAX(ttft_ms), SUM(ttft_ms > 0)
FROM usage_events
GROUP BY hour_start, provider, model, requested_model, account, api_key_hash, reasoning_effort, service_tier, stream`

const s5HourlyStoredSQL = `SELECT hour_start, provider, model, requested_model, account, api_key_hash,
    api_key_hint, reasoning_effort, service_tier, stream,
    requests, failures, input_uncached, input_cache_read, input_cache_write,
    output_reasoning, output_other, unclassified, total_tokens,
    latency_ms_sum, latency_ms_max, ttft_ms_sum, ttft_ms_max, ttft_count
FROM usage_hourly`

func openS5ReadOnly(t *testing.T, path string) *sql.DB {
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

func s5QueryInt(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if errScan := db.QueryRow(query).Scan(&n); errScan != nil {
		t.Fatalf("%s: %v", query, errScan)
	}
	return n
}

func s5QueryRows(t *testing.T, db *sql.DB, query string) []string {
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

// assertS5RollupsMatchEvents checks usage_hourly equals usage_events grouped by
// the hourly key, in both directions, for every counter, sum, and max column.
func assertS5RollupsMatchEvents(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, diff := range []struct{ name, query string }{
		{"events not matched by hourly", s5HourlyFromEventsSQL + " EXCEPT " + s5HourlyStoredSQL},
		{"hourly not matched by events", s5HourlyStoredSQL + " EXCEPT " + s5HourlyFromEventsSQL},
	} {
		if rows := s5QueryRows(t, db, diff.query); len(rows) > 0 {
			t.Fatalf("%s:\n%s", diff.name, strings.Join(rows, "\n"))
		}
	}
	grouped := s5QueryInt(t, db, "SELECT COUNT(*) FROM ("+s5HourlyFromEventsSQL+")")
	if stored := s5QueryInt(t, db, "SELECT COUNT(*) FROM usage_hourly"); stored != grouped {
		t.Fatalf("usage_hourly has %d rows, events group into %d", stored, grouped)
	}
}

// writeS5Evidence writes name under $USAGE_TRACKER_EVIDENCE_DIR when it is set.
func writeS5Evidence(t *testing.T, name string, data []byte) {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv(s5EvidenceEnv))
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
