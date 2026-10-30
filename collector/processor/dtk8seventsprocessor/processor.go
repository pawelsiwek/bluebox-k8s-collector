// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
// Package dtk8seventsprocessor applies per-entity and per-cluster hourly throttle
// to KUBERNETES_EVENT log records (pre-mapped by transform/k8sevents OTTL) and
// fans out OOM-kill Pod MODIFIED watch events into KUBERNETES_INFERRED_EVENT records.
//
// Input (logs/events pipeline, after transform/k8sevents):
//   - K8s Event watch records: event.provider == "KUBERNETES_EVENT" (set by OTTL).
//     Davis field mapping (event.kind, dt.kubernetes.event.*, event.category, etc.)
//     is done upstream; this processor only applies the throttle and passes records through.
//   - K8s Pod MODIFIED watch records: k8s.resource.name == "pods" with OOM detection.
//
// Output: throttle-passing KUBERNETES_EVENT records + KUBERNETES_INFERRED_EVENT per OOMKilled container.
package dtk8seventsprocessor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

const (
	providerK8sInferredEvent = "KUBERNETES_INFERRED_EVENT"
	eventKindDavis           = "DAVIS_EVENT"
	eventStatusClosed        = "CLOSED"
)

type throttleBucket struct {
	hour  int64
	count int
}

type dtk8sEventsProcessor struct {
	config          *Config
	nextConsumer    consumer.Logs
	throttleMu      sync.Mutex
	entityThrottle  map[string]*throttleBucket
	clusterThrottle *throttleBucket
	lastSweepHour   int64
}

func newProcessor(cfg *Config, next consumer.Logs) *dtk8sEventsProcessor {
	return &dtk8sEventsProcessor{
		config:         cfg,
		nextConsumer:   next,
		entityThrottle: make(map[string]*throttleBucket),
	}
}

func (p *dtk8sEventsProcessor) Start(_ context.Context, _ component.Host) error { return nil }
func (p *dtk8sEventsProcessor) Shutdown(_ context.Context) error                { return nil }
func (p *dtk8sEventsProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// ConsumeLogs passes KUBERNETES_EVENT records that clear the throttle and
// emits KUBERNETES_INFERRED_EVENT records for OOMKilled containers.
func (p *dtk8sEventsProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	out := plog.NewLogs()

	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		rl := ld.ResourceLogs().At(i)
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			for k := 0; k < sl.LogRecords().Len(); k++ {
				p.processRecord(rl, sl.LogRecords().At(k), out)
			}
		}
	}

	if out.ResourceLogs().Len() == 0 {
		return nil
	}
	return p.nextConsumer.ConsumeLogs(ctx, out)
}

func (p *dtk8sEventsProcessor) processRecord(srcRL plog.ResourceLogs, lr plog.LogRecord, out plog.Logs) {
	// K8s Event records already mapped by transform/k8sevents OTTL.
	if v, ok := lr.Attributes().Get("event.provider"); ok && v.Str() == "KUBERNETES_EVENT" {
		p.processK8sEvent(srcRL, lr, out)
		return
	}
	// Pod watch records for OOM detection.
	if v, ok := lr.Attributes().Get("k8s.resource.name"); ok && v.Str() == "pods" {
		p.processPodForOOM(srcRL, lr, out)
	}
}

// processK8sEvent applies throttle and copies the record through if allowed.
// The K8s Event → Davis field mapping was performed upstream by transform/k8sevents OTTL.
func (p *dtk8sEventsProcessor) processK8sEvent(srcRL plog.ResourceLogs, lr plog.LogRecord, out plog.Logs) {
	ns := attrStr(lr, "k8s.namespace.name")
	involvedName := attrStr(lr, "dt.kubernetes.event.involved_object.name")
	reason := attrStr(lr, "dt.kubernetes.event.reason")

	entitySig := fmt.Sprintf("%s\x00%s\x00%s\x00%s", p.config.ClusterUID, ns, involvedName, reason)
	if !p.allowEvent(entitySig) {
		return
	}

	rl := out.ResourceLogs().AppendEmpty()
	srcRL.Resource().CopyTo(rl.Resource())
	newRec := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.CopyTo(newRec)
}

// processPodForOOM checks Pod MODIFIED watch events for OOMKilled containers
// and emits a KUBERNETES_INFERRED_EVENT for each.
func (p *dtk8sEventsProcessor) processPodForOOM(srcRL plog.ResourceLogs, lr plog.LogRecord, out plog.Logs) {
	if bodyMapStr(lr.Body(), "type") != "MODIFIED" {
		return
	}

	body := lr.Body()
	if body.Type() != pcommon.ValueTypeMap {
		return
	}

	var objMap pcommon.Map
	if objVal, ok := body.Map().Get("object"); ok && objVal.Type() == pcommon.ValueTypeMap {
		objMap = objVal.Map()
	} else {
		objMap = body.Map()
	}

	statusVal, ok := objMap.Get("status")
	if !ok || statusVal.Type() != pcommon.ValueTypeMap {
		return
	}
	csListVal, ok := statusVal.Map().Get("containerStatuses")
	if !ok || csListVal.Type() != pcommon.ValueTypeSlice {
		return
	}

	podName := bodyMapStr(body, "object", "metadata", "name")
	if podName == "" {
		podName = bodyMapStr(body, "metadata", "name")
	}
	namespace := bodyMapStr(body, "object", "metadata", "namespace")
	if namespace == "" {
		namespace = bodyMapStr(body, "metadata", "namespace")
	}

	csList := csListVal.Slice()
	for i := 0; i < csList.Len(); i++ {
		csItem := csList.At(i)
		if csItem.Type() != pcommon.ValueTypeMap {
			continue
		}
		csMap := csItem.Map()

		lastStateVal, ok := csMap.Get("lastState")
		if !ok || lastStateVal.Type() != pcommon.ValueTypeMap {
			continue
		}
		termVal, ok := lastStateVal.Map().Get("terminated")
		if !ok || termVal.Type() != pcommon.ValueTypeMap {
			continue
		}
		reasonVal, ok := termVal.Map().Get("reason")
		if !ok || reasonVal.Str() != "OOMKilled" {
			continue
		}

		containerName := ""
		if nameVal, ok := csMap.Get("name"); ok {
			containerName = nameVal.Str()
		}

		rl := out.ResourceLogs().AppendEmpty()
		srcRL.Resource().CopyTo(rl.Resource())
		rl.Resource().Attributes().PutStr("k8s.cluster.name", p.config.ClusterName)
		rl.Resource().Attributes().PutStr("k8s.cluster.uid", p.config.ClusterUID)
		if namespace != "" {
			rl.Resource().Attributes().PutStr("k8s.namespace.name", namespace)
		}
		if podName != "" {
			rl.Resource().Attributes().PutStr("k8s.pod.name", podName)
		}

		rec := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		rec.Attributes().PutStr("event.provider", providerK8sInferredEvent)
		rec.Attributes().PutStr("event.group_label", "OOM")
		rec.Attributes().PutStr("event.name", "Container OOM killed")
		rec.Attributes().PutStr("event.kind", eventKindDavis)
		rec.Attributes().PutStr("event.status", eventStatusClosed)
		rec.Attributes().PutBool("dt.davis.is_rootcause_relevant", true)

		if containerName != "" {
			rec.Attributes().PutStr("k8s.container.name", containerName)
		}
		if podName != "" {
			rec.Attributes().PutStr("k8s.pod.name", podName)
		}
		if namespace != "" {
			rec.Attributes().PutStr("k8s.namespace.name", namespace)
		}
	}
}

// allowEvent returns true when this (entity, hour) tuple has not yet hit
// the per-entity or per-cluster throttle ceiling.
func (p *dtk8sEventsProcessor) allowEvent(entitySig string) bool {
	hourBucket := time.Now().Unix() / 3600

	p.throttleMu.Lock()
	defer p.throttleMu.Unlock()

	if p.clusterThrottle == nil || p.clusterThrottle.hour != hourBucket {
		p.clusterThrottle = &throttleBucket{hour: hourBucket, count: 0}
	}
	if p.clusterThrottle.count >= p.config.MaxEventsPerClusterPerHour {
		return false
	}

	// Once per hour: remove stale entries for entities not seen in the current hour.
	// This bounds map size in dynamic clusters with many ephemeral pod names.
	if p.lastSweepHour != hourBucket {
		for k, b := range p.entityThrottle {
			if b.hour != hourBucket {
				delete(p.entityThrottle, k)
			}
		}
		p.lastSweepHour = hourBucket
	}

	bucket, ok := p.entityThrottle[entitySig]
	if !ok || bucket.hour != hourBucket {
		p.entityThrottle[entitySig] = &throttleBucket{hour: hourBucket, count: 1}
		p.clusterThrottle.count++
		return true
	}
	if bucket.count >= p.config.MaxEventsPerEntityPerHour {
		return false
	}
	bucket.count++
	p.clusterThrottle.count++
	return true
}

func attrStr(lr plog.LogRecord, key string) string {
	if v, ok := lr.Attributes().Get(key); ok {
		return v.Str()
	}
	return ""
}

func bodyMapStr(body pcommon.Value, path ...string) string {
	if body.Type() != pcommon.ValueTypeMap {
		return ""
	}
	cur := body.Map()
	for i, key := range path {
		v, ok := cur.Get(key)
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			if v.Type() == pcommon.ValueTypeStr {
				return v.Str()
			}
			return ""
		}
		if v.Type() != pcommon.ValueTypeMap {
			return ""
		}
		cur = v.Map()
	}
	return ""
}
