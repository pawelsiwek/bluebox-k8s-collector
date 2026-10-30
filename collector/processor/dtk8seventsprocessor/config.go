// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
package dtk8seventsprocessor

// Config holds the identity and throttle limits for this processor.
type Config struct {
	// ClusterName is the human-readable cluster name, used on OOM inferred event records.
	ClusterName string `mapstructure:"cluster_name"`

	// ClusterUID is the kube-system namespace UID used as the stable cluster identity.
	ClusterUID string `mapstructure:"cluster_uid"`

	// MaxEventsPerEntityPerHour limits KUBERNETES_EVENT records per
	// (cluster, namespace, involvedObject, reason) tuple per 60-minute bucket.
	MaxEventsPerEntityPerHour int `mapstructure:"max_events_per_entity_per_hour"`

	// MaxEventsPerClusterPerHour is the total KUBERNETES_EVENT ceiling per hour.
	MaxEventsPerClusterPerHour int `mapstructure:"max_events_per_cluster_per_hour"`
}

func (c *Config) Validate() error { return nil }
