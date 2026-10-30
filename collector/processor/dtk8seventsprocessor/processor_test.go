// Vendored from trauter/dt-otel-k8s-platform-monitoring@72bed8b (Apache-2.0); see NOTICE.
package dtk8seventsprocessor

import (
	"context"
	"testing"
	"time"

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

func newTestProcessor(maxEntity, maxCluster int) (*dtk8sEventsProcessor, *captureConsumer) {
	cap := &captureConsumer{}
	proc := newProcessor(&Config{
		ClusterName:                "test-cluster",
		ClusterUID:                 "uid-test",
		MaxEventsPerEntityPerHour:  maxEntity,
		MaxEventsPerClusterPerHour: maxCluster,
	}, cap)
	return proc, cap
}

// makeK8sEventRecord returns a Logs with one KUBERNETES_EVENT record.
func makeK8sEventRecord(ns, involvedObj, reason string) plog.Logs {
	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Attributes().PutStr("event.provider", "KUBERNETES_EVENT")
	lr.Attributes().PutStr("k8s.namespace.name", ns)
	lr.Attributes().PutStr("dt.kubernetes.event.involved_object.name", involvedObj)
	lr.Attributes().PutStr("dt.kubernetes.event.reason", reason)
	return ld
}

// makePodModifiedRecord returns a Logs with one Pod MODIFIED record.
// containerOOM controls whether a container's lastState shows OOMKilled.
func makePodModifiedRecord(podName, ns, containerName string, containerOOM bool) plog.Logs {
	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Attributes().PutStr("k8s.resource.name", "pods")

	body := lr.Body().SetEmptyMap()
	body.PutStr("type", "MODIFIED")
	obj := body.PutEmptyMap("object")
	meta := obj.PutEmptyMap("metadata")
	meta.PutStr("name", podName)
	meta.PutStr("namespace", ns)

	status := obj.PutEmptyMap("status")
	csList := status.PutEmptySlice("containerStatuses")
	cs := csList.AppendEmpty().SetEmptyMap()
	cs.PutStr("name", containerName)
	if containerOOM {
		lastState := cs.PutEmptyMap("lastState")
		terminated := lastState.PutEmptyMap("terminated")
		terminated.PutStr("reason", "OOMKilled")
	}
	return ld
}

func TestThrottleAllowsUpToLimit(t *testing.T) {
	proc, cap := newTestProcessor(3, 1000)

	for i := 0; i < 3; i++ {
		ld := makeK8sEventRecord("default", "my-pod", "BackOff")
		if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
			t.Fatal(err)
		}
	}

	if len(cap.allRecords()) != 3 {
		t.Fatalf("want 3 records allowed, got %d", len(cap.allRecords()))
	}
}

func TestThrottleDropsBeyondEntityLimit(t *testing.T) {
	proc, cap := newTestProcessor(3, 1000)

	for i := 0; i < 5; i++ {
		ld := makeK8sEventRecord("default", "my-pod", "BackOff")
		if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
			t.Fatal(err)
		}
	}

	if len(cap.allRecords()) != 3 {
		t.Fatalf("want 3 records (entity limit), got %d", len(cap.allRecords()))
	}
}

func TestThrottleClusterCeiling(t *testing.T) {
	// Cluster limit 2, entity limit 10 — cluster ceiling should stop after 2.
	proc, cap := newTestProcessor(10, 2)

	ld1 := makeK8sEventRecord("ns1", "pod-a", "Reason1")
	ld2 := makeK8sEventRecord("ns2", "pod-b", "Reason2")
	ld3 := makeK8sEventRecord("ns3", "pod-c", "Reason3")
	for _, ld := range []plog.Logs{ld1, ld2, ld3} {
		if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
			t.Fatal(err)
		}
	}

	if len(cap.allRecords()) != 2 {
		t.Fatalf("want 2 records (cluster ceiling), got %d", len(cap.allRecords()))
	}
}

func TestThrottleHourRollover(t *testing.T) {
	proc, cap := newTestProcessor(1, 1000)

	// Exhaust the entity limit for the current hour.
	ld := makeK8sEventRecord("default", "my-pod", "BackOff")
	_ = proc.ConsumeLogs(context.Background(), ld)
	_ = proc.ConsumeLogs(context.Background(), ld) // this one should be dropped

	if len(cap.allRecords()) != 1 {
		t.Fatalf("setup: want 1 record before rollover, got %d", len(cap.allRecords()))
	}

	// Simulate hour rollover by injecting a stale bucket (hour = -1).
	proc.throttleMu.Lock()
	entitySig := "uid-test\x00default\x00my-pod\x00BackOff"
	proc.entityThrottle[entitySig] = &throttleBucket{hour: -1, count: 999}
	proc.clusterThrottle = &throttleBucket{hour: -1, count: 999}
	proc.throttleMu.Unlock()

	_ = proc.ConsumeLogs(context.Background(), ld)

	if len(cap.allRecords()) != 2 {
		t.Fatalf("want 2 records after hour rollover, got %d", len(cap.allRecords()))
	}
}

func TestThrottleDifferentEntitiesAreIndependent(t *testing.T) {
	// Entity limit 1, cluster limit large — two different entities each get 1 event.
	proc, cap := newTestProcessor(1, 1000)

	ld1 := makeK8sEventRecord("ns1", "pod-a", "Reason")
	ld2 := makeK8sEventRecord("ns1", "pod-b", "Reason")
	ld1extra := makeK8sEventRecord("ns1", "pod-a", "Reason") // should be dropped
	ld2extra := makeK8sEventRecord("ns1", "pod-b", "Reason") // should be dropped

	for _, ld := range []plog.Logs{ld1, ld2, ld1extra, ld2extra} {
		if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
			t.Fatal(err)
		}
	}

	if len(cap.allRecords()) != 2 {
		t.Fatalf("want 2 records (one per entity), got %d", len(cap.allRecords()))
	}
}

func TestOOMDetection(t *testing.T) {
	proc, cap := newTestProcessor(1000, 10000)

	ld := makePodModifiedRecord("crash-pod", "default", "app", true)
	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	records := cap.allRecords()
	if len(records) != 1 {
		t.Fatalf("want 1 KUBERNETES_INFERRED_EVENT, got %d", len(records))
	}

	r := records[0]
	if v, _ := r.Attributes().Get("event.provider"); v.Str() != providerK8sInferredEvent {
		t.Errorf("want event.provider=%s, got %q", providerK8sInferredEvent, v.Str())
	}
	if v, _ := r.Attributes().Get("k8s.container.name"); v.Str() != "app" {
		t.Errorf("want k8s.container.name=app, got %q", v.Str())
	}
	if v, _ := r.Attributes().Get("k8s.pod.name"); v.Str() != "crash-pod" {
		t.Errorf("want k8s.pod.name=crash-pod, got %q", v.Str())
	}
	if v, _ := r.Attributes().Get("k8s.namespace.name"); v.Str() != "default" {
		t.Errorf("want k8s.namespace.name=default, got %q", v.Str())
	}
	if v, _ := r.Attributes().Get("event.status"); v.Str() != eventStatusClosed {
		t.Errorf("want event.status=%s, got %q", eventStatusClosed, v.Str())
	}
	if v, _ := r.Attributes().Get("dt.davis.is_rootcause_relevant"); !v.Bool() {
		t.Errorf("want dt.davis.is_rootcause_relevant=true")
	}
}

func TestOOMNoEmitWhenNotOOMKilled(t *testing.T) {
	proc, cap := newTestProcessor(1000, 10000)

	ld := makePodModifiedRecord("healthy-pod", "default", "app", false)
	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	if len(cap.allRecords()) != 0 {
		t.Fatalf("want 0 records for non-OOM pod, got %d", len(cap.allRecords()))
	}
}

func TestOOMIgnoresNonModifiedType(t *testing.T) {
	proc, cap := newTestProcessor(1000, 10000)

	ld := makePodModifiedRecord("crash-pod", "default", "app", true)
	// Override the event type to ADDED
	rl := ld.ResourceLogs().At(0)
	lr := rl.ScopeLogs().At(0).LogRecords().At(0)
	lr.Body().Map().PutStr("type", "ADDED")

	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	if len(cap.allRecords()) != 0 {
		t.Fatalf("want 0 records for non-MODIFIED pod event, got %d", len(cap.allRecords()))
	}
}

func TestUnknownRecordDropped(t *testing.T) {
	proc, cap := newTestProcessor(1000, 10000)

	// A record that is neither a KUBERNETES_EVENT nor a pods watch record.
	ld := plog.NewLogs()
	lr := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Attributes().PutStr("event.provider", "SOMETHING_ELSE")

	if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}

	if len(cap.allRecords()) != 0 {
		t.Fatalf("want 0 records for unknown provider, got %d", len(cap.allRecords()))
	}
}

func TestProcessorStartShutdown(t *testing.T) {
	proc, _ := newTestProcessor(1000, 10000)
	ctx := context.Background()
	if err := proc.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := proc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestThrottleCounterIncrements verifies the cluster counter increments with each allowed event.
func TestThrottleCounterIncrements(t *testing.T) {
	proc, _ := newTestProcessor(1000, 1000)
	hourBucket := time.Now().Unix() / 3600

	for i := 0; i < 5; i++ {
		ld := makeK8sEventRecord("default", "pod-"+string(rune('a'+i)), "Reason")
		if err := proc.ConsumeLogs(context.Background(), ld); err != nil {
			t.Fatal(err)
		}
	}

	proc.throttleMu.Lock()
	defer proc.throttleMu.Unlock()

	if proc.clusterThrottle == nil {
		t.Fatal("clusterThrottle should not be nil after processing events")
	}
	if proc.clusterThrottle.hour != hourBucket {
		t.Errorf("clusterThrottle.hour mismatch: want %d, got %d", hourBucket, proc.clusterThrottle.hour)
	}
	if proc.clusterThrottle.count != 5 {
		t.Errorf("want clusterThrottle.count=5, got %d", proc.clusterThrottle.count)
	}
}
