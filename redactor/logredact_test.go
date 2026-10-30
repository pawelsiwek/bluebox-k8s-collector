package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logSecrets are assembled at runtime so no credential-shaped literal is ever
// committed (secret scanners would flag the fixture, not the code under test).
func logSecrets() map[string]string {
	rep := func(s string, n int) string { return strings.Repeat(s, n) }
	return map[string]string{
		"BEARER":        "abcDEF123" + "ghiJKL456.mno",
		"BASIC":         "dXNlcjpw" + "YXNzd29yZA==",
		"DT_FULL":       "dt0" + "c01." + rep("A1", 12) + "." + rep("B2C3", 16),
		"DT_SHORT":      "dt0" + "s16." + "PLATFORMTOKEN" + "XYZ.second" + "PART99",
		"JWT":           "ey" + "JhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiIxMjM0In0." + "sig" + "nature_123",
		"AKID":          "AK" + "IA" + "ABCDEFGHIJKLMNOP",
		"AWS_SECRET":    "wJalr" + "XUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"GHP":           "gh" + "p_" + rep("a1B2", 9),
		"SLACK":         "xo" + "xb-" + "12345678901-abcdefghijk",
		"PEM_BODY":      "MIIEpAIBAAKCAQEA" + "privatekeybodyline",
		"JSON_PW":       "json-db-" + "pw-9",
		"JSON_KEY":      "json-api-" + "key-7",
		"ENV_PW":        "env-db-" + "pw-3",
		"LOGFMT_SECRET": "logfmt-" + "client-secret-5",
		"YAML_CRED":     "yaml-" + "cred-1",
		"CONN_PW":       "conn-" + "pw-8",
		"CLI_PW":        "cli-" + "pw-4",
	}
}

func renderLogFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "pods-log.tmpl.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for k, v := range logSecrets() {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	if strings.Contains(s, "{{") {
		t.Fatal("fixture has an unfilled placeholder")
	}
	return s
}

func TestRedactLogText_Golden(t *testing.T) {
	got := redactLogText(renderLogFixture(t))
	golden := filepath.Join("testdata", "pods-log.txt.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("log redaction drifted from %s:\n%s", golden, got)
	}
	for name, secret := range logSecrets() {
		if strings.Contains(got, secret) {
			t.Errorf("%s survived redaction", name)
		}
	}
	for _, keep := range []string{
		"tokenizer loaded 32000 tokens", "the secretariat approved", "passwordless login enabled",
		"password reset requested for user alice; token expired", "https://api.example.com/v1/orders?page=2",
		"bearer auth configured; basic metrics enabled", "OOMKilled: container memhog", `"pool": 10`,
		"region=eu-west-1", "retries=3", "postgres://", "@db.prod.svc:5432/orders",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("over-redaction: %q was lost", keep)
		}
	}
}

// A line the redactor cannot process is dropped, never passed through raw.
func TestRedactLogText_FailsClosed(t *testing.T) {
	secret := "leaked-" + "value-1"
	invalid := "password=" + secret + " \xff\xfe"
	long := "token=" + secret + " " + strings.Repeat("x", maxLogLineBytes)
	got := redactLogText("ok line\n" + invalid + "\n" + long + "\nlast line")
	if strings.Contains(got, secret) || strings.Contains(got, "\xff") {
		t.Fatalf("unprocessable line passed through: %q", got)
	}
	if want := "ok line\n" + droppedLine + "\n" + droppedLine + "\nlast line"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// An unterminated private key block swallows everything after it.
	got = redactLogText("before\n-----BEGIN PRIVATE KEY-----\nMIIEvQ" + "keybody\nmore")
	if got != "before\n"+redactedValue {
		t.Fatalf("unterminated PEM: got %q", got)
	}
}

// pods_log results go through the log redactor and carry the redaction marker;
// other tools keep the structural redactor and no marker.
func TestTransformResponse_PodsLogMarkedAndRedacted(t *testing.T) {
	secret := "env-db-" + "pw-3"
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "DB_PASSWORD=" + secret}},
	}})
	out, err := transformResponse("tools/call", "pods_log", body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), secret) {
		t.Fatalf("secret survived: %s", out)
	}
	var msg struct {
		Result struct {
			Meta map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &msg); err != nil || msg.Result.Meta[logRedactionMarker] != "pattern-based" {
		t.Fatalf("missing redaction marker: %s (%v)", out, err)
	}
	out, err = transformResponse("tools/call", "pods_list", body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), logRedactionMarker) {
		t.Fatalf("marker set on a non-log tool: %s", out)
	}
}

func TestParseFlags(t *testing.T) {
	c, err := parseFlags([]string{"--listen", "127.0.0.1:8080", "--upstream", "http://k8s-mcp.ns.svc:8081/mcp"})
	if err != nil || c.listen != "127.0.0.1:8080" || c.upstream != "http://k8s-mcp.ns.svc:8081/mcp" {
		t.Fatalf("got %+v, %v", c, err)
	}
	for name, args := range map[string][]string{
		"missing upstream":  {"--listen", "127.0.0.1:8080"},
		"non-http upstream": {"--upstream", "file:///etc/passwd"},
		"bad listen":        {"--listen", "8080", "--upstream", "http://x:1/mcp"},
		"unknown flag":      {"--upstream", "http://x:1/mcp", "--insecure"},
		"positional arg":    {"--upstream", "http://x:1/mcp", "extra"},
	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
}
