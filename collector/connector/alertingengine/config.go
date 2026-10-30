// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
package alertingengine

// Config for the alertingengine connector.
type Config struct {
	// WindowSizeSeconds is the evaluation window length in seconds. Default: 60.
	WindowSizeSeconds int `mapstructure:"window_size_seconds"`

	// GracePeriodSeconds is additional time after window expiry before evaluation.
	// Default: 0.
	GracePeriodSeconds int `mapstructure:"grace_period_seconds"`

	// Rules is the ordered list of anomaly-detection rules.
	Rules []Rule `mapstructure:"rules"`
}

// Rule defines one anomaly-detection condition.
type Rule struct {
	// ID is a unique stable identifier for this rule (used in event schema IDs).
	ID string `mapstructure:"id"`

	// MetricKey is the OTel metric name to watch (e.g. "k8s.container.restarts").
	MetricKey string `mapstructure:"metric_key"`

	// GroupBy lists resource or data-point attribute names whose combination
	// defines a distinct evaluation group (one trigger state per combination).
	GroupBy []string `mapstructure:"group_by"`

	// Aggregation is how to reduce data points in a window: max, min, avg, sum.
	Aggregation string `mapstructure:"aggregation"`

	// Threshold defines the value trigger condition.
	Threshold Threshold `mapstructure:"threshold"`

	// TimeTriggerCount is how many consecutive fired windows must occur before
	// the alert transitions to ACTIVE.
	TimeTriggerCount int `mapstructure:"time_trigger_count"`

	// EventType maps to event.type in the emitted log record.
	// Values: AVAILABILITY_EVENT, ERROR_EVENT, RESOURCE_CONTENTION_EVENT.
	EventType string `mapstructure:"event_type"`

	// EventName is the event.name template. Supports {{dim}}, {{value}}, {{window}}.
	EventName string `mapstructure:"event_name"`

	// EventDescription is the event.description template.
	EventDescription string `mapstructure:"event_description"`
}

// Threshold defines the comparison operator and reference value.
type Threshold struct {
	// Operator is one of: >, <, >=, <=, ==
	Operator string `mapstructure:"operator"`
	// Value is the reference value.
	Value float64 `mapstructure:"value"`
}

func (c *Config) Validate() error { return nil }

func defaultRules() []Rule {
	return []Rule{
		{
			ID:               "container_crash_loop",
			MetricKey:        "k8s.container.restarts",
			GroupBy:          []string{"k8s.cluster.uid", "k8s.namespace.name", "k8s.workload.name", "k8s.container.name"},
			Aggregation:      "max",
			Threshold:        Threshold{Operator: ">", Value: 5},
			TimeTriggerCount: 2,
			EventType:        "AVAILABILITY_EVENT",
			EventName:        "Container crash loop detected",
			EventDescription: "Container {{k8s.container.name}} restarted {{value}} times in the last {{window}}s",
		},
		{
			ID:               "node_not_ready",
			MetricKey:        "k8s.node.condition_ready",
			GroupBy:          []string{"k8s.cluster.uid", "k8s.node.name"},
			Aggregation:      "min",
			Threshold:        Threshold{Operator: "<", Value: 1},
			TimeTriggerCount: 2,
			EventType:        "AVAILABILITY_EVENT",
			EventName:        "Node not ready",
			EventDescription: "Node {{k8s.node.name}} has been not-ready for {{window}}s",
		},
		{
			ID:               "deployment_unavailable",
			MetricKey:        "k8s.deployment.available",
			GroupBy:          []string{"k8s.cluster.uid", "k8s.namespace.name", "k8s.deployment.name"},
			Aggregation:      "min",
			Threshold:        Threshold{Operator: "<", Value: 1},
			TimeTriggerCount: 3,
			EventType:        "AVAILABILITY_EVENT",
			EventName:        "Deployment has no available replicas",
			EventDescription: "Deployment {{k8s.deployment.name}} in {{k8s.namespace.name}} has 0 available replicas",
		},
		{
			ID:               "daemonset_misscheduled",
			MetricKey:        "k8s.daemonset.misscheduled_nodes",
			GroupBy:          []string{"k8s.cluster.uid", "k8s.namespace.name", "k8s.daemonset.name"},
			Aggregation:      "max",
			Threshold:        Threshold{Operator: ">", Value: 0},
			TimeTriggerCount: 2,
			EventType:        "ERROR_EVENT",
			EventName:        "DaemonSet has misscheduled pods",
			EventDescription: "DaemonSet {{k8s.daemonset.name}} has {{value}} misscheduled pods",
		},
	}
}
