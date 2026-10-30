package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	maxRequestBytes  = 1 << 20
	maxResponseBytes = 4 << 20
	maxErrorBytes    = 4096
	upstreamTimeout  = 60 * time.Second
)

// JSON-RPC codes returned by the redactor itself.
const (
	rpcInvalidParams = -32602
	rpcInternalError = -32603
)

var (
	forwardRequestHeaders  = []string{"Mcp-Session-Id", "Content-Type", "Accept", "Mcp-Protocol-Version"}
	forwardResponseHeaders = []string{"Mcp-Session-Id", "Content-Type", "Mcp-Protocol-Version"}
)

// proxy fronts kubernetes-mcp-server: it refuses non-read-only tool calls and
// redacts every tool result before it leaves the cluster.
type proxy struct {
	upstream string
	client   *http.Client
	log      *slog.Logger
}

func newProxy(upstream string, log *slog.Logger) *proxy {
	return &proxy{
		upstream: upstream,
		client: &http.Client{
			Timeout: upstreamTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log: log,
	}
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	tool   string
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		p.handlePost(w, r)
	case http.MethodDelete:
		p.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *proxy) handleDelete(w http.ResponseWriter, r *http.Request) {
	resp, err := p.forward(r, http.MethodDelete, nil)
	if err != nil {
		p.log.WarnContext(r.Context(), "upstream delete failed", "error", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
	copyHeaders(w.Header(), resp.Header, forwardResponseHeaders)
	w.WriteHeader(resp.StatusCode)
}

func (p *proxy) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	if len(body) > maxRequestBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		http.Error(w, "JSON-RPC batches are not supported", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		http.Error(w, "body must be a single JSON-RPC request or notification", http.StatusBadRequest)
		return
	}
	if req.Method == "tools/call" {
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, req.ID, rpcInvalidParams, "invalid tools/call params")
			return
		}
		req.tool = params.Name
		if reason := toolCallDenial(params.Name, params.Arguments); reason != "" {
			p.log.InfoContext(r.Context(), "tool call refused", "mcp.tool_name", params.Name, "reason", reason)
			writeRPCError(w, http.StatusOK, req.ID, rpcInvalidParams, reason)
			return
		}
	}

	resp, err := p.forward(r, http.MethodPost, body)
	if err != nil {
		p.log.WarnContext(r.Context(), "upstream request failed", "rpc.method", req.Method, "error", err)
		writeRPCError(w, http.StatusBadGateway, req.ID, rpcInternalError, "upstream unavailable")
		return
	}
	defer resp.Body.Close()

	if len(req.ID) == 0 || resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		copyHeaders(w.Header(), resp.Header, forwardResponseHeaders)
		w.WriteHeader(resp.StatusCode)
		if len(raw) > 0 {
			_, _ = w.Write([]byte(redactText(string(raw))))
		}
		return
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		p.relaySSE(w, resp, req)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		writeRPCError(w, http.StatusBadGateway, req.ID, rpcInternalError, "upstream response unreadable or too large")
		return
	}
	out, err := transformResponse(req.Method, req.tool, raw)
	if err != nil {
		p.log.WarnContext(r.Context(), "upstream response refused", "rpc.method", req.Method, "error", err)
		writeRPCError(w, http.StatusBadGateway, req.ID, rpcInternalError, "upstream response could not be redacted")
		return
	}
	copyHeaders(w.Header(), resp.Header, forwardResponseHeaders)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// relaySSE forwards only the response to this request, redacted, and drops
// server-initiated messages so nothing unredacted rides along on the stream.
func (p *proxy) relaySSE(w http.ResponseWriter, resp *http.Response, req rpcRequest) {
	copyHeaders(w.Header(), resp.Header, forwardResponseHeaders)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(payload []byte) {
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fail := func(msg string) {
		emit(rpcErrorBody(req.ID, rpcInternalError, msg))
	}

	br := bufio.NewReader(io.LimitReader(resp.Body, maxResponseBytes+1))
	var data []string
	read := 0
	for {
		line, err := br.ReadString('\n')
		read += len(line)
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		}
		eventEnd := (trimmed == "" && len(line) > 0) || (err != nil && len(data) > 0)
		if eventEnd && len(data) > 0 {
			payload := strings.Join(data, "\n")
			data = nil
			if isResponse(payload) {
				out, terr := transformResponse(req.Method, req.tool, []byte(payload))
				if terr != nil {
					p.log.Warn("upstream sse response refused", "rpc.method", req.Method, "error", terr)
					fail("upstream response could not be redacted")
					return
				}
				emit(out)
				return
			}
		}
		if err != nil {
			if read > maxResponseBytes {
				fail("upstream response too large")
			} else if !errors.Is(err, io.EOF) {
				fail("upstream stream failed")
			} else {
				fail("upstream stream ended without a response")
			}
			return
		}
	}
}

func isResponse(payload string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &m) != nil {
		return true // fail closed in transformResponse rather than drop silently
	}
	_, hasID := m["id"]
	_, hasResult := m["result"]
	_, hasErr := m["error"]
	return hasID && (hasResult || hasErr)
}

func (p *proxy) forward(r *http.Request, method string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	up, err := http.NewRequestWithContext(r.Context(), method, p.upstream, rd)
	if err != nil {
		return nil, err
	}
	copyHeaders(up.Header, r.Header, forwardRequestHeaders)
	return p.client.Do(up)
}

func copyHeaders(dst, src http.Header, allow []string) {
	for _, k := range allow {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

// transformResponse redacts one JSON-RPC response; it errors instead of passing through anything it cannot read.
func transformResponse(method, tool string, raw []byte) ([]byte, error) {
	var msg map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&msg); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if e, ok := msg["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok {
			e["message"] = redactText(m)
		}
		if d, ok := e["data"]; ok {
			e["data"] = redactJSONValue(d, "")
		}
	}
	if res, ok := msg["result"]; ok {
		resMap, ok := res.(map[string]any)
		if !ok {
			return nil, errors.New("result is not an object")
		}
		switch method {
		case "tools/list":
			if err := filterToolList(resMap); err != nil {
				return nil, err
			}
		case "tools/call":
			if err := redactToolResult(resMap, tool == "pods_log"); err != nil {
				return nil, err
			}
		}
	} else if _, ok := msg["error"]; !ok {
		return nil, errors.New("neither result nor error")
	}
	return marshalNoEscape(msg)
}

func filterToolList(res map[string]any) error {
	tools, ok := res["tools"].([]any)
	if !ok {
		return errors.New("tools/list result has no tools array")
	}
	kept := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		if !readOnlyTools[name] {
			continue
		}
		ann, _ := tm["annotations"].(map[string]any)
		if ann == nil {
			ann = map[string]any{}
		}
		// The allowlist above is what makes these tools read-only at this boundary.
		ann["readOnlyHint"] = true
		ann["destructiveHint"] = false
		tm["annotations"] = ann
		kept = append(kept, tm)
	}
	res["tools"] = kept
	return nil
}

// redactToolResult redacts every content item; isLog switches text items to the
// pattern-based log redactor and marks the result so the caller knows it ran.
func redactToolResult(res map[string]any, isLog bool) error {
	redact := redactText
	if isLog {
		redact = redactLogText
		meta, _ := res["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta[logRedactionMarker] = "pattern-based"
		res["_meta"] = meta
	}
	content, ok := res["content"].([]any)
	if !ok {
		return errors.New("tools/call result has no content array")
	}
	kept := make([]any, 0, len(content))
	for _, c := range content {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch cm["type"] {
		case "text":
			s, _ := cm["text"].(string)
			cm["text"] = redact(s)
			kept = append(kept, cm)
		case "resource":
			rm, ok := cm["resource"].(map[string]any)
			if !ok {
				continue
			}
			delete(rm, "blob")
			if s, ok := rm["text"].(string); ok {
				rm["text"] = redact(s)
			}
			kept = append(kept, cm)
		}
	}
	res["content"] = kept
	if sc, ok := res["structuredContent"]; ok {
		res["structuredContent"] = redactJSONValue(sc, "")
	}
	return nil
}

func rpcErrorBody(id json.RawMessage, code int, msg string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
	return b
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(rpcErrorBody(id, code, msg))
}
