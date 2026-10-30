// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
// Package dtk8stopology emits one K8S_CLUSTER topology record per pull cycle
// and fans out pod topology records into per-container KUBERNETES_OTEL_TOPO
// log records enriched with k8s.container.image.
//
// Input: KUBERNETES_OTEL_TOPO records from the logs/topology pipeline after
// transform/topology + groupbyattrs/pod + k8sattributes + transform/workload_dims.
// Body still carries the raw K8s object (pull mode). Records with
// attributes["k8s.topo.node.type"] == "K8S_POD" trigger container fan-out;
// all other records are passed through unchanged.
//
// Output: one K8S_CLUSTER record per ConsumeLogs call + all input records +
// one CONTAINER record per containers[]/initContainers[] element.
//
// Workload-dims cache: k8sattributes can miss a pod at startup before its
// informer cache is populated. patchWorkloadDims caches the last-seen
// workload name/kind per pod UID and fills them in when the attribute is
// absent, so entities stay visible across pull cycles even during warm-up.
package dtk8stopology

import (
	"context"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

const (
	eventProvider = "KUBERNETES_OTEL_TOPO"
	eventKindNode = "topology_node"
)

type podWorkload struct {
	name string
	kind string
}

type dtk8sTopologyProcessor struct {
	config       *Config
	nextConsumer consumer.Logs
	cacheMu      sync.Mutex
	podCache     map[string]podWorkload // key: pod UID
}

func (p *dtk8sTopologyProcessor) Start(_ context.Context, _ component.Host) error { return nil }
func (p *dtk8sTopologyProcessor) Shutdown(_ context.Context) error                { return nil }
func (p *dtk8sTopologyProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// ConsumeLogs emits one K8S_CLUSTER record per call, passes all input records
// through (patching missing workload dims on K8S_POD records from the cache),
// and appends one CONTAINER record per containers[]/initContainers[] entry.
func (p *dtk8sTopologyProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	out := plog.NewLogs()
	p.emitCluster(out)

	// Copy record-by-record so K8S_POD records can be patched with cached
	// workload dims before passing through and before container fan-out.
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		srcRL := ld.ResourceLogs().At(i)
		dstRL := out.ResourceLogs().AppendEmpty()
		srcRL.Resource().CopyTo(dstRL.Resource())

		for j := 0; j < srcRL.ScopeLogs().Len(); j++ {
			srcSL := srcRL.ScopeLogs().At(j)
			dstSL := dstRL.ScopeLogs().AppendEmpty()
			srcSL.Scope().CopyTo(dstSL.Scope())

			for k := 0; k < srcSL.LogRecords().Len(); k++ {
				dstLR := dstSL.LogRecords().AppendEmpty()
				srcSL.LogRecords().At(k).CopyTo(dstLR)

				if topoType, ok := dstLR.Attributes().Get("k8s.topo.node.type"); ok && topoType.Str() == "K8S_POD" {
					p.patchWorkloadDims(dstLR)
					p.fanOutContainers(dstRL, dstLR, out)
				}
			}
		}
	}

	return p.nextConsumer.ConsumeLogs(ctx, out)
}

// patchWorkloadDims updates the cache when workload dims are present, or fills
// them from the cache when k8sattributes missed the pod UID lookup.
func (p *dtk8sTopologyProcessor) patchWorkloadDims(lr plog.LogRecord) {
	podUID := attrStr(lr, "k8s.pod.uid")
	if podUID == "" {
		return
	}
	workloadName := attrStr(lr, "k8s.workload.name")
	workloadKind := attrStr(lr, "k8s.workload.kind")

	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	if workloadName != "" {
		p.podCache[podUID] = podWorkload{name: workloadName, kind: workloadKind}
		return
	}
	if cached, ok := p.podCache[podUID]; ok && cached.name != "" {
		lr.Attributes().PutStr("k8s.workload.name", cached.name)
		if cached.kind != "" {
			lr.Attributes().PutStr("k8s.workload.kind", cached.kind)
		}
	}
}

// emitCluster appends one K8S_CLUSTER topology record per pull cycle.
// No K8s API resource exists for the cluster itself; this record is
// synthesised from the processor config.
func (p *dtk8sTopologyProcessor) emitCluster(out plog.Logs) {
	if p.config.ClusterUID == "" {
		return
	}
	rl := out.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("k8s.cluster.name", p.config.ClusterName)
	rl.Resource().Attributes().PutStr("k8s.cluster.uid", p.config.ClusterUID)
	rec := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	rec.Attributes().PutStr("event.provider", eventProvider)
	rec.Attributes().PutStr("event.kind", eventKindNode)
	rec.Attributes().PutStr("k8s.topo.node.type", "K8S_CLUSTER")
	rec.Attributes().PutStr("k8s.cluster.name", p.config.ClusterName)
	rec.Attributes().PutStr("k8s.cluster.uid", p.config.ClusterUID)
}

func (p *dtk8sTopologyProcessor) fanOutContainers(rl plog.ResourceLogs, lr plog.LogRecord, out plog.Logs) {
	topoType, ok := lr.Attributes().Get("k8s.topo.node.type")
	if !ok || topoType.Str() != "K8S_POD" {
		return
	}

	podName := attrStr(lr, "k8s.pod.name")
	podUID := attrStr(lr, "k8s.pod.uid")
	ns := attrStr(lr, "k8s.namespace.name")
	nodeName := attrStr(lr, "k8s.node.name")
	workloadName := attrStr(lr, "k8s.workload.name")
	workloadKind := attrStr(lr, "k8s.workload.kind")

	for _, specKey := range []string{"containers", "initContainers"} {
		p.emitContainerList(rl, lr.Body(), specKey, podName, podUID, ns, nodeName, workloadName, workloadKind, out)
	}
}

func (p *dtk8sTopologyProcessor) emitContainerList(
	srcRL plog.ResourceLogs,
	body pcommon.Value,
	specKey, podName, podUID, ns, nodeName, workloadName, workloadKind string,
	out plog.Logs,
) {
	if body.Type() != pcommon.ValueTypeMap {
		return
	}
	specVal, ok := body.Map().Get("spec")
	if !ok || specVal.Type() != pcommon.ValueTypeMap {
		return
	}
	listVal, ok := specVal.Map().Get(specKey)
	if !ok || listVal.Type() != pcommon.ValueTypeSlice {
		return
	}

	for i := 0; i < listVal.Slice().Len(); i++ {
		item := listVal.Slice().At(i)
		if item.Type() != pcommon.ValueTypeMap {
			continue
		}
		cm := item.Map()
		nameVal, ok := cm.Get("name")
		if !ok || nameVal.Str() == "" {
			continue
		}

		rl := out.ResourceLogs().AppendEmpty()
		srcRL.Resource().CopyTo(rl.Resource())
		rec := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()

		rec.Attributes().PutStr("event.provider", eventProvider)
		rec.Attributes().PutStr("event.kind", eventKindNode)
		rec.Attributes().PutStr("k8s.topo.node.type", "CONTAINER")
		rec.Attributes().PutStr("k8s.cluster.name", p.config.ClusterName)
		rec.Attributes().PutStr("k8s.cluster.uid", p.config.ClusterUID)
		rec.Attributes().PutStr("k8s.container.name", nameVal.Str())
		if podName != "" {
			rec.Attributes().PutStr("k8s.pod.name", podName)
		}
		if podUID != "" {
			rec.Attributes().PutStr("k8s.pod.uid", podUID)
		}
		if ns != "" {
			rec.Attributes().PutStr("k8s.namespace.name", ns)
		}
		if nodeName != "" {
			rec.Attributes().PutStr("k8s.node.name", nodeName)
		}
		if workloadName != "" {
			rec.Attributes().PutStr("k8s.workload.name", workloadName)
		}
		if workloadKind != "" {
			rec.Attributes().PutStr("k8s.workload.kind", workloadKind)
		}
		if imageVal, ok := cm.Get("image"); ok && imageVal.Str() != "" {
			rec.Attributes().PutStr("k8s.container.image", imageVal.Str())
		}
	}
}

func attrStr(lr plog.LogRecord, key string) string {
	if v, ok := lr.Attributes().Get(key); ok {
		return v.Str()
	}
	return ""
}
