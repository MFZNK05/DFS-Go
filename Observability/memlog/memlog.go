// Package memlog provides periodic runtime memory statistics logging and
// automatic heap profile dumps at configurable thresholds. This enables
// post-crash forensics for OOM kills (SIGKILL is uncatchable, so we need
// pre-crash dumps written to disk).
package memlog

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"
)

// Thresholds at which heap profiles are auto-dumped to disk.
// Only one dump per threshold to avoid flooding disk.
var thresholds = []uint64{
	500 << 20, // 500 MB
	1 << 30,   // 1 GB
	2 << 30,   // 2 GB
	4 << 30,   // 4 GB
}

// Monitor periodically logs runtime.MemStats and auto-dumps heap profiles.
type Monitor struct {
	dumpDir  string
	interval time.Duration
	stopCh   chan struct{}
	once     sync.Once
	dumped   map[uint64]bool
}

// New creates a Monitor that logs every interval and dumps heap profiles
// into dumpDir when thresholds are crossed.
func New(dumpDir string, interval time.Duration) *Monitor {
	return &Monitor{
		dumpDir:  dumpDir,
		interval: interval,
		stopCh:   make(chan struct{}),
		dumped:   make(map[uint64]bool),
	}
}

// Start launches the background sampling loop.
func (m *Monitor) Start() {
	go m.loop()
}

// Stop shuts down the monitor. Safe to call multiple times.
func (m *Monitor) Stop() {
	m.once.Do(func() { close(m.stopCh) })
}

func (m *Monitor) loop() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.sample()
		case <-m.stopCh:
			return
		}
	}
}

func (m *Monitor) sample() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	goroutines := runtime.NumGoroutine()

	log.Printf("[memstat] heap_alloc=%dMB heap_sys=%dMB heap_inuse=%dMB goroutines=%d gc_runs=%d next_gc=%dMB stack_sys=%dMB",
		ms.HeapAlloc>>20, ms.HeapSys>>20, ms.HeapInuse>>20,
		goroutines, ms.NumGC, ms.NextGC>>20, ms.StackSys>>20)

	for _, t := range thresholds {
		if ms.HeapAlloc >= t && !m.dumped[t] {
			m.dumped[t] = true
			m.dumpHeap(ms.HeapAlloc)
		}
	}
}

func (m *Monitor) dumpHeap(allocBytes uint64) {
	_ = os.MkdirAll(m.dumpDir, 0700)
	name := fmt.Sprintf("heap-%s-%dMB.prof",
		time.Now().Format("150405"), allocBytes>>20)
	path := filepath.Join(m.dumpDir, name)
	f, err := os.Create(path)
	if err != nil {
		log.Printf("[memstat] failed to create heap dump %s: %v", path, err)
		return
	}
	defer f.Close()
	if err := pprof.WriteHeapProfile(f); err != nil {
		log.Printf("[memstat] failed to write heap dump: %v", err)
		return
	}
	log.Printf("[memstat] HEAP DUMP written to %s (heap=%dMB)", path, allocBytes>>20)
}
