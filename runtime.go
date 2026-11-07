package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const version = "0.1.2"
const pluginID = "spx-cursor"
const pluginLogo = "https://raw.githubusercontent.com/spancerxing/cliproxyapi-cursor-plugin/main/assets/cursor.png"
const cursorAPI = "https://api2.cursor.sh"
const clientVersion = "cli-2026.09.23-86fc751"
const maxWireBytes = 64 << 20

// Some Cursor team accounts only list models such as Claude Opus when the
// catalog client type is desktop. Agent runs stay cli so Cursor keeps the
// MCP tool protocol this plugin translates.
const agentClientType = "cli"
const catalogClientType = "desktop"

type hostCaller func(string, []byte) ([]byte, error)
type rpcError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}
type message struct {
	Result json.RawMessage
	Error  *rpcError
}
type statusError struct {
	status  int
	message string
}

func (e *statusError) Error() string { return e.message }
func failure(status int, format string, args ...any) error {
	return &statusError{status, fmt.Sprintf(format, args...)}
}
func errorStatus(err error) int {
	var e *statusError
	if errors.As(err, &e) {
		return e.status
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return 502
}
func envelope(m message) []byte {
	v := map[string]any{"ok": m.Error == nil}
	if m.Error != nil {
		v["error"] = m.Error
	} else if len(m.Result) > 0 {
		v["result"] = m.Result
	} else {
		v["result"] = map[string]any{}
	}
	raw, _ := json.Marshal(v)
	return raw
}
func resultEnvelope(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return errorEnvelope(err)
	}
	return envelope(message{Result: raw})
}
func errorEnvelope(err error) []byte {
	return envelope(message{Error: &rpcError{Code: "cursor_error", Message: err.Error(), HTTPStatus: errorStatus(err)}})
}

type rpcRequest struct {
	BasePath         string
	ResourceBasePath string
	Method           string
	Path             string
	Body             []byte

	Provider       string
	AuthID         string
	AuthProvider   string
	FileName       string
	RawJSON        []byte
	StorageJSON    []byte
	Metadata       map[string]any
	AuthMetadata   map[string]any
	Attributes     map[string]string
	AuthAttributes map[string]string
	Host           struct{ ProxyURL string }
	State          string
	Model          string
	Format         string
	Payload        []byte
	CallbackID     string `json:"host_callback_id"`
	StreamID       string `json:"stream_id"`
}

func parseRequest(raw []byte, method string) (rpcRequest, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return rpcRequest{}, failure(400, "Expected a JSON object")
	}
	if strings.HasPrefix(method, "quota.") {
		for old, key := range map[string]string{"storage_json": "StorageJSON", "metadata": "Metadata", "attributes": "Attributes", "host": "Host", "auth_id": "AuthID", "provider": "Provider"} {
			if value, ok := fields[old]; ok {
				fields[key] = value
				delete(fields, old)
			}
		}
		raw, _ = json.Marshal(fields)
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, failure(400, "Invalid plugin request: %v", err)
	}
	return req, nil
}

type pluginRuntime struct {
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	closed         bool
	scopes         map[*requestScope]struct{}
	logins         map[string]*loginFlow
	managementBase string
	resourceBase   string
	models         map[string]modelCache
	keys           map[string]keyCache
	now            func() time.Time
}

func newRuntime() *pluginRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &pluginRuntime{ctx: ctx, cancel: cancel, scopes: make(map[*requestScope]struct{}), logins: make(map[string]*loginFlow), models: make(map[string]modelCache), keys: make(map[string]keyCache), now: time.Now}
}
func (p *pluginRuntime) stop() { p.mu.Lock(); p.closed = true; p.mu.Unlock(); p.cancel() }

func dispatch(p *pluginRuntime, method string, raw json.RawMessage, host hostCaller) (out []byte) {
	defer func() {
		if recover() != nil {
			out = errorEnvelope(failure(500, "Cursor plugin rejected an invalid request"))
		}
	}()
	req, err := parseRequest(raw, method)
	if err != nil {
		return errorEnvelope(err)
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return errorEnvelope(failure(503, "Cursor plugin is shut down"))
	}
	scope := p.newScope(host, req.CallbackID)
	defer func() {
		if scope != nil {
			scope.close()
		}
	}()
	var value any
	switch method {
	case "plugin.register", "plugin.reconfigure":
		value = registration()
	case "executor.identifier", "auth.identifier", "quota.identifier":
		value = map[string]any{"identifier": "cursor"}
	case "plugin.quiesce":
		value = map[string]any{}
	case "plugin.shutdown":
		p.stop()
		value = map[string]any{}
	case "model.static":
		value = map[string]any{"Provider": "cursor", "Models": []any{}}
	case "quota.describe":
		value = map[string]any{"supported_providers": []string{"cursor"}, "display_name": "Cursor subscription", "supports_reset": false}
	case "quota.reset", "executor.count_tokens", "executor.http_request":
		err = failure(501, "Cursor does not support this operation: %s", method)
	case "management.register":
		value = p.registerManagement(req)
	case "management.handle":
		value, err = p.handleManagement(scope, req)
	case "auth.parse":
		value, err = p.parseAuth(req)
	case "auth.login.start":
		value, err = p.startLogin(scope, req)
	case "auth.login.poll":
		value, err = p.pollLogin(scope, req)
	case "auth.refresh":
		value, err = p.refreshAuth(scope, req)
	case "model.for_auth":
		var token string
		token, err = p.token(scope, req)
		if err == nil {
			var models []modelEntry
			models, err = p.catalog(scope, token)
			if err == nil {
				list := make([]any, 0, len(models))
				for _, m := range models {
					list = append(list, m.info())
				}
				value = map[string]any{"Provider": "cursor", "Models": list}
			}
		}
	case "quota.fetch":
		value, err = p.quota(scope, req)
	case "executor.execute", "executor.execute_stream":
		value, err = p.execute(scope, req, method == "executor.execute_stream")
		if err == nil && method == "executor.execute_stream" {
			scope = nil
		}
	default:
		err = failure(400, "Unknown plugin method: %s", method)
	}
	if err != nil {
		return errorEnvelope(err)
	}
	return resultEnvelope(value)
}
func registration() any {
	return map[string]any{"schema_version": 1, "metadata": map[string]any{"Name": "Cursor", "Version": version, "Author": "spx", "GitHubRepository": "https://github.com/spancerxing/cliproxyapi-cursor-plugin", "Logo": pluginLogo, "ConfigFields": []any{}}, "capabilities": map[string]any{"management_api": true, "auth_provider": true, "model_provider": true, "quota_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions"}}}
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func digest(s string) string { d := sha256.Sum256([]byte(s)); return hex.EncodeToString(d[:]) }
func str(v any) string       { s, _ := v.(string); return s }
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		var f float64
		_, _ = fmt.Sscan(n, &f)
		return f
	}
	return 0
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func jsonObject(raw []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, failure(400, "Expected a JSON object")
	}
	return m, nil
}
func encodeJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	m, _ := jsonObject(b)
	return int64(num(m["exp"]) * 1000)
}
func headers(token string) http.Header {
	return headersWithType(token, agentClientType)
}
func headersWithType(token, clientType string) http.Header {
	h := http.Header{"Content-Type": {"application/json"}, "Connect-Protocol-Version": {"1"}, "X-Cursor-Client-Version": {clientVersion}, "X-Cursor-Client-Type": {clientType}, "X-Ghost-Mode": {"true"}}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}
