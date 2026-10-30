package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// logRedactionMarker is set on a tools/call result whose log text went through redactLogText.
const logRedactionMarker = "bluebox.redaction"

// droppedLine replaces a log line the redactor could not process safely.
const droppedLine = "<line dropped by redactor>"

// maxLogLineBytes bounds per-line regex work; a longer line is dropped, not passed through.
const maxLogLineBytes = 64 << 10

// Replacement templates: drop the whole match, or keep the key/scheme in group 1.
const (
	replAll     = redactedValue
	replKeepKey = "${1}" + redactedValue
)

// logPatterns are applied in order. Pattern-based and best effort: a secret in
// a format not listed here is not caught.
var logPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private key blocks on a single line (multi-line blocks are handled by the line state machine).
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), replAll},
	// Authorization headers: Bearer / Basic / Api-Token schemes.
	{regexp.MustCompile(`(?i)(\b(?:bearer|basic|api-token)\s+)[A-Za-z0-9._~+/=\-]{8,}`), replKeepKey},
	// Dynatrace tokens: the documented dt0<x><nn>.<24>.<64> shape, then any dt0 prefixed token.
	{regexp.MustCompile(`\bdt0[a-z]\d{2}\.[A-Z0-9]{24}\.[A-Z0-9]{64}\b`), replAll},
	{regexp.MustCompile(`\bdt0[a-z]\d{2}\.[A-Za-z0-9]{8,}(?:\.[A-Za-z0-9]{8,})?\b`), replAll},
	// JWTs: three base64url segments, header starting with eyJ.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}`), replAll},
	// AWS access key ids.
	{regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`), replAll},
	// GitHub tokens.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,255}|github_pat_[A-Za-z0-9_]{40,255})\b`), replAll},
	// Slack tokens.
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9\-]{10,}`), replAll},
	// Connection strings with credentials: scheme://user:pass@host keeps the scheme.
	{regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.\-]*://)[^\s:/@'"]+:[^\s@'"]+@`), "${1}" + redactedValue + "@"},
	// Key/value assignments: env (KEY=v), YAML/logfmt (key: v), JSON ("key": "v"), CLI (--key=v).
	// The key must END in a credential word, so "tokenizer" or "secretariat" never match.
	{regexp.MustCompile(`(?i)((?:^|[\s,{;(\-"'])["']?[A-Za-z0-9_.\-]*(?:password|passwd|pwd|secret|token|api[_\-]?key|credentials?|aws_secret_access_key)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;}"']+)`), replKeepKey},
}

// redactLogLine applies logPatterns to one line; ok is false when the line must be dropped.
func redactLogLine(line string) (string, bool) {
	if len(line) > maxLogLineBytes || !utf8.ValidString(line) {
		return "", false
	}
	for _, p := range logPatterns {
		line = p.re.ReplaceAllString(line, p.repl)
	}
	return line, true
}

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// redactLogText redacts free-form log text line by line. Lines inside a
// multi-line PEM private key block are replaced as a whole; a line that cannot
// be processed is dropped, never passed through raw.
func redactLogText(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inKey := false
	for _, line := range lines {
		if inKey {
			if pemEnd.MatchString(line) {
				inKey = false
				after := line[pemEnd.FindStringIndex(line)[1]:]
				if red, ok := redactLogLine(after); ok && strings.TrimSpace(after) != "" {
					out = append(out, red)
				}
			}
			continue
		}
		if loc := pemBegin.FindStringIndex(line); loc != nil && !pemEnd.MatchString(line[loc[1]:]) {
			inKey = true
			if red, ok := redactLogLine(line[:loc[0]]); ok {
				out = append(out, red+redactedValue)
			} else {
				out = append(out, droppedLine)
			}
			continue
		}
		red, ok := redactLogLine(line)
		if !ok {
			red = droppedLine
		}
		out = append(out, red)
	}
	return strings.Join(out, "\n")
}
