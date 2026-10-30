// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
package dtk8stopology

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

var typeStr = component.MustNewType("dtk8stopology")

func NewFactory() processor.Factory {
	return processor.NewFactory(
		typeStr,
		createDefaultConfig,
		processor.WithLogs(createLogsProcessor, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{}
}

func createLogsProcessor(
	_ context.Context,
	_ processor.Settings,
	cfg component.Config,
	nextConsumer consumer.Logs,
) (processor.Logs, error) {
	return &dtk8sTopologyProcessor{
		config:       cfg.(*Config),
		nextConsumer: nextConsumer,
		podCache:     make(map[string]podWorkload),
	}, nil
}
