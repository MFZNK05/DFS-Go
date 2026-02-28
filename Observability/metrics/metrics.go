// Package metrics exposes Prometheus instrumentation for DFS-Go.
//
// Call Init() once at startup to register all metrics with the default registry.
// Then use the Record* helpers at operation boundaries so callers never import
// the prometheus package directly.
//
// Exposed metrics (all prefixed "dfs_"):
//
//	dfs_store_ops_total{operation,status}   — count of StoreData calls
//	dfs_get_ops_total{source,status}        — count of GetData calls
//	dfs_replication_total{status}           — replication fan-out results
//	dfs_gossip_rounds_total                 — gossip rounds executed
//	dfs_heartbeats_total{direction}         — heartbeats sent/received
//	dfs_store_duration_seconds{operation}   — latency histogram for stores
//	dfs_get_duration_seconds{source}        — latency histogram for gets
//	dfs_gossip_duration_seconds             — gossip round duration
//	dfs_quorum_duration_seconds{type}       — quorum read/write latency
//	dfs_peer_count                          — current connected peers
//	dfs_ring_size                           — nodes in consistent hash ring
//	dfs_hints_pending                       — pending hinted-handoff hints
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ---------------------------------------------------------------------------
// Counters
// ---------------------------------------------------------------------------

var (
	// StoreOpsTotal counts StoreData invocations.
	// operation: "store" (local) | "replicate" (outgoing copy)
	// status:    "ok" | "err"
	StoreOpsTotal *prometheus.CounterVec

	// GetOpsTotal counts GetData invocations.
	// source: "local" | "remote" | "miss" (not found)
	// status: "ok" | "err"
	GetOpsTotal *prometheus.CounterVec

	// ReplicationTotal counts replication fan-out outcomes.
	// status: "ok" | "hint" (written to handoff store) | "skip" (already up-to-date)
	ReplicationTotal *prometheus.CounterVec

	// GossipRoundsTotal counts gossip rounds completed.
	GossipRoundsTotal prometheus.Counter

	// HeartbeatsTotal counts heartbeats by direction.
	// direction: "sent" | "received"
	HeartbeatsTotal *prometheus.CounterVec
)

// ---------------------------------------------------------------------------
// Histograms
// ---------------------------------------------------------------------------

var (
	// StoreDuration measures StoreData latency in seconds.
	StoreDuration *prometheus.HistogramVec

	// GetDuration measures GetData latency in seconds.
	GetDuration *prometheus.HistogramVec

	// GossipDuration measures how long a single gossip round takes.
	GossipDuration prometheus.Histogram

	// QuorumDuration measures quorum operation latency.
	// type: "read" | "write"
	QuorumDuration *prometheus.HistogramVec
)

// ---------------------------------------------------------------------------
// Gauges
// ---------------------------------------------------------------------------

var (
	// PeerCount is the number of currently connected peers.
	PeerCount prometheus.Gauge

	// RingSize is the number of nodes in the consistent hash ring.
	RingSize prometheus.Gauge

	// HintsPending is the total count of pending hinted-handoff hints.
	HintsPending prometheus.Gauge
)

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

var (
	once     sync.Once
	registry *prometheus.Registry // package-local; nil means default
)

// Init registers all DFS metrics with the provided Prometheus registry.
// If reg is nil the default registry (prometheus.DefaultRegisterer) is used.
// Calling Init more than once is a no-op — metrics are registered exactly once.
func Init(reg prometheus.Registerer) {
	once.Do(func() {
		if reg == nil {
			reg = prometheus.DefaultRegisterer
		}
		initMetrics(reg)
	})
}

// Reset clears the once guard so tests can call Init with a fresh registry.
// Do NOT call this in production code.
func Reset() {
	once = sync.Once{}
}

func initMetrics(reg prometheus.Registerer) {
	StoreOpsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dfs_store_ops_total",
		Help: "Total number of store operations.",
	}, []string{"operation", "status"})

	GetOpsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dfs_get_ops_total",
		Help: "Total number of get operations.",
	}, []string{"source", "status"})

	ReplicationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dfs_replication_total",
		Help: "Total number of replication fan-out outcomes.",
	}, []string{"status"})

	GossipRoundsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "dfs_gossip_rounds_total",
		Help: "Total gossip rounds completed.",
	})

	HeartbeatsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dfs_heartbeats_total",
		Help: "Total heartbeats sent or received.",
	}, []string{"direction"})

	StoreDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dfs_store_duration_seconds",
		Help:    "Latency distribution for store operations.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	GetDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dfs_get_duration_seconds",
		Help:    "Latency distribution for get operations.",
		Buckets: prometheus.DefBuckets,
	}, []string{"source"})

	GossipDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "dfs_gossip_duration_seconds",
		Help:    "Latency of a single gossip round.",
		Buckets: prometheus.DefBuckets,
	})

	QuorumDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dfs_quorum_duration_seconds",
		Help:    "Latency distribution for quorum operations.",
		Buckets: prometheus.DefBuckets,
	}, []string{"type"})

	PeerCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dfs_peer_count",
		Help: "Current number of connected peers.",
	})

	RingSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dfs_ring_size",
		Help: "Number of nodes in the consistent hash ring.",
	})

	HintsPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dfs_hints_pending",
		Help: "Total pending hinted-handoff hints.",
	})

	collectors := []prometheus.Collector{
		StoreOpsTotal, GetOpsTotal, ReplicationTotal,
		GossipRoundsTotal, HeartbeatsTotal,
		StoreDuration, GetDuration, GossipDuration, QuorumDuration,
		PeerCount, RingSize, HintsPending,
	}
	for _, c := range collectors {
		reg.MustRegister(c)
	}

	// Pre-initialise well-known label sets so Gather() returns all metric
	// families even before the first real observation.
	for _, op := range []string{"store", "replicate"} {
		for _, st := range []string{"ok", "err"} {
			StoreOpsTotal.WithLabelValues(op, st)
			StoreDuration.WithLabelValues(op)
		}
	}
	for _, src := range []string{"local", "remote", "miss"} {
		for _, st := range []string{"ok", "err"} {
			GetOpsTotal.WithLabelValues(src, st)
		}
		GetDuration.WithLabelValues(src)
	}
	for _, st := range []string{"ok", "hint", "skip"} {
		ReplicationTotal.WithLabelValues(st)
	}
	for _, dir := range []string{"sent", "received"} {
		HeartbeatsTotal.WithLabelValues(dir)
	}
	for _, typ := range []string{"read", "write"} {
		QuorumDuration.WithLabelValues(typ)
	}
}

// ---------------------------------------------------------------------------
// Record helpers — thin wrappers used by server and cluster packages
// ---------------------------------------------------------------------------

// RecordStore increments StoreOpsTotal and observes StoreDuration.
// operation: "store" or "replicate"; err nil → status "ok", else "err".
func RecordStore(operation string, err error, d time.Duration) {
	if StoreOpsTotal == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "err"
	}
	StoreOpsTotal.WithLabelValues(operation, status).Inc()
	StoreDuration.WithLabelValues(operation).Observe(d.Seconds())
}

// RecordGet increments GetOpsTotal and observes GetDuration.
// source: "local", "remote", or "miss".
func RecordGet(source string, err error, d time.Duration) {
	if GetOpsTotal == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "err"
	}
	GetOpsTotal.WithLabelValues(source, status).Inc()
	GetDuration.WithLabelValues(source).Observe(d.Seconds())
}

// RecordReplication increments ReplicationTotal.
// status: "ok", "hint", or "skip".
func RecordReplication(status string) {
	if ReplicationTotal == nil {
		return
	}
	ReplicationTotal.WithLabelValues(status).Inc()
}

// RecordGossipRound increments GossipRoundsTotal and observes GossipDuration.
func RecordGossipRound(d time.Duration) {
	if GossipRoundsTotal == nil {
		return
	}
	GossipRoundsTotal.Inc()
	GossipDuration.Observe(d.Seconds())
}

// RecordHeartbeat increments HeartbeatsTotal for direction ("sent" or "received").
func RecordHeartbeat(direction string) {
	if HeartbeatsTotal == nil {
		return
	}
	HeartbeatsTotal.WithLabelValues(direction).Inc()
}

// RecordQuorum observes QuorumDuration for type ("read" or "write").
// err nil → the call is still observed (latency is always useful).
func RecordQuorum(opType string, d time.Duration) {
	if QuorumDuration == nil {
		return
	}
	QuorumDuration.WithLabelValues(opType).Observe(d.Seconds())
}

// SetPeerCount updates the PeerCount gauge.
func SetPeerCount(n int) {
	if PeerCount == nil {
		return
	}
	PeerCount.Set(float64(n))
}

// SetRingSize updates the RingSize gauge.
func SetRingSize(n int) {
	if RingSize == nil {
		return
	}
	RingSize.Set(float64(n))
}

// SetHintsPending updates the HintsPending gauge.
func SetHintsPending(n int) {
	if HintsPending == nil {
		return
	}
	HintsPending.Set(float64(n))
}
