package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// redactedValue replaces every value the C1 redaction rules cover.
const redactedValue = "<redacted>"

// lastAppliedAnnotation holds the full previous manifest, env values included.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

func isSensitiveKey(key string) bool {
	return strings.EqualFold(key, "token") || strings.EqualFold(key, "password") || strings.EqualFold(key, "secret")
}

func isWholeValueRedacted(key string) bool {
	return isSensitiveKey(key) || key == "command" || key == "args"
}

// redactText redacts a tool-result text that is either JSON or YAML-like text.
func redactText(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		dec := json.NewDecoder(strings.NewReader(t))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err == nil && !dec.More() {
			if out, err := marshalNoEscape(redactJSONValue(v, "")); err == nil {
				return string(out)
			}
		}
	}
	return redactYAML(s)
}

// redactJSONValue applies the C1 rules to a decoded JSON document in place.
func redactJSONValue(v any, parentKey string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			switch {
			case k == lastAppliedAnnotation:
				delete(t, k)
			case isWholeValueRedacted(k):
				t[k] = redactedValue
			case (parentKey == "env" || parentKey == "httpHeaders") && k == "value":
				t[k] = redactedValue
			default:
				t[k] = redactJSONValue(val, k)
			}
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactJSONValue(t[i], parentKey)
		}
		return t
	default:
		return v
	}
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

type yamlFrameKind int

const (
	frameEnv yamlFrameKind = iota
	frameHeaders
	frameSwallow
)

type yamlFrame struct {
	indent int
	kind   yamlFrameKind
}

// covers reports whether a line belongs to the block opened at f.indent; a
// sequence may sit at the same indent as its parent key.
func (f yamlFrame) covers(indent int, trimmed string) bool {
	return indent > f.indent || (indent == f.indent && strings.HasPrefix(trimmed, "-"))
}

// redactYAML is a line-oriented redactor for the block-style YAML that
// kubernetes-mcp-server emits; it over-redacts rather than parse ambiguously.
func redactYAML(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	var stack []yamlFrame
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		blank := strings.TrimSpace(line) == ""
		if !blank {
			for len(stack) > 0 && !stack[len(stack)-1].covers(indent, trimmed) {
				stack = stack[:len(stack)-1]
			}
		}
		if len(stack) > 0 && stack[len(stack)-1].kind == frameSwallow {
			continue
		}
		if blank || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		keyIndent, key, valStart, ok := parseYAMLKey(line, indent)
		if !ok {
			out = append(out, line)
			continue
		}
		value := strings.TrimSpace(line[valStart:])
		inList := len(stack) > 0 && (stack[len(stack)-1].kind == frameEnv || stack[len(stack)-1].kind == frameHeaders)
		switch {
		case key == lastAppliedAnnotation:
			stack = append(stack, yamlFrame{indent: keyIndent, kind: frameSwallow})
		case isWholeValueRedacted(key) || (inList && key == "value"):
			out = append(out, line[:valStart]+` "`+redactedValue+`"`)
			stack = append(stack, yamlFrame{indent: keyIndent, kind: frameSwallow})
		case key == "env" || key == "httpHeaders":
			if value != "" && value != "[]" && value != "null" && value != "~" {
				out = append(out, line[:valStart]+` "`+redactedValue+`"`)
				stack = append(stack, yamlFrame{indent: keyIndent, kind: frameSwallow})
				continue
			}
			kind := frameEnv
			if key == "httpHeaders" {
				kind = frameHeaders
			}
			out = append(out, line)
			stack = append(stack, yamlFrame{indent: keyIndent, kind: kind})
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// parseYAMLKey finds a "key:" on line after any "- " sequence markers and
// returns the key's column, the unquoted key and the offset just past the colon.
func parseYAMLKey(line string, indent int) (int, string, int, bool) {
	pos := indent
	for strings.HasPrefix(line[pos:], "- ") {
		pos += 2
		for pos < len(line) && line[pos] == ' ' {
			pos++
		}
	}
	if pos >= len(line) {
		return 0, "", 0, false
	}
	rest := line[pos:]
	switch rest[0] {
	case '"', '\'':
		end := strings.IndexByte(rest[1:], rest[0])
		if end < 0 {
			return 0, "", 0, false
		}
		key := rest[1 : end+1]
		after := strings.TrimLeft(rest[end+2:], " ")
		if !strings.HasPrefix(after, ":") {
			return 0, "", 0, false
		}
		colon := len(line) - len(after)
		if colon+1 < len(line) && line[colon+1] != ' ' {
			return 0, "", 0, false
		}
		return pos, key, colon + 1, true
	case '[', '{', '|', '>', '-', '#', '&', '*', '!':
		return 0, "", 0, false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == ':' && (i+1 == len(rest) || rest[i+1] == ' ') {
			key := strings.TrimRight(rest[:i], " ")
			if key == "" {
				return 0, "", 0, false
			}
			return pos, key, pos + i + 1, true
		}
	}
	return 0, "", 0, false
}
