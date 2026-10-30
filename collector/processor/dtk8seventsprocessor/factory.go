// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
package dtk8seventsprocessor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

var typeStr = component.MustNewType("dtk8seventsprocessor")

func NewFactory() processor.Factory {
	return processor.NewFactory(
		typeStr,
		createDefaultConfig,
		processor.WithLogs(createLogsProcessor, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		MaxEventsPerEntityPerHour:  1000,
		MaxEventsPerClusterPerHour: 10000,
	}
}

func createLogsProcessor(
	_ context.Context,
	_ processor.Settings,
	cfg component.Config,
	nextConsumer consumer.Logs,
) (processor.Logs, error) {
	return newProcessor(cfg.(*Config), nextConsumer), nil
}
