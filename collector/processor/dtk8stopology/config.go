// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
package dtk8stopology

// Config holds identity fields stamped on every CONTAINER topology record.
// RefreshInterval and a heartbeat loop are intentionally absent — topology
// pull cycles refresh entities every 120 s.
type Config struct {
	ClusterName string `mapstructure:"cluster_name"`
	ClusterUID  string `mapstructure:"cluster_uid"`
}

func (c *Config) Validate() error { return nil }
