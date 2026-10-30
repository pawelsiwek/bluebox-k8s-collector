// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
package alertingengine

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
)

var typeStr = component.MustNewType("alertingengine")

func NewFactory() connector.Factory {
	return connector.NewFactory(
		typeStr,
		createDefaultConfig,
		connector.WithMetricsToLogs(createMetricsToLogsConnector, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		WindowSizeSeconds:  60,
		GracePeriodSeconds: 0,
		Rules:              defaultRules(),
	}
}

func createMetricsToLogsConnector(
	_ context.Context,
	set connector.Settings,
	cfg component.Config,
	nextConsumer consumer.Logs,
) (connector.Metrics, error) {
	return newConnector(cfg.(*Config), nextConsumer, set.Logger), nil
}
