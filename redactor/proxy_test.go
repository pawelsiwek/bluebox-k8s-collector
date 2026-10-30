package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const podYAMLText = "spec:\n  containers:\n  - env:\n    - name: DB_PASSWORD\n      value: hunter2\n"

type fakeUpstream struct {
	calls   atomic.Int32
	lastHdr http.Header
	handler func(w http.ResponseWriter, body map[string]any)
}

func newRedactor(t *testing.T, up *fakeUpstream) *httptest.Server {
	t.Helper()
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.calls.Add(1)
		up.lastHdr = r.Header.Clone()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		up.handler(w, body)
	}))
	t.Cleanup(upSrv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(newMux(newProxy(upSrv.URL, log)))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func jsonResult(result any) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("X-Internal", "leak")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body["id"], "result": result})
	}
}

func toolText(t *testing.T, raw []byte) string {
	t.Helper()
	var msg struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if msg.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", msg.Error)
	}
	var parts []string
	for _, c := range msg.Result.Content {
		parts = append(parts, c.Type+":"+c.Text)
	}
	return strings.Join(parts, "|")
}

func TestToolCallResultIsRedacted_JSONAndSSE(t *testing.T) {
	result := map[string]any{"content": []any{
		map[string]any{"type": "text", "text": podYAMLText},
		map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
	}}
	for _, sse := range []bool{false, true} {
		up := &fakeUpstream{handler: jsonResult(result)}
		if sse {
			up.handler = func(w http.ResponseWriter, body map[string]any) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Mcp-Session-Id", "sess-1")
				notif, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"data": "value: hunter2"}})
				resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body["id"], "result": result})
				_, _ = io.WriteString(w, "event: message\ndata: "+string(notif)+"\n\nevent: message\ndata: "+string(resp)+"\n\n")
			}
		}
		srv := newRedactor(t, up)
		resp, raw := post(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"pods_get","arguments":{"name":"x"}}}`,
			map[string]string{"Mcp-Session-Id": "sess-1", "Authorization": "Bearer nope", "MCP-Protocol-Version": "2025-03-26"})
		if resp.Header.Get("Mcp-Session-Id") != "sess-1" || resp.Header.Get("X-Internal") != "" {
			t.Fatalf("sse=%v: response headers not filtered: %v", sse, resp.Header)
		}
		if up.lastHdr.Get("Authorization") != "" || up.lastHdr.Get("Mcp-Session-Id") != "sess-1" || up.lastHdr.Get("Mcp-Protocol-Version") != "2025-03-26" {
			t.Fatalf("sse=%v: upstream headers not filtered: %v", sse, up.lastHdr)
		}
		body := raw
		if sse {
			if strings.Contains(string(raw), "hunter2") {
				t.Fatalf("server notification leaked plaintext: %s", raw)
			}
			i := strings.Index(string(raw), "data: ")
			body = []byte(strings.TrimSpace(string(raw)[i+len("data: "):]))
		}
		got := toolText(t, body)
		if strings.Contains(got, "hunter2") || !strings.Contains(got, `value: "<redacted>"`) || strings.Contains(got, "image:") {
			t.Fatalf("sse=%v: result not redacted or non-text content kept: %q", sse, got)
		}
	}
}

func TestToolCallRefusedWithoutCallingUpstream(t *testing.T) {
	cases := map[string]string{
		"write tool":           `{"name":"resources_delete","arguments":{"kind":"Pod"}}`,
		"exec":                 `{"name":"pods_exec","arguments":{}}`,
		"secret get":           `{"name":"resources_get","arguments":{"apiVersion":"v1","kind":"Secret","name":"db"}}`,
		"configmap list lower": `{"name":"resources_list","arguments":{"apiVersion":"v1","kind":" configmap "}}`,
		"kubeconfig":           `{"name":"configuration_view","arguments":{}}`,
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			up := &fakeUpstream{handler: jsonResult(map[string]any{"content": []any{}})}
			srv := newRedactor(t, up)
			_, raw := post(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":`+params+`}`, nil)
			if up.calls.Load() != 0 {
				t.Fatal("refused call reached upstream")
			}
			var msg struct {
				ID    int `json:"id"`
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil || msg.ID != 3 || msg.Error.Code != rpcInvalidParams {
				t.Fatalf("want JSON-RPC -32602 for id 3, got %s", raw)
			}
		})
	}
}

func TestToolsListFilteredToAllowlistAndMarkedReadOnly(t *testing.T) {
	up := &fakeUpstream{handler: jsonResult(map[string]any{"tools": []any{
		map[string]any{"name": "pods_list", "inputSchema": map[string]any{}},
		map[string]any{"name": "pods_exec"},
		map[string]any{"name": "resources_create_or_update"},
		map[string]any{"name": "resources_get", "annotations": map[string]any{"title": "Get"}},
	}})}
	srv := newRedactor(t, up)
	_, raw := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	var msg struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Annotations map[string]any `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range msg.Result.Tools {
		names = append(names, tool.Name)
		if tool.Annotations["readOnlyHint"] != true {
			t.Errorf("%s not marked readOnlyHint", tool.Name)
		}
	}
	if strings.Join(names, ",") != "pods_list,resources_get" {
		t.Fatalf("tools = %v", names)
	}
}

func TestUnreadableUpstreamFailsClosed(t *testing.T) {
	for name, handler := range map[string]func(http.ResponseWriter, map[string]any){
		"garbage json": func(w http.ResponseWriter, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `value: hunter2 {not json`)
		},
		"content missing": jsonResult(map[string]any{"raw": "value: hunter2"}),
		"sse without response": func(w http.ResponseWriter, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: value: hunter2\n\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newRedactor(t, &fakeUpstream{handler: handler})
			_, raw := post(t, srv, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"pods_log","arguments":{}}}`, nil)
			if strings.Contains(string(raw), "hunter2") || !strings.Contains(string(raw), `"code":-32603`) {
				t.Fatalf("want fail-closed JSON-RPC error, got %s", raw)
			}
		})
	}
}

func TestMalformedRequestsRejected(t *testing.T) {
	up := &fakeUpstream{handler: jsonResult(map[string]any{})}
	srv := newRedactor(t, up)
	for _, body := range []string{`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, `not json`, `{"jsonrpc":"2.0","id":1}`} {
		resp, _ := post(t, srv, body, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status %d, want 405", resp.StatusCode)
	}
	if up.calls.Load() != 0 {
		t.Fatal("malformed request reached upstream")
	}
}
