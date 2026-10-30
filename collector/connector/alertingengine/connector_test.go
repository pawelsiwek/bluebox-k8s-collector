package alertingengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/connector/connectortest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func restartsMetric(value int64, container string) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	attrs := rm.Resource().Attributes()
	attrs.PutStr("k8s.cluster.uid", "uid-1")
	attrs.PutStr("k8s.namespace.name", "demo")
	attrs.PutStr("k8s.workload.name", "web")
	attrs.PutStr("k8s.container.name", container)
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("k8s.container.restarts")
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(value)
	return md
}

func attr(t *testing.T, lr plog.LogRecord, key string) string {
	t.Helper()
	v, ok := lr.Attributes().Get(key)
	if !ok {
		t.Fatalf("attribute %q missing", key)
	}
	return v.AsString()
}

// The crash-loop rule needs two consecutive windows above the threshold
// before it goes ACTIVE, and emits CLOSED once the value clears.
func TestCrashLoopActivatesAfterTriggerCountAndCloses(t *testing.T) {
	sink := new(consumertest.LogsSink)
	cfg := createDefaultConfig().(*Config)
	c := newConnector(cfg, sink, zap.NewNop())
	t0 := time.Unix(1_700_000_000, 0)
	window := time.Duration(cfg.WindowSizeSeconds) * time.Second

	feed := func(at time.Time, v int64) {
		md := restartsMetric(v, "app")
		rm := md.ResourceMetrics().At(0)
		c.mu.Lock()
		c.ingestMetric(at, rm.ScopeMetrics().At(0).Metrics().At(0), rm.Resource().Attributes())
		c.mu.Unlock()
	}

	feed(t0, 9)
	c.evaluateExpiredWindows(t0.Add(window))
	if n := sink.LogRecordCount(); n != 0 {
		t.Fatalf("first breaching window emitted %d records, want 0 (time_trigger_count=2)", n)
	}

	feed(t0.Add(window), 9)
	c.evaluateExpiredWindows(t0.Add(2 * window))
	if n := sink.LogRecordCount(); n != 1 {
		t.Fatalf("second breaching window emitted %d records, want 1", n)
	}
	lr := sink.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	if got := attr(t, lr, "event.status"); got != "ACTIVE" {
		t.Fatalf("event.status = %q, want ACTIVE", got)
	}
	if got := attr(t, lr, "event.provider"); got != "KUBERNETES_ANOMALY_DETECTION" {
		t.Fatalf("event.provider = %q", got)
	}
	if got := attr(t, lr, "event.description"); got != "Container app restarted 9 times in the last 60s" {
		t.Fatalf("event.description = %q", got)
	}

	feed(t0.Add(2*window), 9)
	c.evaluateExpiredWindows(t0.Add(3 * window))
	if n := sink.LogRecordCount(); n != 1 {
		t.Fatalf("still-active alert re-emitted: %d records", n)
	}

	feed(t0.Add(3*window), 0)
	c.evaluateExpiredWindows(t0.Add(4 * window))
	if n := sink.LogRecordCount(); n != 2 {
		t.Fatalf("cleared window emitted %d records total, want 2", n)
	}
	lr = sink.AllLogs()[1].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	if got := attr(t, lr, "event.status"); got != "CLOSED" {
		t.Fatalf("event.status = %q, want CLOSED", got)
	}
}

// Separate dimension groups keep separate trigger state.
func TestGroupsAreIndependent(t *testing.T) {
	sink := new(consumertest.LogsSink)
	cfg := createDefaultConfig().(*Config)
	c := newConnector(cfg, sink, zap.NewNop())
	t0 := time.Unix(1_700_000_000, 0)
	w := time.Minute
	for i := 0; i < 2; i++ {
		for _, ctr := range []string{"a", "b"} {
			v := int64(0)
			if ctr == "a" {
				v = 10
			}
			md := restartsMetric(v, ctr)
			rm := md.ResourceMetrics().At(0)
			c.mu.Lock()
			c.ingestMetric(t0.Add(time.Duration(i)*w), rm.ScopeMetrics().At(0).Metrics().At(0), rm.Resource().Attributes())
			c.mu.Unlock()
		}
		c.evaluateExpiredWindows(t0.Add(time.Duration(i+1) * w))
	}
	if n := sink.LogRecordCount(); n != 1 {
		t.Fatalf("got %d records, want 1 (only container a breaches)", n)
	}
	lr := sink.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	if got := attr(t, lr, "k8s.container.name"); got != "a" {
		t.Fatalf("alert for container %q, want a", got)
	}
}

// A downstream failure is logged, not swallowed (Bluebox port change).
func TestForwardErrorIsLogged(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	cfg := createDefaultConfig().(*Config)
	for i := range cfg.Rules {
		cfg.Rules[i].TimeTriggerCount = 1
	}
	c := newConnector(cfg, consumertest.NewErr(errors.New("boom")), zap.New(core))
	t0 := time.Unix(1_700_000_000, 0)
	md := restartsMetric(9, "app")
	rm := md.ResourceMetrics().At(0)
	c.mu.Lock()
	c.ingestMetric(t0, rm.ScopeMetrics().At(0).Metrics().At(0), rm.Resource().Attributes())
	c.mu.Unlock()
	c.evaluateExpiredWindows(t0.Add(time.Minute))
	if logs.FilterMessage("alertingengine: failed to forward alert records").Len() != 1 {
		t.Fatalf("forward error not logged: %v", logs.All())
	}
}

func TestFactoryLifecycle(t *testing.T) {
	f := NewFactory()
	if f.Type().String() != "alertingengine" {
		t.Fatalf("type = %s", f.Type())
	}
	conn, err := f.CreateMetricsToLogs(context.Background(), connectortest.NewNopSettings(f.Type()),
		f.CreateDefaultConfig(), new(consumertest.LogsSink))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	if err := conn.ConsumeMetrics(context.Background(), restartsMetric(1, "x")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
