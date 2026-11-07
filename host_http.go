package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

type requestScope struct {
	p          *pluginRuntime
	ctx        context.Context
	cancel     context.CancelFunc
	host       hostCaller
	callbackID string
	mu         sync.Mutex
	closed     bool
	bodies     map[*hostBody]struct{}
	pending    sync.WaitGroup
}

func (p *pluginRuntime) newScope(host hostCaller, id string) *requestScope {
	ctx, cancel := context.WithCancel(p.ctx)
	s := &requestScope{p: p, ctx: ctx, cancel: cancel, host: host, callbackID: id, bodies: make(map[*hostBody]struct{})}
	p.mu.Lock()
	p.scopes[s] = struct{}{}
	p.mu.Unlock()
	return s
}
func (s *requestScope) call(method string, fields map[string]any, result any) error {
	if s.host == nil || s.callbackID == "" {
		return failure(502, "CPA HTTP callback is unavailable outside its request scope")
	}
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["host_callback_id"] = s.callbackID
	raw, err := s.host(method, encodeJSON(fields))
	if err != nil {
		return err
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err = json.Unmarshal(raw, &env); err != nil {
		return failure(502, "Invalid CPA callback response: %v", err)
	}
	if !env.OK {
		if env.Error != nil {
			status := env.Error.HTTPStatus
			if status < 400 {
				status = 502
			}
			return failure(status, "%s", env.Error.Message)
		}
		return failure(502, "CPA HTTP callback failed")
	}
	if result != nil && len(env.Result) > 0 {
		if err = json.Unmarshal(env.Result, result); err != nil {
			return failure(502, "Invalid CPA callback result: %v", err)
		}
	}
	return nil
}
func (s *requestScope) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	list := make([]*hostBody, 0, len(s.bodies))
	for b := range s.bodies {
		list = append(list, b)
	}
	s.mu.Unlock()
	s.cancel()
	for _, b := range list {
		_ = b.Close()
	}
	s.pending.Wait()
	s.p.mu.Lock()
	delete(s.p.scopes, s)
	s.p.mu.Unlock()
}

type hostBody struct {
	s                 *requestScope
	operation, stream string
	pending           []byte
	eof               bool
	once              sync.Once
	stop              func() bool
}

func (s *requestScope) do(ctx context.Context, method, url string, h http.Header, payload []byte) (*http.Response, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, context.Canceled
	}
	s.pending.Add(1)
	s.mu.Unlock()
	defer s.pending.Done()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var opened struct {
		ID string `json:"operation_id"`
	}
	if err := s.call("host.http.operation_open", nil, &opened); err != nil {
		return nil, err
	}
	if opened.ID == "" {
		return nil, failure(502, "CPA returned no HTTP operation ID")
	}
	b := &hostBody{s: s, operation: opened.ID}
	// Cancellation is registered before the response head is requested.
	s.mu.Lock()
	b.stop = context.AfterFunc(ctx, func() { _ = b.Close() })
	s.mu.Unlock()
	var response struct {
		Status  int         `json:"status_code"`
		Headers http.Header `json:"headers"`
		ID      string      `json:"stream_id"`
	}
	err := s.call("host.http.do_stream", map[string]any{"operation_id": opened.ID, "method": method, "url": url, "headers": h, "body": payload, "wire_profile": map[string]any{"http1_only": true}}, &response)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	if response.ID == "" {
		_ = b.Close()
		return nil, failure(502, "CPA returned no HTTP stream ID")
	}
	// Close may have canceled an operation whose response head arrived late.
	s.mu.Lock()
	b.stream = response.ID
	closed := s.closed || ctx.Err() != nil
	if !closed {
		s.bodies[b] = struct{}{}
	}
	s.mu.Unlock()
	if closed {
		_ = s.call("host.http.stream_close", map[string]any{"stream_id": response.ID}, nil)
		_ = b.Close()
		return nil, context.Canceled
	}
	if response.Status < 100 || response.Status > 599 {
		_ = b.Close()
		return nil, failure(502, "CPA returned an invalid HTTP status")
	}
	return &http.Response{StatusCode: response.Status, Header: response.Headers, Body: b}, nil
}
func (b *hostBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.eof {
			return 0, io.EOF
		}
		var chunk struct {
			Payload []byte `json:"payload"`
			Done    bool   `json:"done"`
			Error   string `json:"error"`
		}
		if err := b.s.call("host.http.stream_read", map[string]any{"stream_id": b.stream}, &chunk); err != nil {
			_ = b.Close()
			return 0, err
		}
		if len(chunk.Payload) > maxWireBytes {
			_ = b.Close()
			return 0, failure(502, "CPA HTTP chunk exceeds the size limit")
		}
		b.pending = chunk.Payload
		b.eof = chunk.Done
		if chunk.Error != "" {
			_ = b.Close()
			return 0, failure(502, "%s", chunk.Error)
		}
		if b.eof {
			_ = b.Close()
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *hostBody) Close() error {
	b.once.Do(func() {
		b.s.mu.Lock()
		id, stop := b.stream, b.stop
		delete(b.s.bodies, b)
		b.s.mu.Unlock()
		if stop != nil {
			stop()
		}
		_ = b.s.call("host.http.cancel", map[string]any{"operation_id": b.operation}, nil)
		if id != "" {
			_ = b.s.call("host.http.stream_close", map[string]any{"stream_id": id}, nil)
		}
	})
	return nil
}
func (s *requestScope) json(ctx context.Context, method, path, token string, body any) (map[string]any, error) {
	return s.jsonClient(ctx, method, path, token, body, agentClientType)
}
func (s *requestScope) jsonClient(ctx context.Context, method, path, token string, body any, clientType string) (map[string]any, error) {
	url := path
	if len(path) > 0 && path[0] == '/' {
		url = cursorAPI + path
	}
	var payload []byte
	if body != nil {
		payload = encodeJSON(body)
	}
	h := headersWithType(token, clientType)
	if strings.HasPrefix(path, "/auth/") || strings.HasPrefix(path, "/oauth/") {
		h.Del("Connect-Protocol-Version")
	}
	resp, err := s.do(ctx, method, url, h, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWireBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxWireBytes {
		return nil, failure(502, "Cursor JSON response exceeds the size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, connectError(resp.StatusCode, raw)
	}
	m, err := jsonObject(raw)
	if err != nil {
		return nil, failure(502, "Cursor returned invalid JSON: %v", err)
	}
	return m, nil
}
func connectError(status int, raw []byte) error {
	m, _ := jsonObject(raw)
	if nested := object(m["error"]); nested != nil {
		m = nested
	}
	code, msg := str(m["code"]), str(m["message"])
	for _, d := range array(m["details"]) {
		detail := object(object(object(d)["debug"])["details"])
		title, text := str(detail["title"]), str(detail["detail"])
		if title != "" || text != "" {
			msg = title
			if text != "" {
				if msg != "" {
					msg += ": "
				}
				msg += text
			}
		}
	}
	if msg == "" {
		msg = code
	}
	if msg == "" {
		msg = string(raw)
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	low := strings.ToLower(msg)
	switch {
	case containsAny(low, "region"):
		status = 403
	case code == "permission_denied":
		status = 403
	case code == "unauthenticated" || status == 401 || containsAny(low, "expired"):
		status = 401
	case code == "resource_exhausted" || containsAny(low, "quota", "rate limit", "usage limit"):
		status = 429
	case containsAny(low, "too long", "context length", "too many tokens") || code == "invalid_argument":
		status = 400
	case code == "unavailable":
		status = 503
	case code != "":
		status = 502
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return failure(status, "%s", msg)
}
func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
