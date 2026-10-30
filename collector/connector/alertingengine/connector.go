// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
// Package alertingengine implements a MetricsToLogs OTel connector that
// evaluates anomaly-detection rules against incoming metric data points and
// emits KUBERNETES_ANOMALY_DETECTION Davis event log records.
//
// Each rule defines a metric key, a group-by dimension set, a windowed
// aggregation, a value threshold, and a time-trigger count. The connector
// maintains in-memory per-(rule, dimensionGroup) windows of 60-second
// duration (configurable). A background goroutine closes expired windows each
// second and evaluates the aggregate against the threshold. When
// consecutiveCount >= time_trigger_count the event transitions ACTIVE; when
// the threshold clears the event transitions CLOSED.
//
// State is in-memory only — a collector restart resets all windows and
// trigger states.
package alertingengine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

type stateKey struct {
	ruleID  string
	dimHash string
}

type windowBucket struct {
	startTime  time.Time
	dataPoints []float64
	dimensions map[string]string
}

type triggerState struct {
	consecutiveCount int
	lastFiredTime    time.Time
	active           bool
}

type alertingConnector struct {
	config       *Config
	nextConsumer consumer.Logs
	logger       *zap.Logger

	mu       sync.Mutex
	windows  map[stateKey]*windowBucket
	triggers map[stateKey]*triggerState

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func newConnector(cfg *Config, next consumer.Logs, logger *zap.Logger) *alertingConnector {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &alertingConnector{
		config:       cfg,
		nextConsumer: next,
		logger:       logger,
		windows:      make(map[stateKey]*windowBucket),
		triggers:     make(map[stateKey]*triggerState),
		stopCh:       make(chan struct{}),
	}
}

func (c *alertingConnector) Start(_ context.Context, _ component.Host) error {
	c.wg.Add(1)
	go c.evaluationLoop()
	return nil
}

func (c *alertingConnector) Shutdown(_ context.Context) error {
	close(c.stopCh)
	c.wg.Wait()
	return nil
}

func (c *alertingConnector) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// ConsumeMetrics ingests each data point into the active window for any
// matching rule × dimension combination.
func (c *alertingConnector) ConsumeMetrics(_ context.Context, md pmetric.Metrics) error {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			for k := 0; k < sm.Metrics().Len(); k++ {
				c.ingestMetric(now, sm.Metrics().At(k), rm.Resource().Attributes())
			}
		}
	}
	return nil
}

func (c *alertingConnector) ingestMetric(now time.Time, metric pmetric.Metric, resAttrs pcommon.Map) {
	for i := range c.config.Rules {
		rule := &c.config.Rules[i]
		if rule.MetricKey != metric.Name() {
			continue
		}

		add := func(dpAttrs pcommon.Map, value float64) {
			dims := extractDims(rule.GroupBy, dpAttrs, resAttrs)
			key := stateKey{ruleID: rule.ID, dimHash: hashDims(dims)}
			bucket, ok := c.windows[key]
			if !ok {
				// Create window only if none exists; the evaluationLoop owns expiry+deletion.
				// Adding to an already-expired (but not yet evaluated) window is fine —
				// the evaluationLoop will evaluate and delete it within 1s.
				bucket = &windowBucket{startTime: now, dimensions: dims}
				c.windows[key] = bucket
			}
			bucket.dataPoints = append(bucket.dataPoints, value)
		}

		dpFloat := func(dp pmetric.NumberDataPoint) float64 {
			if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
				return float64(dp.IntValue())
			}
			return dp.DoubleValue()
		}

		switch metric.Type() {
		case pmetric.MetricTypeGauge:
			for dp := 0; dp < metric.Gauge().DataPoints().Len(); dp++ {
				p := metric.Gauge().DataPoints().At(dp)
				add(p.Attributes(), dpFloat(p))
			}
		case pmetric.MetricTypeSum:
			for dp := 0; dp < metric.Sum().DataPoints().Len(); dp++ {
				p := metric.Sum().DataPoints().At(dp)
				add(p.Attributes(), dpFloat(p))
			}
		}
	}
}

func (c *alertingConnector) evaluationLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case now := <-ticker.C:
			c.evaluateExpiredWindows(now)
		}
	}
}

type expiredItem struct {
	key    stateKey
	bucket *windowBucket
}

func (c *alertingConnector) evaluateExpiredWindows(now time.Time) {
	windowDur := time.Duration(c.config.WindowSizeSeconds) * time.Second

	c.mu.Lock()
	var expired []expiredItem
	for key, bucket := range c.windows {
		if now.Sub(bucket.startTime) >= windowDur {
			expired = append(expired, expiredItem{key, bucket})
			delete(c.windows, key)
		}
	}
	c.mu.Unlock()

	if len(expired) == 0 {
		return
	}

	out := plog.NewLogs()

	for _, item := range expired {
		rule := c.findRule(item.key.ruleID)
		if rule == nil {
			continue
		}

		value := aggregate(item.bucket.dataPoints, rule.Aggregation)
		fired := evalThreshold(value, rule.Threshold)

		c.mu.Lock()
		state, ok := c.triggers[item.key]
		if !ok {
			state = &triggerState{}
			c.triggers[item.key] = state
		}

		var emitStatus string
		if fired {
			state.consecutiveCount++
			if state.consecutiveCount >= rule.TimeTriggerCount && !state.active {
				state.active = true
				state.lastFiredTime = now
				emitStatus = "ACTIVE"
			}
		} else {
			if state.active {
				emitStatus = "CLOSED"
				state.active = false
			}
			state.consecutiveCount = 0
		}
		c.mu.Unlock()

		if emitStatus != "" {
			appendAlertRecord(out, rule, item.bucket.dimensions, emitStatus, value, c.config.WindowSizeSeconds)
		}
	}

	if out.ResourceLogs().Len() > 0 {
		// Bluebox port: the upstream code discarded this error; surface it.
		if err := c.nextConsumer.ConsumeLogs(context.Background(), out); err != nil {
			c.logger.Error("alertingengine: failed to forward alert records",
				zap.Int("records", out.LogRecordCount()), zap.Error(err))
		}
	}
}

func (c *alertingConnector) findRule(id string) *Rule {
	for i := range c.config.Rules {
		if c.config.Rules[i].ID == id {
			return &c.config.Rules[i]
		}
	}
	return nil
}

func appendAlertRecord(out plog.Logs, rule *Rule, dims map[string]string, status string, value float64, windowSecs int) {
	rl := out.ResourceLogs().AppendEmpty()
	if v, ok := dims["k8s.cluster.uid"]; ok {
		rl.Resource().Attributes().PutStr("k8s.cluster.uid", v)
	}
	if v, ok := dims["k8s.cluster.name"]; ok {
		rl.Resource().Attributes().PutStr("k8s.cluster.name", v)
	}

	rec := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	rec.Attributes().PutStr("event.provider", "KUBERNETES_ANOMALY_DETECTION")
	rec.Attributes().PutStr("event.type", rule.EventType)
	rec.Attributes().PutStr("event.kind", "DAVIS_EVENT")
	rec.Attributes().PutStr("event.status", status)
	rec.Attributes().PutStr("event.name", expandTemplate(rule.EventName, dims, value, windowSecs))
	rec.Attributes().PutStr("event.description", expandTemplate(rule.EventDescription, dims, value, windowSecs))
	rec.Attributes().PutBool("dt.davis.is_merging_allowed", true)
	rec.Attributes().PutBool("dt.davis.is_rootcause_relevant", true)
	rec.Attributes().PutInt("dt.davis.timeout", 10)
	rec.Attributes().PutStr("dt.settings.schema_id", "custom:alerting.k8s."+rule.ID)

	for k, v := range dims {
		rec.Attributes().PutStr(k, v)
	}
}

// --- helpers -----------------------------------------------------------------

func extractDims(groupBy []string, dpAttrs, resAttrs pcommon.Map) map[string]string {
	dims := make(map[string]string, len(groupBy))
	for _, key := range groupBy {
		if v, ok := dpAttrs.Get(key); ok {
			dims[key] = v.AsString()
			continue
		}
		if v, ok := resAttrs.Get(key); ok {
			dims[key] = v.AsString()
		}
	}
	return dims
}

func hashDims(dims map[string]string) string {
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(dims[k])
		sb.WriteByte(';')
	}
	return sb.String()
}

func aggregate(points []float64, method string) float64 {
	if len(points) == 0 {
		return 0
	}
	switch method {
	case "max":
		m := points[0]
		for _, v := range points[1:] {
			if v > m {
				m = v
			}
		}
		return m
	case "min":
		m := points[0]
		for _, v := range points[1:] {
			if v < m {
				m = v
			}
		}
		return m
	case "sum":
		var s float64
		for _, v := range points {
			s += v
		}
		return s
	default: // avg
		var s float64
		for _, v := range points {
			s += v
		}
		return s / float64(len(points))
	}
}

func evalThreshold(value float64, t Threshold) bool {
	switch t.Operator {
	case ">":
		return value > t.Value
	case "<":
		return value < t.Value
	case ">=":
		return value >= t.Value
	case "<=":
		return value <= t.Value
	case "==":
		return value == t.Value
	}
	return false
}

func expandTemplate(tmpl string, dims map[string]string, value float64, windowSecs int) string {
	s := tmpl
	for k, v := range dims {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	s = strings.ReplaceAll(s, "{{value}}", fmt.Sprintf("%.0f", value))
	s = strings.ReplaceAll(s, "{{window}}", fmt.Sprintf("%d", windowSecs))
	return s
}
