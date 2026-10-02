package main

import (
	"encoding/json"
	"strings"
)

const toolLimit = 64 * 1024
const truncated = "\n[truncated]"

func limitTool(text string) string {
	text = cleanControls(text)
	if len(text) > toolLimit {
		return truncateBytes(text, toolLimit-len(truncated)) + truncated
	}
	return text
}

func toolCall(name string, input any) string {
	text := name
	if input != nil {
		if text != "" {
			text += "\n"
		}
		text += stringValue(input)
	}
	return limitTool(text)
}

// Only textual result fields are searchable. Walking arbitrary object values
// would also retain images, reasoning, diagnostics and nested chat state.
func toolText(value any) string {
	var b strings.Builder
	var visit func(any, int)
	visit = func(value any, depth int) {
		if depth > 32 || b.Len() > toolLimit {
			return
		}
		switch v := value.(type) {
		case string:
			if v != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(truncateBytes(cleanControls(v), toolLimit+len(truncated)-b.Len()))
			}
		case []any:
			for _, x := range v {
				visit(x, depth+1)
			}
		case map[string]any:
			switch str(v["type"]) {
			case "", "text", "input_text", "output_text", "tool_result", "tool_search_output":
			default:
				return
			}
			for _, key := range []string{"text", "content", "contents", "output", "outputRaw", "stdout", "stderr", "interleavedOutput", "error", "message", "modelVisibleErrorMessage", "clientVisibleErrorMessage", "result", "results", "success", "failure"} {
				visit(v[key], depth+1)
			}
			if str(v["case"]) == "success" || str(v["case"]) == "failure" {
				visit(v["value"], depth+1)
			}
			for _, value := range arr(v["tools"]) {
				tool := obj(value)
				if f := obj(tool["function"]); f != nil {
					tool = f
				}
				visit(str(tool["name"]), depth+1)
				visit(str(tool["description"]), depth+1)
			}
		}
	}
	visit(value, 0)
	return limitTool(b.String())
}

// Cursor persists some protobuf values as JSON strings, others as opaque
// binary strings. The latter are not text and need a schema to decode safely.
func cursorValue(v any) any {
	if s, ok := v.(string); ok {
		if strings.HasPrefix(s, "~") {
			return nil
		}
		var decoded any
		if json.Unmarshal([]byte(s), &decoded) == nil {
			return decoded
		}
	}
	return v
}

func cursorTools(value any, group string) []record {
	value = cursorValue(value)
	if values, ok := value.([]any); ok {
		var out []record
		for _, v := range values {
			out = append(out, cursorTools(v, group)...)
		}
		return out
	}
	v := obj(value)
	name := str(v["name"])
	if name == "" {
		name = str(v["toolName"])
	}
	var input any
	for _, key := range []string{"rawArgs", "args", "params"} {
		if x := cursorValue(v[key]); x != nil && x != "" {
			input = x
			break
		}
	}
	var out []record
	for _, text := range []string{toolCall(name, input), toolText(v["content"]), toolText(cursorValue(v["result"])), toolText(cursorValue(v["error"]))} {
		if strings.TrimSpace(text) != "" {
			out = append(out, record{"tool", text, true, group})
		}
	}
	return out
}
