// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
package dtk8stopology

import (
	"context"
	"testing"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
)

// captureConsumer collects all log batches passed to it.
type captureConsumer struct {
	received []plog.Logs
}

func (c *captureConsumer) ConsumeLogs(_ context.Context, ld plog.Logs) error {
	c.received = append(c.received, ld)
	return nil
}

func (c *captureConsumer) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (c *captureConsumer) allRecords() []plog.LogRecord {
	var out []plog.LogRecord
	for _, ld := range c.received {
		for i := 0; i < ld.ResourceLogs().Len(); i++ {
			rl := ld.ResourceLogs().At(i)
			for j := 0; j < rl.ScopeLogs().Len(); j++ {
				sl := rl.ScopeLogs().At(j)
				for k := 0; k < sl.LogRecords().Len(); k++ {
					out = append(out, sl.LogRecords().At(k))
				}
			}
		}
	}
	return out
}

func newTestProcessor(clusterName, clusterUID string) (*dtk8sTopologyProcessor, *captureConsumer) {
	cap := &captureConsumer{}
	return &dtk8sTopologyProcessor{
		config:       &Config{ClusterName: clusterName, ClusterUID: clusterUID},
		nextConsumer: cap,
		podCache:     make(map[string]podWorkload),
	}, cap
}

// makePodRecord builds a minimal K8S_POD topology log record with the given containers.
func makePodRecord(podName, ns, nodeName string, containers []string) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()

	lr.Attributes().PutStr("k8s.topo.node.type", "K8S_POD")
	lr.Attributes().PutStr("k8s.pod.name", podName)
	lr.Attributes().PutStr("k8s.pod.uid", "uid-"+podName)
	lr.Attributes().PutStr("k8s.namespace.name", ns)
	lr.Attributes().PutStr("k8s.node.name", nodeName)

	bodyMap := lr.Body().SetEmptyMap()
	spec := bodyMap.PutEmptyMap("spec")
	cList := spec.PutEmptySlice("containers")
	for _, name := range containers {
		c := cList.AppendEmpty().SetEmptyMap()
		c.PutStr("name", name)
		c.PutStr("image", name+":latest")
	}
	return ld
}

func TestClusterRecord(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	ld := plog.NewLogs() // no pods — just the cluster record
	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	records := cap.allRecords()
	// exactly one K8S_CLUSTER record per ConsumeLogs call
	if len(records) != 1 {
		t.Fatalf("want 1 record (cluster), got %d", len(records))
	}
	if v, _ := records[0].Attributes().Get("k8s.topo.node.type"); v.Str() != "K8S_CLUSTER" {
		t.Fatalf("want K8S_CLUSTER, got %q", v.Str())
	}
}

func TestPodFanOut(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	ld := makePodRecord("my-pod", "default", "node-1", []string{"app", "sidecar"})
	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	records := cap.allRecords()
	// 1 cluster record + 1 pod record (pass-through) + 2 container records
	if len(records) != 4 {
		t.Fatalf("want 4 records, got %d", len(records))
	}

	containerCount := 0
	for _, r := range records {
		if v, ok := r.Attributes().Get("k8s.topo.node.type"); ok && v.Str() == "CONTAINER" {
			containerCount++
		}
	}
	if containerCount != 2 {
		t.Fatalf("want 2 CONTAINER records, got %d", containerCount)
	}
}

func TestInitContainerFanOut(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Attributes().PutStr("k8s.topo.node.type", "K8S_POD")
	lr.Attributes().PutStr("k8s.pod.name", "my-pod")

	bodyMap := lr.Body().SetEmptyMap()
	spec := bodyMap.PutEmptyMap("spec")
	spec.PutEmptySlice("containers").AppendEmpty().SetEmptyMap().PutStr("name", "main")
	spec.PutEmptySlice("initContainers").AppendEmpty().SetEmptyMap().PutStr("name", "init")

	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	records := cap.allRecords()
	// 1 cluster + 1 pod + 1 container + 1 initContainer
	if len(records) != 4 {
		t.Fatalf("want 4 records, got %d", len(records))
	}
}

func TestImageEnrichment(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	ld := makePodRecord("my-pod", "default", "node-1", []string{"nginx"})
	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	for _, r := range cap.allRecords() {
		v, ok := r.Attributes().Get("k8s.topo.node.type")
		if !ok || v.Str() != "CONTAINER" {
			continue
		}
		img, ok := r.Attributes().Get("k8s.container.image")
		if !ok || img.Str() != "nginx:latest" {
			t.Fatalf("want k8s.container.image=nginx:latest, got %q", img.Str())
		}
	}
}

func TestClusterIdentityOnContainers(t *testing.T) {
	proc, cap := newTestProcessor("my-cluster", "cluster-uid-42")

	ld := makePodRecord("my-pod", "ns", "node", []string{"c1"})
	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	for _, r := range cap.allRecords() {
		v, ok := r.Attributes().Get("k8s.topo.node.type")
		if !ok || v.Str() != "CONTAINER" {
			continue
		}
		if cn, _ := r.Attributes().Get("k8s.cluster.name"); cn.Str() != "my-cluster" {
			t.Errorf("want cluster name my-cluster, got %q", cn.Str())
		}
		if cu, _ := r.Attributes().Get("k8s.cluster.uid"); cu.Str() != "cluster-uid-42" {
			t.Errorf("want cluster uid cluster-uid-42, got %q", cu.Str())
		}
	}
}

func TestNonPodPassthrough(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Attributes().PutStr("k8s.topo.node.type", "K8S_NODE")
	lr.Attributes().PutStr("k8s.node.name", "node-1")

	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	records := cap.allRecords()
	// 1 cluster record + 1 non-pod pass-through, no extra records emitted
	if len(records) != 2 {
		t.Fatalf("want 2 records (cluster + pass-through), got %d", len(records))
	}
	clusterSeen := false
	for _, r := range records {
		if v, _ := r.Attributes().Get("k8s.topo.node.type"); v.Str() == "K8S_CLUSTER" {
			clusterSeen = true
		}
	}
	if !clusterSeen {
		t.Fatal("cluster record missing")
	}
}

func TestMalformedBodyNoCrash(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	// K8S_POD record with a string body (not a map) — should not crash.
	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Attributes().PutStr("k8s.topo.node.type", "K8S_POD")
	lr.Body().SetStr("not a map")

	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	// 1 cluster + 1 pod pass-through; no containers (body not a map)
	if len(cap.allRecords()) != 2 {
		t.Fatalf("want 2 records (cluster + pod pass-through), got %d", len(cap.allRecords()))
	}
}

func TestWorkloadDimsOnContainers(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	ld := makePodRecord("my-pod", "default", "node-1", []string{"app"})
	ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).
		Attributes().PutStr("k8s.workload.name", "my-deploy")
	ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).
		Attributes().PutStr("k8s.workload.kind", "deployment")

	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	for _, r := range cap.allRecords() {
		if v, ok := r.Attributes().Get("k8s.topo.node.type"); !ok || v.Str() != "CONTAINER" {
			continue
		}
		if wn, ok := r.Attributes().Get("k8s.workload.name"); !ok || wn.Str() != "my-deploy" {
			t.Errorf("CONTAINER: want k8s.workload.name=my-deploy, got %q", wn.Str())
		}
		if wk, ok := r.Attributes().Get("k8s.workload.kind"); !ok || wk.Str() != "deployment" {
			t.Errorf("CONTAINER: want k8s.workload.kind=deployment, got %q", wk.Str())
		}
	}
}

func TestWorkloadCacheFallback(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	// First call: pod arrives with workload dims — cache gets populated.
	ld1 := makePodRecord("my-pod", "default", "node-1", []string{"app"})
	ld1.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).
		Attributes().PutStr("k8s.workload.name", "my-deploy")
	ld1.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).
		Attributes().PutStr("k8s.workload.kind", "deployment")
	if err := proc.ConsumeLogs(context.Background(), ld1); err != nil {
		t.Fatal(err)
	}
	cap.received = nil // discard first-call output

	// Second call: same pod, no workload dims (k8sattributes cache miss).
	// Expect cache to fill them in on both the pod and container records.
	ld2 := makePodRecord("my-pod", "default", "node-1", []string{"app"})
	if err := proc.ConsumeLogs(context.Background(), ld2); err != nil {
		t.Fatal(err)
	}

	for _, r := range cap.allRecords() {
		topoType, ok := r.Attributes().Get("k8s.topo.node.type")
		if !ok {
			continue
		}
		switch topoType.Str() {
		case "K8S_POD", "CONTAINER":
			wn, ok := r.Attributes().Get("k8s.workload.name")
			if !ok || wn.Str() != "my-deploy" {
				t.Errorf("%s: want k8s.workload.name=my-deploy from cache, got %q", topoType.Str(), wn.Str())
			}
			wk, ok := r.Attributes().Get("k8s.workload.kind")
			if !ok || wk.Str() != "deployment" {
				t.Errorf("%s: want k8s.workload.kind=deployment from cache, got %q", topoType.Str(), wk.Str())
			}
		}
	}
}

func TestNoSpecNoCrash(t *testing.T) {
	proc, cap := newTestProcessor("test-cluster", "uid-1")

	// K8S_POD record with a map body but no "spec" key.
	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Attributes().PutStr("k8s.topo.node.type", "K8S_POD")
	lr.Body().SetEmptyMap().PutStr("kind", "Pod")

	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	// 1 cluster + 1 pod pass-through; no containers (no spec key)
	if len(cap.allRecords()) != 2 {
		t.Fatalf("want 2 records (cluster + pod pass-through), got %d", len(cap.allRecords()))
	}
}
