package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

type runEvent struct {
	text, reasoning string
	tool            map[string]any
	stop            string
	usage           map[string]any
	err             error
}
type runState struct {
	wrote, said, calls, answered, listed int
	ended                                bool
	usage                                map[string]any
	tools                                []chatTool
	blobs                                map[string][]byte
	seen                                 map[string]string
	pending                              []map[string]any
	estimate                             int
}

// toolBatchWait is how long a step may stay silent after its latest tool call,
// local answer, or declared count before the batch is abandoned. Cursor sends
// each call only after its arguments are written, so the wait restarts on each
// of those. Calls stay buffered until the count seals the batch; a deadline or
// a stream that ends first drops them instead of handing the caller a partial.
var toolBatchWait = 30 * time.Second

var maxModeRequired = regexp.MustCompile(`(?i)max[ _]mode[ _]required|requires max mode`)

func isChatCompletionsFormat(format string) bool {
	// CPA normalizes Chat Completions aliases to the SDK's "openai" format
	// before invoking an executor. Keep aliases for older hosts and direct calls.
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "openai", "chat-completions", "chat_completions", "openai-chat-completions", "openai_chat_completions":
		return true
	default:
		return false
	}
}

func (p *pluginRuntime) execute(s *requestScope, req rpcRequest, stream bool) (any, error) {
	if !isChatCompletionsFormat(req.Format) {
		return nil, failure(400, "Expected OpenAI Chat Completions format (openai); received %q", req.Format)
	}
	if stream && req.StreamID == "" {
		return nil, failure(400, "Host stream_id is required")
	}
	if len(req.Payload) > maxWireBytes {
		return nil, failure(413, "Cursor request exceeds size limit")
	}
	var chat chatRequest
	if err := json.Unmarshal(req.Payload, &chat); err != nil || len(chat.Messages) == 0 {
		return nil, failure(400, "Expected a nonempty Chat Completions messages array")
	}
	model := req.Model
	if model == "" {
		model = chat.Model
	}
	if model == "" {
		model = "cursor/auto"
	}
	token, err := p.token(s, req)
	if err != nil {
		return nil, err
	}
	entries, catalogErr := p.catalog(s, token)
	if catalogErr != nil {
		p.mu.Lock()
		entries = p.models[digest(token)].Entries
		p.mu.Unlock()
	}
	resolution := resolveModel(entries, model, chat.ReasoningEffort, chat.ServiceTier)
	tools := toolsOf(chat)
	conv, err := conversation(chat, tools)
	if err != nil {
		return nil, err
	}
	cid := conversationID(chat, req)
	events := p.run(s, token, resolution, conv, tools, cid)
	first, ok := <-events
	if !ok {
		return nil, failure(502, "Cursor returned an empty reply")
	}
	if first.err != nil && !resolution.Max && maxModeRequired.MatchString(first.err.Error()) {
		for range events {
		}
		resolution.Max = true
		events = p.run(s, token, resolution, conv, tools, cid)
		first, ok = <-events
	}
	if !ok {
		return nil, failure(502, "Cursor returned an empty reply")
	}
	if first.err != nil {
		for range events {
		}
		return nil, first.err
	}
	responseHeaders := http.Header{"Content-Type": {"application/json"}, "X-Cursor-Continuity": {"stateless; bootstrap=imported-history; transport=CPA-host-HTTP"}}
	id := "chatcmpl-" + strings.ReplaceAll(randomID(), "-", "")
	created := p.now().Unix()
	if stream {
		responseHeaders.Set("Content-Type", "text/event-stream")
		responseHeaders.Set("Cache-Control", "no-cache")
		go streamReply(s, req.StreamID, events, first, id, model, created)
		return map[string]any{"headers": responseHeaders}, nil
	}
	var content, reasoning strings.Builder
	calls := []any{}
	stop := "stop"
	var usage map[string]any
	apply := func(ev runEvent) error {
		if ev.err != nil {
			return ev.err
		}
		content.WriteString(ev.text)
		reasoning.WriteString(ev.reasoning)
		if ev.tool != nil {
			call := copyMap(ev.tool)
			delete(call, "index")
			calls = append(calls, call)
		}
		if ev.stop != "" {
			stop = ev.stop
			usage = ev.usage
		}
		return nil
	}
	if err = apply(first); err != nil {
		return nil, err
	}
	for ev := range events {
		if err = apply(ev); err != nil {
			return nil, err
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	body := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": stop}}}
	if usage != nil {
		body["usage"] = usage
	}
	return map[string]any{"Payload": encodeJSON(body), "Headers": responseHeaders}, nil
}
func copyMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func streamReply(s *requestScope, streamID string, events <-chan runEvent, first runEvent, id, model string, created int64) {
	defer s.close()
	closeStream := func(err error) {
		fields := map[string]any{"stream_id": streamID}
		if err != nil {
			fields["error"] = err.Error()
		}
		_ = s.call("host.stream.close", fields, nil)
	}
	emit := func(value any) error {
		return s.call("host.stream.emit", map[string]any{"stream_id": streamID, "payload": append(append([]byte("data: "), encodeJSON(value)...), []byte("\n\n")...)}, nil)
	}
	chunk := func(delta map[string]any, stop any, usage map[string]any) error {
		value := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": stop}}}
		if usage != nil {
			value["usage"] = usage
		}
		return emit(value)
	}
	if err := chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil); err != nil {
		closeStream(err)
		return
	}
	apply := func(ev runEvent) error {
		switch {
		case ev.err != nil:
			_ = emit(map[string]any{"error": map[string]any{"message": ev.err.Error(), "code": errorStatus(ev.err)}})
			return ev.err
		case ev.text != "":
			return chunk(map[string]any{"content": ev.text}, nil, nil)
		case ev.reasoning != "":
			return chunk(map[string]any{"reasoning_content": ev.reasoning}, nil, nil)
		case ev.tool != nil:
			return chunk(map[string]any{"tool_calls": []any{ev.tool}}, nil, nil)
		case ev.stop != "":
			return chunk(map[string]any{}, ev.stop, ev.usage)
		}
		return nil
	}
	if err := apply(first); err != nil {
		closeStream(err)
		return
	}
	for ev := range events {
		if err := apply(ev); err != nil {
			closeStream(err)
			return
		}
	}
	err := s.call("host.stream.emit", map[string]any{"stream_id": streamID, "payload": []byte("data: [DONE]\n\n")}, nil)
	closeStream(err)
}

func (p *pluginRuntime) run(s *requestScope, token string, model modelResolution, conv chatConversation, tools []chatTool, cid string) <-chan runEvent {
	events := make(chan runEvent, 1)
	go func() {
		defer close(events)
		ctx, cancel := context.WithCancel(s.ctx)
		defer cancel()
		emit := func(ev runEvent) bool {
			select {
			case events <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		requestID := randomID()
		h := headers(token)
		h.Set("Content-Type", "application/connect+proto")
		h.Set("Accept-Encoding", "identity")
		h.Set("X-Cursor-Streaming", "true")
		h.Set("X-Request-Id", requestID)
		h.Set("X-Cursor-Agent-Allowed-Tools", "mcp_tool_call,get_mcp_tools_tool_call")
		first, blobs := buildRun(conv, tools, model, cid)
		queue := make(chan []byte, 32)
		var queued atomic.Int64
		send := func(data []byte) error {
			if len(data) > maxWireBytes {
				return failure(413, "Cursor outbound frame exceeds the size limit")
			}
			if queued.Add(int64(len(data))) > maxWireBytes {
				queued.Add(-int64(len(data)))
				return failure(503, "Cursor outbound queue limit exceeded")
			}
			select {
			case queue <- data:
				return nil
			case <-ctx.Done():
				queued.Add(-int64(len(data)))
				return ctx.Err()
			}
		}
		if err := send(first); err != nil {
			emit(runEvent{err: err})
			return
		}
		senderDone := make(chan struct{})
		senderError := make(chan error, 1)
		go func() {
			defer close(senderDone)
			heartbeat := time.NewTicker(5 * time.Second)
			defer heartbeat.Stop()
			var seq uint64
			for {
				var data []byte
				fromQueue := false
				select {
				case <-ctx.Done():
					return
				case data = <-queue:
					fromQueue = true
				case <-heartbeat.C:
					data = protobuf(nil).bytes(7, nil)
				}
				body := protobuf(nil).text(1, hex.EncodeToString(data)).bytes(2, protobuf(nil).text(1, requestID)).uint(3, seq)
				seq++
				appendHeaders := h.Clone()
				appendHeaders.Set("Content-Type", "application/proto")
				resp, err := s.do(ctx, http.MethodPost, cursorAPI+"/aiserver.v1.BidiService/BidiAppend", appendHeaders, body)
				if err == nil {
					var raw []byte
					raw, err = io.ReadAll(io.LimitReader(resp.Body, maxWireBytes+1))
					_ = resp.Body.Close()
					if err == nil && len(raw) > maxWireBytes {
						err = failure(502, "Cursor append response exceeds limit")
					}
					if err == nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
						err = connectError(resp.StatusCode, raw)
					}
				}
				if fromQueue {
					queued.Add(-int64(len(data)))
				}
				if err != nil {
					select {
					case senderError <- err:
					default:
					}
					cancel()
					return
				}
			}
		}()
		defer func() { cancel(); <-senderDone }()
		resp, err := s.do(ctx, http.MethodPost, cursorAPI+"/agent.v1.AgentService/RunSSE", h, connectFrame(protobuf(nil).text(1, requestID)))
		if err == nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxWireBytes+1))
			_ = resp.Body.Close()
			if readErr != nil {
				err = readErr
			} else {
				err = connectError(resp.StatusCode, raw)
			}
		}
		if err != nil {
			select {
			case original := <-senderError:
				err = original
			default:
			}
			if s.ctx.Err() == nil {
				select {
				case events <- runEvent{err: err}:
				case <-s.ctx.Done():
				}
			}
			return
		}
		defer resp.Body.Close()
		state := runState{tools: tools, blobs: blobs, seen: map[string]string{}, estimate: conv.Estimate}
		err = state.decode(resp.Body, send, emit)
		if err != nil {
			select {
			case original := <-senderError:
				err = original
			default:
			}
			if s.ctx.Err() == nil {
				select {
				case events <- runEvent{err: err}:
				case <-s.ctx.Done():
				}
			}
		}
	}()
	return events
}
func (st *runState) finished() runEvent {
	usage := st.usage
	if usage == nil {
		input := st.estimate
		output := (st.said + 3) / 4
		usage = map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output, "cursor_usage_estimated": true, "prompt_tokens_details": map[string]any{"cached_tokens": 0, "cache_write_tokens": 0}}
	}
	stop := "stop"
	if st.calls > 0 {
		stop = "tool_calls"
	}
	return runEvent{stop: stop, usage: usage}
}
func usageFromFields(fs []pbField) map[string]any {
	input, output, read, write, reason := pbNum(fs, 1), pbNum(fs, 2), pbNum(fs, 3), pbNum(fs, 4), pbNum(fs, 5)
	out := map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output, "prompt_tokens_details": map[string]any{"cached_tokens": read, "cache_write_tokens": write}}
	if reason > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": reason}
	}
	return out
}

type readFrame struct {
	data []byte
	end  bool
	err  error
}

func (st *runState) batchOpen() bool {
	made := st.calls + st.answered
	if made == 0 {
		return false
	}
	return st.listed == 0 || made < st.listed
}
func (st *runState) batchIncomplete() error {
	if st.listed > 0 {
		return failure(502, "Cursor did not complete its declared tool batch")
	}
	return failure(502, "Cursor did not declare its tool batch")
}
func (st *runState) publish(emit func(runEvent) bool) error {
	for _, call := range st.pending {
		if !emit(runEvent{tool: call}) {
			return context.Canceled
		}
	}
	st.pending = nil
	if !emit(st.finished()) {
		return context.Canceled
	}
	return nil
}

// seal delivers buffered client calls once Cursor's count matches them.
// A count that covers only calls answered locally lets the turn continue.
func (st *runState) seal(emit func(runEvent) bool) (bool, error) {
	made := st.calls + st.answered
	if st.listed == 0 || made < st.listed {
		return false, nil
	}
	if made > st.listed {
		return false, failure(502, "Cursor tool batch exceeded its declared count")
	}
	if st.calls == 0 {
		st.listed = 0
		st.answered = 0
		return false, nil
	}
	if err := st.publish(emit); err != nil {
		return false, err
	}
	return true, nil
}
func (st *runState) decode(reader io.Reader, send func([]byte) error, emit func(runEvent) bool) error {
	frames := make(chan readFrame, 1)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		for {
			data, end, err := readConnectFrame(reader)
			select {
			case <-quit:
				return
			case frames <- readFrame{data: data, end: end, err: err}:
			}
			if err != nil || end {
				return
			}
		}
	}()
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerC = nil
	}
	defer stopTimer()
	arm := func() {
		if !st.batchOpen() {
			stopTimer()
			return
		}
		if timer == nil {
			timer = time.NewTimer(toolBatchWait)
		} else if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
			timer.Reset(toolBatchWait)
		} else {
			timer.Reset(toolBatchWait)
		}
		timerC = timer.C
	}
	for {
		var frame readFrame
		expired := false
		select {
		case frame = <-frames:
		case <-timerC:
			select {
			case frame = <-frames:
				expired = true
			default:
				return st.batchIncomplete()
			}
		}
		sealed, touched, err := st.consume(frame, send, emit)
		if err != nil || sealed {
			return err
		}
		if expired && st.batchOpen() {
			return st.batchIncomplete()
		}
		if touched {
			arm()
		}
	}
}
func (st *runState) consume(frame readFrame, send func([]byte) error, emit func(runEvent) bool) (bool, bool, error) {
	if frame.err != nil {
		if errors.Is(frame.err, io.EOF) && st.batchOpen() {
			return false, false, st.batchIncomplete()
		}
		if errors.Is(frame.err, io.EOF) {
			return false, false, failure(502, "Cursor reply broke off before turn completion")
		}
		return false, false, frame.err
	}
	if frame.end {
		if err := connectError(200, frame.data); err != nil {
			return false, false, err
		}
		if st.batchOpen() {
			return false, false, st.batchIncomplete()
		}
		if st.wrote == 0 && st.calls == 0 {
			return false, false, failure(502, "Cursor returned an empty reply")
		}
		if !emit(st.finished()) {
			return false, false, context.Canceled
		}
		return true, false, nil
	}
	fields, err := pbFields(frame.data)
	if err != nil {
		return false, false, err
	}
	touched := false
	for _, field := range fields {
		switch field.number {
		case 1:
			updates, err := pbFields(field.data)
			if err != nil {
				return false, false, err
			}
			for _, update := range updates {
				fs, err := pbFields(update.data)
				if err != nil {
					return false, false, err
				}
				switch update.number {
				case 1:
					text := pbText(fs, 1)
					st.said += len(text)
					st.wrote += len(text)
					if text != "" && !emit(runEvent{text: text}) {
						return false, false, context.Canceled
					}
				case 4:
					text := pbText(fs, 1)
					st.said += len(text)
					if text != "" && !emit(runEvent{reasoning: text}) {
						return false, false, context.Canceled
					}
				case 14:
					st.ended = true
					st.usage = usageFromFields(fs)
					if st.calls == 0 {
						if st.wrote == 0 {
							return false, false, failure(502, "Cursor ended a turn without text or tool calls")
						}
						if !emit(st.finished()) {
							return false, false, context.Canceled
						}
						return true, false, nil
					}
				case 27:
					n := pbNum(fs, 1)
					if f := pbGet(fs, 1); f == nil || f.wire != 0 || n == 0 || n > 100000 {
						return false, false, failure(502, "Invalid Cursor tool batch count")
					}
					if st.listed > 0 && st.listed != int(n) {
						return false, false, failure(502, "Conflicting Cursor tool batch counts")
					}
					st.listed = int(n)
					touched = true
				}
			}
		case 2:
			call, counted, err := st.exec(field.data, send)
			if err != nil {
				return false, false, err
			}
			if call != nil {
				call["index"] = st.calls
				st.calls++
				st.said += len(str(object(call["function"])["arguments"]))
				st.pending = append(st.pending, call)
				touched = true
			} else if counted {
				st.answered++
				touched = true
			}
		case 4:
			fs, err := pbFields(field.data)
			if err != nil {
				return false, false, err
			}
			id := pbNum(fs, 1)
			for _, part := range fs {
				switch part.number {
				case 2:
					want, err := pbFields(part.data)
					if err != nil {
						return false, false, err
					}
					key := hex.EncodeToString([]byte(pbText(want, 1)))
					var result protobuf
					if blob, ok := st.blobs[key]; ok {
						result = result.bytes(1, blob)
					} else {
						result = result.bytes(2, protobuf(nil).text(1, "blob not found"))
					}
					if err = send(protobuf(nil).bytes(3, protobuf(nil).uint(1, id).bytes(2, result))); err != nil {
						return false, false, err
					}
				case 3:
					if err = send(protobuf(nil).bytes(3, protobuf(nil).uint(1, id).bytes(3, nil))); err != nil {
						return false, false, err
					}
				}
			}
		}
		sealed, err := st.seal(emit)
		if err != nil || sealed {
			return sealed, touched, err
		}
	}
	return false, touched, nil
}
func (st *runState) exec(data []byte, send func([]byte) error) (map[string]any, bool, error) {
	fs, err := pbFields(data)
	if err != nil {
		return nil, false, err
	}
	id := pbNum(fs, 1)
	execID := pbText(fs, 15)
	closeExec := func() error { return send(protobuf(nil).bytes(5, protobuf(nil).bytes(1, protobuf(nil).uint(1, id)))) }
	answer := func(field int, value []byte) error {
		if err := send(protobuf(nil).bytes(2, protobuf(nil).uint(1, id).text(15, execID).bytes(field, value))); err != nil {
			return err
		}
		return closeExec()
	}
	unavailable := func() (map[string]any, bool, error) {
		if err := send(protobuf(nil).bytes(5, protobuf(nil).bytes(2, protobuf(nil).uint(1, id).text(2, "not available")))); err != nil {
			return nil, false, err
		}
		return nil, true, closeExec()
	}
	for _, f := range fs {
		switch f.number {
		case 11:
			if len(f.data) > 1<<20 {
				return nil, false, failure(502, "Cursor tool arguments exceed size limit")
			}
			argsFields, err := pbFields(f.data)
			if err != nil {
				return nil, false, err
			}
			name := pbText(argsFields, 5)
			if name == "" {
				name = strings.TrimPrefix(pbText(argsFields, 1), "magpie-")
			}
			allowed := false
			for _, t := range st.tools {
				allowed = allowed || t.Name == name
			}
			provider, server := pbText(argsFields, 4), pbText(argsFields, 9)
			if !allowed || provider != "" && provider != "magpie" || server != "" && server != "magpie" || pbGet(argsFields, 6) != nil || pbNum(argsFields, 7) != 0 {
				return unavailable()
			}
			args := map[string]any{}
			for _, kv := range argsFields {
				if kv.number != 2 {
					continue
				}
				if kv.wire != 2 {
					return nil, false, failure(502, "Invalid Cursor tool argument entry")
				}
				parts, err := pbFields(kv.data)
				if err != nil {
					return nil, false, err
				}
				key := pbText(parts, 1)
				var value any
				if v := pbGet(parts, 2); v != nil {
					value, err = pbAny(v.data)
					if err != nil {
						return nil, false, err
					}
				}
				args[key] = value
			}
			callID := strings.ReplaceAll(pbText(argsFields, 3), "\n", "__")
			if callID == "" {
				callID = "call_cursor_" + digest(fmt.Sprintf("%d:%s", id, execID))[:20]
			}
			encoded := string(encodeJSON(args))
			identity := fmt.Sprintf("%d:%s", id, execID)
			if prior, ok := st.seen[identity]; ok {
				if prior != name+encoded {
					return nil, false, failure(502, "Cursor changed a repeated tool call")
				}
				return nil, false, nil
			}
			st.seen[identity] = name + encoded
			return map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": encoded}}, false, nil
		case 36:
			server := protobuf(nil).text(1, "magpie").text(2, "magpie").text(7, "connected")
			for _, t := range st.tools {
				server = server.bytes(5, toolDefinition(t))
			}
			return nil, true, answer(36, protobuf(nil).bytes(1, protobuf(nil).bytes(1, server)))
		case 10:
			return nil, false, answer(10, protobuf(nil).bytes(1, protobuf(nil).bytes(1, protobuf(nil).bytes(4, cursorEnvironment()))))
		}
	}
	return unavailable()
}
