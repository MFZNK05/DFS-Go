package metrics_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Observability/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// newReg creates a fresh Prometheus registry and calls metrics.Init on it.
// metrics.Reset() ensures the sync.Once guard is cleared between tests.
func newReg(t *testing.T) *prometheus.Registry {
	t.Helper()
	metrics.Reset()
	reg := prometheus.NewRegistry()
	metrics.Init(reg)
	return reg
}

// gatherCounter finds the counter value for the given metric name and label pair.
func gatherCounter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mf := gatherFamily(t, reg, name)
	for _, m := range mf.GetMetric() {
		if labelsMatch(m.GetLabel(), labels) {
			return m.GetCounter().GetValue()
		}
	}
	t.Fatalf("no metric %s with labels %v found", name, labels)
	return 0
}

// gatherGauge reads a gauge value by metric name.
func gatherGauge(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mf := gatherFamily(t, reg, name)
	if len(mf.GetMetric()) == 0 {
		t.Fatalf("no metric %s found", name)
	}
	return mf.GetMetric()[0].GetGauge().GetValue()
}

// gatherSampleCount returns the histogram sample count for a given label set.
func gatherSampleCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	mf := gatherFamily(t, reg, name)
	for _, m := range mf.GetMetric() {
		if labelsMatch(m.GetLabel(), labels) {
			return m.GetHistogram().GetSampleCount()
		}
	}
	t.Fatalf("no histogram %s with labels %v found", name, labels)
	return 0
}

func gatherFamily(t *testing.T, reg *prometheus.Registry, name string) *dto.MetricFamily {
	t.Helper()
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	for _, mf := range gathered {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("metric family %q not found in registry", name)
	return nil
}

func labelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	got := make(map[string]string, len(pairs))
	for _, p := range pairs {
		got[p.GetName()] = p.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// RecordStore
// ---------------------------------------------------------------------------

func TestRecordStoreOkIncrements(t *testing.T) {
	reg := newReg(t)
	// Read baseline then record one more; expect delta=1.
	before := gatherCounter(t, reg, "dfs_store_ops_total", map[string]string{"operation": "store", "status": "ok"})
	metrics.RecordStore("store", nil, 5*time.Millisecond)

	v := gatherCounter(t, reg, "dfs_store_ops_total", map[string]string{"operation": "store", "status": "ok"})
	if v-before != 1 {
		t.Errorf("expected delta=1, got %v", v-before)
	}
}

func TestRecordStoreErrLabel(t *testing.T) {
	reg := newReg(t)
	before := gatherCounter(t, reg, "dfs_store_ops_total", map[string]string{"operation": "replicate", "status": "err"})
	metrics.RecordStore("replicate", errors.New("io error"), 2*time.Millisecond)

	v := gatherCounter(t, reg, "dfs_store_ops_total", map[string]string{"operation": "replicate", "status": "err"})
	if v-before != 1 {
		t.Errorf("expected err counter delta=1, got %v", v-before)
	}
}

func TestRecordStoreDurationObserved(t *testing.T) {
	reg := newReg(t)
	before := gatherSampleCount(t, reg, "dfs_store_duration_seconds", map[string]string{"operation": "store"})
	metrics.RecordStore("store", nil, 10*time.Millisecond)

	n := gatherSampleCount(t, reg, "dfs_store_duration_seconds", map[string]string{"operation": "store"})
	if n-before != 1 {
		t.Errorf("expected histogram delta=1, got %d", n-before)
	}
}

// ---------------------------------------------------------------------------
// RecordGet
// ---------------------------------------------------------------------------

func TestRecordGetLocalOk(t *testing.T) {
	reg := newReg(t)
	before := gatherCounter(t, reg, "dfs_get_ops_total", map[string]string{"source": "local", "status": "ok"})
	metrics.RecordGet("local", nil, time.Millisecond)

	v := gatherCounter(t, reg, "dfs_get_ops_total", map[string]string{"source": "local", "status": "ok"})
	if v-before != 1 {
		t.Errorf("expected delta=1, got %v", v-before)
	}
}

func TestRecordGetMiss(t *testing.T) {
	reg := newReg(t)
	before := gatherCounter(t, reg, "dfs_get_ops_total", map[string]string{"source": "miss", "status": "err"})
	metrics.RecordGet("miss", errors.New("not found"), time.Millisecond)

	v := gatherCounter(t, reg, "dfs_get_ops_total", map[string]string{"source": "miss", "status": "err"})
	if v-before != 1 {
		t.Errorf("expected miss/err counter delta=1, got %v", v-before)
	}
}

// ---------------------------------------------------------------------------
// RecordReplication
// ---------------------------------------------------------------------------

func TestRecordReplicationOk(t *testing.T) {
	reg := newReg(t)
	before := gatherCounter(t, reg, "dfs_replication_total", map[string]string{"status": "ok"})
	metrics.RecordReplication("ok")
	metrics.RecordReplication("ok")

	v := gatherCounter(t, reg, "dfs_replication_total", map[string]string{"status": "ok"})
	if v-before != 2 {
		t.Errorf("expected delta=2, got %v", v-before)
	}
}

func TestRecordReplicationHint(t *testing.T) {
	reg := newReg(t)
	before := gatherCounter(t, reg, "dfs_replication_total", map[string]string{"status": "hint"})
	metrics.RecordReplication("hint")

	v := gatherCounter(t, reg, "dfs_replication_total", map[string]string{"status": "hint"})
	if v-before != 1 {
		t.Errorf("expected delta=1, got %v", v-before)
	}
}

// ---------------------------------------------------------------------------
// RecordGossipRound
// ---------------------------------------------------------------------------

func TestRecordGossipRoundIncrementsCounter(t *testing.T) {
	reg := newReg(t)
	mf := gatherFamily(t, reg, "dfs_gossip_rounds_total")
	before := mf.GetMetric()[0].GetCounter().GetValue()

	metrics.RecordGossipRound(50 * time.Millisecond)
	metrics.RecordGossipRound(30 * time.Millisecond)

	mf = gatherFamily(t, reg, "dfs_gossip_rounds_total")
	v := mf.GetMetric()[0].GetCounter().GetValue()
	if v-before != 2 {
		t.Errorf("expected delta=2 gossip rounds, got %v", v-before)
	}
}

func TestRecordGossipRoundObservesDuration(t *testing.T) {
	reg := newReg(t)
	mf := gatherFamily(t, reg, "dfs_gossip_duration_seconds")
	before := mf.GetMetric()[0].GetHistogram().GetSampleCount()

	metrics.RecordGossipRound(100 * time.Millisecond)

	mf = gatherFamily(t, reg, "dfs_gossip_duration_seconds")
	n := mf.GetMetric()[0].GetHistogram().GetSampleCount()
	if n-before != 1 {
		t.Errorf("expected delta=1 sample, got %d", n-before)
	}
}

// ---------------------------------------------------------------------------
// RecordHeartbeat
// ---------------------------------------------------------------------------

func TestRecordHeartbeatSent(t *testing.T) {
	reg := newReg(t)
	beforeSent := gatherCounter(t, reg, "dfs_heartbeats_total", map[string]string{"direction": "sent"})
	beforeRecv := gatherCounter(t, reg, "dfs_heartbeats_total", map[string]string{"direction": "received"})

	metrics.RecordHeartbeat("sent")
	metrics.RecordHeartbeat("sent")
	metrics.RecordHeartbeat("received")

	sent := gatherCounter(t, reg, "dfs_heartbeats_total", map[string]string{"direction": "sent"})
	recv := gatherCounter(t, reg, "dfs_heartbeats_total", map[string]string{"direction": "received"})
	if sent-beforeSent != 2 || recv-beforeRecv != 1 {
		t.Errorf("expected sent delta=2 recv delta=1, got sent=%v recv=%v", sent-beforeSent, recv-beforeRecv)
	}
}

// ---------------------------------------------------------------------------
// RecordQuorum
// ---------------------------------------------------------------------------

func TestRecordQuorumRead(t *testing.T) {
	reg := newReg(t)
	before := gatherSampleCount(t, reg, "dfs_quorum_duration_seconds", map[string]string{"type": "read"})
	metrics.RecordQuorum("read", 3*time.Millisecond)

	n := gatherSampleCount(t, reg, "dfs_quorum_duration_seconds", map[string]string{"type": "read"})
	if n-before != 1 {
		t.Errorf("expected delta=1 quorum read sample, got %d", n-before)
	}
}

func TestRecordQuorumWrite(t *testing.T) {
	reg := newReg(t)
	before := gatherSampleCount(t, reg, "dfs_quorum_duration_seconds", map[string]string{"type": "write"})
	metrics.RecordQuorum("write", 5*time.Millisecond)

	n := gatherSampleCount(t, reg, "dfs_quorum_duration_seconds", map[string]string{"type": "write"})
	if n-before != 1 {
		t.Errorf("expected delta=1 quorum write sample, got %d", n-before)
	}
}

// ---------------------------------------------------------------------------
// Gauges
// ---------------------------------------------------------------------------

func TestSetPeerCount(t *testing.T) {
	reg := newReg(t)
	metrics.SetPeerCount(5)

	v := gatherGauge(t, reg, "dfs_peer_count")
	if v != 5 {
		t.Errorf("expected peer_count=5, got %v", v)
	}
}

func TestSetRingSize(t *testing.T) {
	reg := newReg(t)
	metrics.SetRingSize(3)

	v := gatherGauge(t, reg, "dfs_ring_size")
	if v != 3 {
		t.Errorf("expected ring_size=3, got %v", v)
	}
}

func TestSetHintsPending(t *testing.T) {
	reg := newReg(t)
	metrics.SetHintsPending(12)

	v := gatherGauge(t, reg, "dfs_hints_pending")
	if v != 12 {
		t.Errorf("expected hints_pending=12, got %v", v)
	}
}

// ---------------------------------------------------------------------------
// All metric names registered
// ---------------------------------------------------------------------------

func TestAllMetricNamesRegistered(t *testing.T) {
	reg := newReg(t)
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	names := make(map[string]bool, len(gathered))
	for _, mf := range gathered {
		names[mf.GetName()] = true
	}
	expected := []string{
		"dfs_store_ops_total", "dfs_get_ops_total", "dfs_replication_total",
		"dfs_gossip_rounds_total", "dfs_heartbeats_total",
		"dfs_store_duration_seconds", "dfs_get_duration_seconds",
		"dfs_gossip_duration_seconds", "dfs_quorum_duration_seconds",
		"dfs_peer_count", "dfs_ring_size", "dfs_hints_pending",
	}
	var missing []string
	for _, e := range expected {
		if !names[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		t.Errorf("missing metrics: %s", strings.Join(missing, ", "))
	}
}

// ---------------------------------------------------------------------------
// Safe before Init (no panic)
// ---------------------------------------------------------------------------

func TestRecordSafeBeforeInit(t *testing.T) {
	// Simulate uninitialized state by resetting without calling Init.
	// Metric objects are always non-nil singletons; calls must not panic.
	metrics.Reset()
	metrics.RecordStore("store", nil, time.Millisecond)
	metrics.RecordGet("local", nil, time.Millisecond)
	metrics.RecordReplication("ok")
	metrics.RecordGossipRound(time.Millisecond)
	metrics.RecordHeartbeat("sent")
	metrics.RecordQuorum("read", time.Millisecond)
	metrics.SetPeerCount(1)
	metrics.SetRingSize(3)
	metrics.SetHintsPending(0)
}
