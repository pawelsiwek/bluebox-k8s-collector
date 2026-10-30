package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// secretsInFixtures are the plaintext values planted in testdata; none may survive redaction.
var secretsInFixtures = []string{
	"hunter2", "line-one-secret", "line-two-secret", "quoted-secret", "probe-token-xyz",
	"s3cret", "abc123", "p4ss", "inline-token", "abc-raw-token", "Upper-Case-Pass",
	"leak", "plain-secret", "probe-secret", "nested-secret", "tkn", "--password=x", "--key=abc",
	"last-applied-configuration",
}

func TestRedactText_Golden(t *testing.T) {
	cases := []struct {
		in       string
		mustKeep []string
	}{
		{"pod.yaml", []string{"secretKeyRef:", "name: db-creds", "key: password", "image: shop/checkout:1.4.2",
			"prometheus.io/scrape", "path: /healthz", "name: Authorization", "phase: Running", "serviceAccountToken: placeholder"}},
		{"pod-oneline-annotation.yaml", []string{"team: payments", "name: api"}},
		{"pod.json", []string{`"secretKeyRef"`, `"X-Token"`, `"team":"payments"`, `"path":"/ready"`}},
		{"table.txt", []string{"CrashLoopBackOff", "cart-55b8"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join("testdata", tc.in))
			if err != nil {
				t.Fatal(err)
			}
			got := redactText(string(in))
			golden := filepath.Join("testdata", tc.in+".golden")
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
				t.Errorf("redaction drifted from %s:\n%s", golden, got)
			}
			for _, s := range secretsInFixtures {
				if strings.Contains(got, s) {
					t.Errorf("plaintext %q survived redaction", s)
				}
			}
			for _, s := range tc.mustKeep {
				if !strings.Contains(got, s) {
					t.Errorf("non-secret %q was lost", s)
				}
			}
		})
	}
}
