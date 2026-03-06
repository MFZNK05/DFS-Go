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
// Counters — allocated once at package init; pointers never change.
// ---------------------------------------------------------------------------

var (
	// StoreOpsTotal counts StoreData invocations.
	// operation: "store" (local) | "replicate" (outgoing copy)
	// status:    "ok" | "err"
	StoreOpsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dfs_store_ops_total",
		Help: "Total number of store operations.",
	}, []string{"operation", "status"})

	// GetOpsTotal counts GetData invocations.
	// source: "local" | "remote" | "miss" (not found)
	// status: "ok" | "err"
	GetOpsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dfs_get_ops_total",
		Help: "Total number of get operations.",
	}, []string{"source", "status"})

	// ReplicationTotal counts replication fan-out outcomes.
	// status: "ok" | "hint" (written to handoff store) | "skip" (already up-to-date)
	ReplicationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dfs_replication_total",
		Help: "Total number of replication fan-out outcomes.",
	}, []string{"status"})

	// GossipRoundsTotal counts gossip rounds completed.
	GossipRoundsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "dfs_gossip_rounds_total",
		Help: "Total gossip rounds completed.",
	})

	// HeartbeatsTotal counts heartbeats by direction.
	// direction: "sent" | "received"
	HeartbeatsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dfs_heartbeats_total",
		Help: "Total heartbeats sent or received.",
	}, []string{"direction"})
)

// ---------------------------------------------------------------------------
// Histograms — allocated once at package init; pointers never change.
// ---------------------------------------------------------------------------

var (
	// StoreDuration measures StoreData latency in seconds.
	StoreDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dfs_store_duration_seconds",
		Help:    "Latency distribution for store operations.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// GetDuration measures GetData latency in seconds.
	GetDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dfs_get_duration_seconds",
		Help:    "Latency distribution for get operations.",
		Buckets: prometheus.DefBuckets,
	}, []string{"source"})

	// GossipDuration measures how long a single gossip round takes.
	GossipDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "dfs_gossip_duration_seconds",
		Help:    "Latency of a single gossip round.",
		Buckets: prometheus.DefBuckets,
	})

	// QuorumDuration measures quorum operation latency.
	// type: "read" | "write"
	QuorumDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dfs_quorum_duration_seconds",
		Help:    "Latency distribution for quorum operations.",
		Buckets: prometheus.DefBuckets,
	}, []string{"type"})
)

// ---------------------------------------------------------------------------
// Gauges — allocated once at package init; pointers never change.
// ---------------------------------------------------------------------------

var (
	// PeerCount is the number of currently connected peers.
	PeerCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dfs_peer_count",
		Help: "Current number of connected peers.",
	})

	// RingSize is the number of nodes in the consistent hash ring.
	RingSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dfs_ring_size",
		Help: "Number of nodes in the consistent hash ring.",
	})

	// HintsPending is the total count of pending hinted-handoff hints.
	HintsPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dfs_hints_pending",
		Help: "Total pending hinted-handoff hints.",
	})
)

// allCollectors lists every metric object so Init/Reset can register/unregister
// them in one place without listing them twice.
var allCollectors = []prometheus.Collector{
	StoreOpsTotal, GetOpsTotal, ReplicationTotal,
	GossipRoundsTotal, HeartbeatsTotal,
	StoreDuration, GetDuration, GossipDuration, QuorumDuration,
	PeerCount, RingSize, HintsPending,
}

// ---------------------------------------------------------------------------
// Init / Reset
// ---------------------------------------------------------------------------

var (
	once sync.Once
	reg  prometheus.Registerer // registry used by Init; kept for Reset
)

// Init registers all DFS metrics with the provided Prometheus registry.
// If r is nil the default registry (prometheus.DefaultRegisterer) is used.
// Calling Init more than once is a no-op — metrics are registered exactly once.
func Init(r prometheus.Registerer) {
	once.Do(func() {
		if r == nil {
			r = prometheus.DefaultRegisterer
		}
		reg = r
		registerAll(r)
	})
}

// Reset resets the once guard so Init can be called again with a new registry.
// Gauge values are zeroed for a clean test slate.  Metric objects are never
// replaced — their memory addresses are stable — so goroutines that still hold
// a reference to e.g. PeerCount write safely without any data race.
//
// Do NOT call this in production code.
func Reset() {
	// Zero out gauge values so the next test starts from a clean slate.
	PeerCount.Set(0)
	RingSize.Set(0)
	HintsPending.Set(0)
	reg = nil
	once = sync.Once{}
}

func registerAll(r prometheus.Registerer) {
	for _, c := range allCollectors {
		r.MustRegister(c)
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
// Record helpers — thin wrappers used by server and cluster packages.
// These are always safe to call; the metric objects are never nil.
// ---------------------------------------------------------------------------

// RecordStore increments StoreOpsTotal and observes StoreDuration.
// operation: "store" or "replicate"; err nil → status "ok", else "err".
func RecordStore(operation string, err error, d time.Duration) {
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
	ReplicationTotal.WithLabelValues(status).Inc()
}

// RecordGossipRound increments GossipRoundsTotal and observes GossipDuration.
func RecordGossipRound(d time.Duration) {
	GossipRoundsTotal.Inc()
	GossipDuration.Observe(d.Seconds())
}

// RecordHeartbeat increments HeartbeatsTotal for direction ("sent" or "received").
func RecordHeartbeat(direction string) {
	HeartbeatsTotal.WithLabelValues(direction).Inc()
}

// RecordQuorum observes QuorumDuration for type ("read" or "write").
func RecordQuorum(opType string, d time.Duration) {
	QuorumDuration.WithLabelValues(opType).Observe(d.Seconds())
}

// SetPeerCount updates the PeerCount gauge.
func SetPeerCount(n int) {
	PeerCount.Set(float64(n))
}

// SetRingSize updates the RingSize gauge.
func SetRingSize(n int) {
	RingSize.Set(float64(n))
}

// SetHintsPending updates the HintsPending gauge.
func SetHintsPending(n int) {
	HintsPending.Set(float64(n))
}
