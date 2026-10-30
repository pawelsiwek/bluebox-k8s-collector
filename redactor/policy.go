package main

import "strings"

// readOnlyTools is the kubernetes-mcp-server tool allowlist. Excluded on
// purpose: anything that writes or executes, configuration_view (kubeconfig),
// nodes_log and nodes_stats_summary (kubelet proxy), and helm_list (release values).
var readOnlyTools = map[string]bool{
	"events_list":            true,
	"namespaces_list":        true,
	"nodes_top":              true,
	"pods_get":               true,
	"pods_list":              true,
	"pods_list_in_namespace": true,
	"pods_log":               true,
	"pods_top":               true,
	"resources_get":          true,
	"resources_list":         true,
}

// deniedKinds hold credentials or configuration values the agent must never read.
var deniedKinds = map[string]bool{
	"secret":     true,
	"secrets":    true,
	"configmap":  true,
	"configmaps": true,
}

// toolCallDenial returns a non-empty reason when a tools/call must not reach upstream.
func toolCallDenial(name string, args map[string]any) string {
	if !readOnlyTools[name] {
		return "tool is not permitted by the read-only policy"
	}
	if strings.HasPrefix(name, "resources_") {
		kind, _ := args["kind"].(string)
		if deniedKinds[strings.ToLower(strings.TrimSpace(kind))] {
			return "Secret and ConfigMap resources are not readable"
		}
	}
	return ""
}
