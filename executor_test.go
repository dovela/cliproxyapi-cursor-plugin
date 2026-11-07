package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExecutorChatCompletionsFormatCompatibility(t *testing.T) {
	for _, format := range []string{"", "openai", "chat-completions", "chat_completions", "openai-chat-completions", "openai_chat_completions", " OpenAI "} {
		if !isChatCompletionsFormat(format) {
			t.Errorf("rejected Chat Completions format %q", format)
		}
	}
	p := newTestRuntime(t)
	for _, format := range []string{"responses", "openai-response", "claude", "gemini"} {
		for _, method := range []string{"executor.execute", "executor.execute_stream"} {
			result := invokeNative(t, p, method, map[string]any{
				"Format": format, "Payload": []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			}, nil)
			err := object(result["error"])
			if result["ok"] != false || num(err["http_status"]) != 400 || !strings.Contains(str(err["message"]), "received") {
				t.Fatalf("unsupported format %q was not rejected: %v", format, result)
			}
		}
	}
}

func testUint(field int, n uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(field<<3)), n)
}
func testToolFrame(name, provider, server string, args []byte) []byte {
	return testToolFrameN(7, name, provider, server, args)
}
func testToolFrameN(id int, name, provider, server string, args []byte) []byte {
	label := fmt.Sprintf("call_fixture_%d", id)
	argument := bytes.Join([][]byte{wireBytes(1, []byte("magpie-"+name)), wireBytes(2, append(wireBytes(1, []byte("path")), wireBytes(2, args)...)), wireBytes(3, []byte(label)), wireBytes(4, []byte(provider)), wireBytes(5, []byte(name)), wireBytes(9, []byte(server))}, nil)
	exec := append(append(testUint(1, uint64(id)), wireBytes(15, []byte(fmt.Sprintf("exec-%d", id)))...), wireBytes(11, argument)...)
	return wireFrame(wireBytes(2, exec), 0)
}
func testBatchCount(n uint64) []byte {
	return wireFrame(wireBytes(1, wireBytes(27, testUint(1, n))), 0)
}
func TestNativeToolDecodingAndAuthority(t *testing.T) {
	for _, name := range []string{"read", "unknown"} {
		for _, provider := range []string{"magpie", "foreign"} {
			t.Run(name+"/"+provider, func(t *testing.T) {
				state := runState{tools: []chatTool{{Name: "read", Schema: map[string]any{}}}, seen: map[string]string{}, estimate: 10}
				data := append(testToolFrame(name, provider, "magpie", wireBytes(3, []byte("a.txt"))), wireFrame(wireBytes(1, wireBytes(27, testUint(1, 1))), 0)...)
				if name != "read" || provider != "magpie" {
					data = append(data, syntheticReply()...)
				}
				var events []runEvent
				var sent [][]byte
				err := state.decode(bytes.NewReader(data), func(v []byte) error { sent = append(sent, v); return nil }, func(ev runEvent) bool { events = append(events, ev); return true })
				if err != nil {
					t.Fatal(err)
				}
				if name == "read" && provider == "magpie" {
					if len(events) != 2 || events[0].tool == nil || events[1].stop != "tool_calls" {
						t.Fatalf("tool was not returned: %+v", events)
					}
					if len(sent) != 0 {
						t.Fatal("caller tool was executed or acknowledged before its result")
					}
					if events[1].usage["cursor_usage_estimated"] != true {
						t.Fatal("missing estimate marker")
					}
				} else {
					if len(sent) < 2 {
						t.Fatal("unavailable tool was not refused")
					}
					for _, ev := range events {
						if ev.tool != nil {
							t.Fatal("unregistered or foreign tool escaped")
						}
					}
				}
			})
		}
	}
}
func TestToolBatchIsHeldUntilCursorDeclaresIt(t *testing.T) {
	previous := toolBatchWait
	toolBatchWait = 250 * time.Millisecond
	t.Cleanup(func() { toolBatchWait = previous })
	args := wireBytes(3, []byte("a.txt"))
	t.Run("declared count releases every call", func(t *testing.T) {
		reader, writer := io.Pipe()
		go func() {
			_, _ = writer.Write(testToolFrameN(7, "read", "magpie", "magpie", args))
			time.Sleep(25 * time.Millisecond)
			_, _ = writer.Write(testToolFrameN(8, "read", "magpie", "magpie", args))
			time.Sleep(25 * time.Millisecond)
			_, _ = writer.Write(testBatchCount(2))
			_ = writer.Close()
		}()
		var events []runEvent
		state := runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}, estimate: 4}
		err := state.decode(reader, func([]byte) error { return nil }, func(ev runEvent) bool { events = append(events, ev); return true })
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 3 || events[0].tool == nil || events[1].tool == nil || events[2].stop != "tool_calls" {
			t.Fatalf("batch was not released together: %+v", events)
		}
	})
	t.Run("silence drops the call", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		go func() {
			_, _ = writer.Write(testToolFrameN(7, "glob", "magpie", "magpie", nil))
		}()
		published := false
		state := runState{tools: []chatTool{{Name: "glob"}}, seen: map[string]string{}}
		err := state.decode(reader, func([]byte) error { return nil }, func(runEvent) bool { published = true; return true })
		if err == nil || !strings.Contains(err.Error(), "did not declare") || published {
			t.Fatalf("silent batch was delivered: err=%v published=%v", err, published)
		}
	})
	t.Run("eof before the count drops the call", func(t *testing.T) {
		published := false
		state := runState{tools: []chatTool{{Name: "glob"}}, seen: map[string]string{}}
		err := state.decode(bytes.NewReader(testToolFrameN(7, "glob", "magpie", "magpie", nil)), func([]byte) error { return nil }, func(runEvent) bool { published = true; return true })
		if err == nil || !strings.Contains(err.Error(), "did not declare") || published {
			t.Fatalf("undeclared eof batch was delivered: err=%v published=%v", err, published)
		}
	})
	t.Run("short count is not a partial delivery", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		go func() {
			_, _ = writer.Write(append(testToolFrameN(7, "read", "magpie", "magpie", args), testBatchCount(2)...))
		}()
		published := false
		state := runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}}
		err := state.decode(reader, func([]byte) error { return nil }, func(runEvent) bool { published = true; return true })
		if err == nil || !strings.Contains(err.Error(), "did not complete") || published {
			t.Fatalf("partial batch was delivered: err=%v published=%v", err, published)
		}
	})
}
func TestNativeMalformedToolArgumentsNeverPublishCalls(t *testing.T) {
	for _, argument := range [][]byte{{0x1a, 0x05, 'x'}, {0x18, 0x01}, {0x1a, 0x01, 0xff}, {0x08, 0x02}} {
		t.Run(hex.EncodeToString(argument), func(t *testing.T) {
			state := runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}}
			published := false
			err := state.decode(bytes.NewReader(testToolFrame("read", "magpie", "magpie", argument)), func([]byte) error { return nil }, func(runEvent) bool { published = true; return true })
			if err == nil || published {
				t.Fatalf("corrupt arguments accepted: err=%v published=%v", err, published)
			}
		})
	}
}
func TestNativeFrameErrorsAndGzip(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write([]byte("frame"))
	_ = gz.Close()
	frame := wireFrame(compressed.Bytes(), 1)
	data, end, err := readConnectFrame(bytes.NewReader(frame))
	if err != nil || end || string(data) != "frame" {
		t.Fatalf("gzip decode failed: %q %v", data, err)
	}
	for _, data := range [][]byte{{0, 0, 0, 0, 3, 'a'}, {0x80, 0, 0, 0, 0}, {0, 0x7f, 0xff, 0xff, 0xff}} {
		if _, _, err = readConnectFrame(bytes.NewReader(data)); err == nil {
			t.Fatalf("malformed frame accepted: %x", data)
		}
	}
}
func TestNativeConnectStatusMapping(t *testing.T) {
	for code, status := range map[string]int{"unauthenticated": 401, "permission_denied": 403, "resource_exhausted": 429, "invalid_argument": 400, "unavailable": 503} {
		err := connectError(200, encodeJSON(map[string]any{"error": map[string]any{"code": code, "message": "fixture failure"}}))
		if errorStatus(err) != status {
			t.Errorf("%s -> %v", code, err)
		}
	}
	if connectError(200, []byte(`{}`)) != nil {
		t.Fatal("successful end frame failed")
	}
}
func TestNativeMaxModeRetryIsOnlyForRequiredMode(t *testing.T) {
	for _, msg := range []string{"MAX_MODE_REQUIRED", "this model requires max mode", "max mode required"} {
		if !maxModeRequired.MatchString(msg) {
			t.Errorf("required mode was not recognized: %s", msg)
		}
	}
	for _, msg := range []string{"max mode usage limit reached", "max mode quota exhausted", "max mode unavailable in this region"} {
		if maxModeRequired.MatchString(msg) {
			t.Errorf("quota or region failure would trigger a mode retry: %s", msg)
		}
	}
}

type fixtureRun struct {
	number  int
	queue   chan []byte
	refs    map[uint64]string
	history map[uint64][]byte
	cid     string
}
type toolFixture struct {
	mu              sync.Mutex
	runs            map[string]*fixtureRun
	count           int
	histories       []string
	conversationIDs []string
}

func (f *toolFixture) run(id string) *fixtureRun {
	r := f.runs[id]
	if r == nil {
		f.count++
		r = &fixtureRun{number: f.count, queue: make(chan []byte, 32), refs: map[uint64]string{}, history: map[uint64][]byte{}}
		f.runs[id] = r
	}
	return r
}
func fixtureFields(data []byte, number int) ([]byte, error) {
	fs, err := pbFields(data)
	if err != nil {
		return nil, err
	}
	f := pbGet(fs, number)
	if f == nil {
		return nil, fmt.Errorf("fixture missing protobuf field %d", number)
	}
	return f.data, nil
}
func (f *toolFixture) respond(ctx context.Context, req mockRequest) (mockResponse, error) {
	if !strings.HasSuffix(req.URL, "RunSSE") && !strings.HasSuffix(req.URL, "BidiAppend") {
		body := []byte(`{}`)
		if strings.HasSuffix(req.URL, "AvailableModels") {
			body = []byte(`{"models":[{"name":"fixture-model","contextTokenLimit":200000}]}`)
		}
		return mockResponse{Status: 200, Body: body}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.run(req.Headers.Get("X-Request-Id"))
	if strings.HasSuffix(req.URL, "RunSSE") {
		return mockResponse{Status: 200, Read: func(ctx context.Context) ([]byte, bool, error) {
			select {
			case payload := <-run.queue:
				return payload, false, nil
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		}}, nil
	}
	appendFields, err := pbFields(req.Body)
	if err != nil {
		return mockResponse{}, err
	}
	wire, err := hex.DecodeString(pbText(appendFields, 1))
	if err != nil {
		return mockResponse{}, err
	}
	fields, err := pbFields(wire)
	if err != nil {
		return mockResponse{}, err
	}
	if first := pbGet(fields, 1); first != nil {
		request, err := pbFields(first.data)
		if err != nil {
			return mockResponse{}, err
		}
		run.cid = pbText(request, 5)
		f.conversationIDs = append(f.conversationIDs, run.cid)
		state, err := fixtureFields(first.data, 1)
		if err != nil {
			return mockResponse{}, err
		}
		refs, err := pbFields(state)
		if err != nil {
			return mockResponse{}, err
		}
		index := uint64(1000)
		for _, ref := range refs {
			if ref.number != 1 {
				continue
			}
			run.refs[index] = hex.EncodeToString(ref.data)
			ask := append(testUint(1, index), wireBytes(2, wireBytes(1, ref.data))...)
			run.queue <- wireFrame(wireBytes(4, ask), 0)
			index++
		}
	} else if kv := pbGet(fields, 3); kv != nil {
		reply, err := pbFields(kv.data)
		if err != nil {
			return mockResponse{}, err
		}
		id := pbNum(reply, 1)
		if _, wanted := run.refs[id]; wanted {
			content, err := fixtureFields(kv.data, 2)
			if err != nil {
				return mockResponse{}, err
			}
			blob, err := fixtureFields(content, 1)
			if err != nil {
				return mockResponse{}, err
			}
			run.history[id] = blob
			if len(run.history) == len(run.refs) {
				var history []json.RawMessage
				for i := uint64(1000); i < uint64(1000+len(run.refs)); i++ {
					history = append(history, run.history[i])
				}
				text := string(encodeJSON(history))
				f.histories = append(f.histories, text)
				if run.number == 1 {
					tool := testToolFrame("read", "magpie", "magpie", wireBytes(3, []byte("a.txt")))
					run.queue <- append(tool, wireFrame(wireBytes(1, wireBytes(27, testUint(1, 1))), 0)...)
				} else {
					if !strings.Contains(text, "source-marker") {
						return mockResponse{}, fmt.Errorf("tool result missing from rebuilt transcript")
					}
					result := wireFrame(wireBytes(1, wireBytes(1, wireBytes(1, []byte("continued with source-marker")))), 0)
					run.queue <- append(result, wireFrame(wireBytes(1, wireBytes(14, []byte{8, 22, 16, 7})), 0)...)
				}
			}
		}
	}
	return mockResponse{Status: 200, Body: []byte(`{}`)}, nil
}
func TestNativeToolResultContinuationRebuildsHistory(t *testing.T) {
	p := newTestRuntime(t)
	h := newMockHost(t)
	fixture := &toolFixture{runs: map[string]*fixtureRun{}}
	h.respond = fixture.respond
	user := map[string]any{"role": "user", "content": "Read a.txt then explain"}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}}}
	execute := func(messages []any) map[string]any {
		req := map[string]any{"host_callback_id": "tool-context", "StorageJSON": []byte(`{"type":"cursor","cursor_auth":{"type":"oauth","access":"fixture-tools","expires":9999999999999}}`), "Model": "cursor/fixture-model", "Format": "chat-completions", "AuthID": "fixture", "Metadata": map[string]any{"canonical_session_id": "same-session"}, "Payload": encodeJSON(map[string]any{"messages": messages, "tools": tools})}
		result := invokeNative(t, p, "executor.execute", req, h.call)
		if result["ok"] != true {
			t.Fatalf("execution failed: %v", result)
		}
		raw, _ := base64.StdEncoding.DecodeString(str(object(result["result"])["Payload"]))
		body, err := jsonObject(raw)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	first := execute([]any{user})
	choice := object(array(first["choices"])[0])
	if str(choice["finish_reason"]) != "tool_calls" {
		t.Fatal(first)
	}
	assistant := object(choice["message"])
	calls := array(assistant["tool_calls"])
	if len(calls) != 1 {
		t.Fatal(assistant)
	}
	call := object(calls[0])
	if str(object(call["function"])["name"]) != "read" {
		t.Fatal(call)
	}
	second := execute([]any{user, assistant, map[string]any{"role": "tool", "tool_call_id": str(call["id"]), "content": "source-marker"}})
	message := object(object(array(second["choices"])[0])["message"])
	if str(message["content"]) != "continued with source-marker" {
		t.Fatal(second)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.count != 2 || len(fixture.histories) != 2 {
		t.Fatal("continuation did not rebuild exactly one fresh Run")
	}
	if !reflect.DeepEqual(fixture.conversationIDs, []string{fixture.conversationIDs[0], fixture.conversationIDs[0]}) {
		t.Fatal("conversation ID changed")
	}
	h.assertClosed(t)
}

func TestNativeStreamFailurePreservesUpstreamStatus(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p := newTestRuntime(t)
			h := newMockHost(t)
			h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
				if strings.HasSuffix(req.URL, "RunSSE") {
					return mockResponse{Status: 429, Body: []byte(`{"code":"resource_exhausted","message":"plan limit"}`)}, nil
				}
				return mockResponse{Status: 200, Body: []byte(`{}`)}, nil
			}
			req := map[string]any{"host_callback_id": "err", "stream_id": "stream", "StorageJSON": []byte(`{"type":"cursor","cursor_auth":{"type":"oauth","access":"fixture-error","expires":9999999999999}}`), "Model": "cursor/test", "Payload": []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
			method := "executor.execute"
			if stream {
				method = "executor.execute_stream"
			}
			result := invokeNative(t, p, method, req, h.call)
			if num(object(result["error"])["http_status"]) != 429 {
				t.Fatal(result)
			}
			h.assertClosed(t)
		})
	}
}
