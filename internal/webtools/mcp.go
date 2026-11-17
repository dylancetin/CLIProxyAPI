package webtools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// A minimal MCP (Model Context Protocol) client: hosted search MCP servers
// answer a single stateless JSON-RPC "tools/call" POST, either as plain JSON
// or as a short server-sent event stream. No MCP SDK is needed for that.

const (
	mcpAccept          = "application/json, text/event-stream"
	mcpProtocolVersion = "2025-06-18"
)

// mcpToolResult is the result of tools/call.
type mcpToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// text returns the first non-empty text content.
func (r *mcpToolResult) text() string {
	for _, c := range r.Content {
		if strings.TrimSpace(c.Text) != "" {
			return c.Text
		}
	}
	return ""
}

type mcpEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// mcpPost sends one JSON-RPC message and returns the response and its body.
func mcpPost(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, message any) (*http.Response, []byte, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	req.Header.Set("User-Agent", UserAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, body, statusError(resp)
	}
	return resp, body, nil
}

// mcpCallTool calls one tool and returns its result. Tool-level failures
// (isError, JSON-RPC errors) become providerErrors; ones that read like a
// rate limit or a missing key get status 429 / 401 so the router treats them
// like the HTTP equivalents.
func mcpCallTool(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, tool string, args any) (*mcpToolResult, error) {
	_, body, err := mcpPost(ctx, client, endpoint, headers, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return nil, err
	}
	env, ok := parseMCPBody(body)
	if !ok {
		return nil, &providerError{msg: "the provider returned an invalid MCP response"}
	}
	if env.Error != nil {
		return nil, mcpFailure(env.Error.Message)
	}
	var result mcpToolResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, &providerError{msg: "the provider returned an invalid MCP result"}
	}
	if result.IsError {
		return nil, mcpFailure(result.text())
	}
	return &result, nil
}

// mcpSession opens a session (initialize + notifications/initialized) for
// servers that require the Mcp-Session-Id header, and returns its id.
func mcpSession(ctx context.Context, client *http.Client, endpoint string, headers map[string]string) (string, error) {
	resp, _, err := mcpPost(ctx, client, endpoint, headers, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": clientName, "version": appVersion()},
		},
	})
	if err != nil {
		return "", err
	}
	session := resp.Header.Get("Mcp-Session-Id")
	if session == "" {
		return "", &providerError{msg: "the provider did not open an MCP session"}
	}
	next := map[string]string{"Mcp-Session-Id": session}
	for k, v := range headers {
		next[k] = v
	}
	if _, _, err := mcpPost(ctx, client, endpoint, next, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}); err != nil {
		return "", err
	}
	return session, nil
}

// parseMCPBody reads a JSON-RPC response sent as plain JSON or as SSE
// "data:" lines (the first data payload with a result or error wins).
func parseMCPBody(body []byte) (mcpEnvelope, bool) {
	try := func(payload []byte) (mcpEnvelope, bool) {
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || payload[0] != '{' {
			return mcpEnvelope{}, false
		}
		var env mcpEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			return mcpEnvelope{}, false
		}
		return env, env.Error != nil || len(env.Result) > 0
	}
	if env, ok := try(body); ok {
		return env, true
	}
	// SSE: an event's data may span several "data:" lines, joined by "\n".
	var data []byte
	flush := func() (mcpEnvelope, bool) {
		env, ok := try(data)
		data = data[:0]
		return env, ok
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			if env, ok := flush(); ok {
				return env, true
			}
			continue
		}
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(rest, []byte(" "))...)
		}
	}
	return flush()
}

// mcpFailure classifies a tool error message.
func mcpFailure(message string) *providerError {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many requests"),
		strings.Contains(lower, "429"), strings.Contains(lower, "quota"), strings.Contains(lower, "limit reached"),
		strings.Contains(lower, "limit exceeded"):
		return &providerError{status: http.StatusTooManyRequests, msg: "the provider is rate limiting searches"}
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "api key"), strings.Contains(lower, "invalid key"),
		strings.Contains(lower, "401"):
		return &providerError{status: http.StatusUnauthorized, msg: "the provider refused the API key"}
	}
	return &providerError{msg: "the provider returned an error"}
}
