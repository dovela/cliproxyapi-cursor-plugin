package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
)

const dynamicCall = "CallDynamicTool"
const missingToolResult = "Tool use was interrupted and did not produce a result."

type chatRequest struct {
	Model           string           `json:"model"`
	Messages        []map[string]any `json:"messages"`
	Tools           []map[string]any `json:"tools"`
	ToolChoice      any              `json:"tool_choice"`
	ReasoningEffort string           `json:"reasoning_effort"`
	ServiceTier     string           `json:"service_tier"`
	PromptCacheKey  string           `json:"prompt_cache_key"`
	toolSchemas     []string
}

func (c *chatRequest) UnmarshalJSON(raw []byte) error {
	type alias chatRequest
	if err := json.Unmarshal(raw, (*alias)(c)); err != nil {
		return err
	}
	var source struct {
		Tools []struct {
			Function struct {
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return err
	}
	c.toolSchemas = make([]string, len(source.Tools))
	for i, t := range source.Tools {
		if len(t.Function.Parameters) > 0 {
			var b bytes.Buffer
			if json.Compact(&b, t.Function.Parameters) == nil {
				c.toolSchemas[i] = b.String()
			}
		}
	}
	return nil
}

type chatTool struct {
	Name, Description string
	Schema            map[string]any
	SchemaText        string
}
type chatConversation struct {
	Messages [][]byte
	Last     string
	Estimate int
}

func textOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	var out strings.Builder
	for _, raw := range array(value) {
		part := object(raw)
		if str(part["type"]) == "text" {
			out.WriteString(str(part["text"]))
		}
	}
	return out.String()
}
func reasoningOf(m map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if s := str(m[key]); strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
func toolsOf(chat chatRequest) []chatTool {
	tools := []chatTool{}
	if chat.ToolChoice == "none" {
		return tools
	}
	only := str(object(object(chat.ToolChoice)["function"])["name"])
	for i, raw := range chat.Tools {
		f := object(raw["function"])
		name := str(f["name"])
		if str(raw["type"]) != "function" || name == "" || only != "" && only != name {
			continue
		}
		schema := object(f["parameters"])
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		schemaText := string(encodeJSON(schema))
		if i < len(chat.toolSchemas) && chat.toolSchemas[i] != "" {
			schemaText = chat.toolSchemas[i]
		}
		tools = append(tools, chatTool{Name: name, Description: str(f["description"]), Schema: schema, SchemaText: schemaText})
	}
	return tools
}
func toolCatalog(tools []chatTool) string {
	if len(tools) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n\n<dynamic_tool_catalog>\nThe MCP namespace \"magpie\" has the tools below. They are reached only through `CallDynamicTool`, never as tools of their own. Each entry is the exact call to make; its arguments must match the schema given. The schemas are complete, so there is no need to call `GetDynamicTools` first.\n")
	for _, t := range tools {
		out.WriteString("<call>CallDynamicTool({\"namespace\":\"magpie\",\"toolName\":")
		out.Write(encodeJSON(t.Name))
		out.WriteString(",\"arguments\":{...}})\n")
		out.WriteString(t.Description)
		out.WriteString("\narguments schema: ")
		out.WriteString(t.SchemaText)
		out.WriteString("\n</call>\n")
	}
	out.WriteString("</dynamic_tool_catalog>")
	return out.String()
}
func joinAssistantCalls(messages []map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, m := range messages {
		if len(out) > 0 && str(m["role"]) == "assistant" {
			prev := out[len(out)-1]
			if str(prev["role"]) == "assistant" && len(array(prev["tool_calls"])) > 0 {
				copy := map[string]any{}
				for k, v := range prev {
					copy[k] = v
				}
				copy["content"] = joinText(textOf(prev["content"]), textOf(m["content"]))
				copy["reasoning_content"] = joinText(reasoningOf(prev), reasoningOf(m))
				copy["tool_calls"] = append(append([]any{}, array(prev["tool_calls"])...), array(m["tool_calls"])...)
				out[len(out)-1] = copy
				continue
			}
		}
		out = append(out, m)
	}
	return out
}
func joinText(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n\n" + b
}
func cursorCallID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return strings.Replace(id, "__fc_", "\nfc_", 1)
	}
	return id
}
func toolResult(id, text string, isError bool) map[string]any {
	var result any = text
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		result = decoded
	}
	out := map[string]any{"type": "tool-result", "toolCallId": cursorCallID(id), "toolName": dynamicCall, "result": result, "experimental_content": []any{map[string]any{"type": "text", "text": text}}}
	if isError {
		out["isError"] = true
	}
	return out
}
func conversation(chat chatRequest, tools []chatTool) (chatConversation, error) {
	conv := chatConversation{Messages: [][]byte{}, Last: "."}
	size := 0
	systems := []string{}
	add := func(role string, content any) {
		conv.Messages = append(conv.Messages, encodeJSON(map[string]any{"role": role, "content": content}))
	}
	for _, m := range chat.Messages {
		if role := str(m["role"]); role == "system" || role == "developer" {
			if text := textOf(m["content"]); text != "" {
				systems = append(systems, text)
			}
		}
	}
	system := strings.Join(systems, "\n\n") + toolCatalog(tools)
	size += len(system)
	if system != "" {
		add("system", system)
	}
	pending := []string{}
	results := []any{}
	answer := func() {
		for _, id := range pending {
			results = append(results, toolResult(id, missingToolResult, true))
		}
		pending = nil
		if len(results) > 0 {
			add("tool", results)
		}
		results = nil
	}
	for _, m := range joinAssistantCalls(chat.Messages) {
		role := str(m["role"])
		if role == "system" || role == "developer" {
			continue
		}
		if role == "tool" {
			id := str(m["tool_call_id"])
			if at := indexOf(pending, id); at >= 0 {
				pending = append(pending[:at], pending[at+1:]...)
				text := textOf(m["content"])
				size += len(text)
				results = append(results, toolResult(id, text, m["is_error"] == true || m["isError"] == true))
			}
			continue
		}
		if role == "assistant" {
			answer()
			content := []any{}
			if reasoning := reasoningOf(m); reasoning != "" {
				content = append(content, map[string]any{"type": "reasoning", "text": reasoning})
				size += len(reasoning)
			}
			if text := textOf(m["content"]); text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
				size += len(text)
			}
			for _, raw := range array(m["tool_calls"]) {
				call := object(raw)
				if kind := str(call["type"]); kind != "" && kind != "function" {
					continue
				}
				f := object(call["function"])
				args := object(f["arguments"])
				if args == nil {
					args, _ = jsonObject([]byte(str(f["arguments"])))
				}
				if args == nil {
					args = map[string]any{}
				}
				id := str(call["id"])
				content = append(content, map[string]any{"type": "tool-call", "toolCallId": cursorCallID(id), "toolName": dynamicCall, "args": map[string]any{"namespace": "magpie", "toolName": str(f["name"]), "arguments": args}})
				pending = append(pending, id)
				size += len(encodeJSON(args)) + len(str(f["name"]))
			}
			if len(content) > 0 {
				add("assistant", content)
			}
			continue
		}
		content := []any{}
		parts := array(m["content"])
		if text, ok := m["content"].(string); ok {
			parts = []any{map[string]any{"type": "text", "text": text}}
		}
		first := ""
		for _, raw := range parts {
			part := object(raw)
			switch str(part["type"]) {
			case "text":
				text := str(part["text"])
				if text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
					size += len(text)
					if first == "" {
						first = text
					}
				}
			case "image_url":
				u := str(part["image_url"])
				if u == "" {
					u = str(object(part["image_url"])["url"])
				}
				if strings.HasPrefix(u, "data:") {
					at := strings.Index(u, ";base64,")
					if at < 5 {
						return conv, failure(400, "Invalid image data URL")
					}
					data, err := base64.StdEncoding.DecodeString(u[at+8:])
					if err != nil {
						return conv, failure(400, "Invalid base64 image")
					}
					content = append(content, map[string]any{"type": "image", "mimeType": u[5:at], "image": map[string]any{"__type": "Uint8Array", "hex": hex.EncodeToString(data)}})
				} else {
					content = append(content, map[string]any{"type": "text", "text": "[attachment image: " + u + "]"})
				}
			case "file":
				content = append(content, map[string]any{"type": "text", "text": "[attachment " + str(object(part["file"])["filename"]) + "]"})
			}
		}
		answer()
		if first != "" {
			conv.Last = first
		}
		if len(content) > 0 {
			add("user", content)
		}
	}
	answer()
	for _, t := range tools {
		size += len(t.Name) + len(t.Description) + len(encodeJSON(t.Schema))
	}
	conv.Estimate = (size + 3) / 4
	return conv, nil
}
func toolDefinition(t chatTool) protobuf {
	return protobuf(nil).text(1, t.Name).text(2, t.Description).bytes(3, pbValue(t.Schema)).text(4, "magpie").text(5, t.Name).text(6, t.SchemaText)
}
func cursorEnvironment() protobuf {
	return protobuf(nil).text(1, runtime.GOOS).text(2, os.TempDir()).text(10, "UTC")
}
func buildRun(conv chatConversation, tools []chatTool, model modelResolution, id string) ([]byte, map[string][]byte) {
	blobs := map[string][]byte{}
	put := func(data []byte) []byte {
		sum := sha256.Sum256(data)
		blobs[hex.EncodeToString(sum[:])] = data
		return sum[:]
	}
	var state protobuf
	for _, m := range conv.Messages {
		state = state.bytes(1, put(m))
	}
	mid := randomID()
	user := protobuf(nil).text(1, conv.Last).text(2, mid).uint(4, 1)
	turn := protobuf(nil).bytes(1, protobuf(nil).bytes(1, put(user)).text(10, mid))
	state = state.bytes(8, put(turn)).uint(10, 1).text(22, agentClientType)
	rc := protobuf(nil).bytes(4, cursorEnvironment())
	var mcp protobuf
	for _, t := range tools {
		d := toolDefinition(t)
		rc = rc.bytes(7, d)
		mcp = mcp.bytes(1, d)
	}
	action := protobuf(nil).bytes(2, protobuf(nil).bytes(2, rc))
	details := protobuf(nil).text(1, model.ID).text(3, model.ID).text(4, model.ID)
	requested := protobuf(nil).text(1, model.ID)
	if model.Max {
		details = details.uint(7, 1)
		requested = requested.uint(2, 1)
	}
	for _, p := range model.Params {
		requested = requested.bytes(3, protobuf(nil).text(1, p.ID).text(2, p.Value))
	}
	run := protobuf(nil).bytes(1, state).bytes(2, action).bytes(3, details).bytes(4, mcp).text(5, id).bytes(9, requested).uint(19, 1)
	return protobuf(nil).bytes(1, run), blobs
}
func conversationID(chat chatRequest, req rpcRequest) string {
	session := chat.PromptCacheKey
	if session == "" {
		for _, key := range []string{"canonical_session_id", "execution_session_id", "derived_session_id"} {
			if session = str(req.Metadata[key]); session != "" {
				break
			}
		}
	}
	if session == "" {
		return randomID()
	}
	var first any
	for _, m := range chat.Messages {
		if str(m["role"]) == "user" {
			first = m
			break
		}
	}
	sum := sha256.Sum256([]byte("cursor conversation\x00" + req.AuthID + "\x00" + session + "\x00" + string(encodeJSON(first))))
	b := sum[:16]
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
