// Package transfer provides an in-memory registry for active file transfers.
// It tracks upload/download progress with unique IDs and supports
// pause/cancel via context cancellation.
package transfer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Direction indicates whether a transfer is an upload or download.
type Direction int

const (
	Upload   Direction = iota
	Download
)

func (d Direction) String() string {
	if d == Upload {
		return "upload"
	}
	return "download"
}

// Status tracks the lifecycle state of a transfer.
type Status int

const (
	Queued Status = iota
	Active
	Paused
	Completed
	Failed
)

func (s Status) String() string {
	switch s {
	case Queued:
		return "queued"
	case Active:
		return "active"
	case Paused:
		return "paused"
	case Completed:
		return "completed"
	case Failed:
		return "failed"
	default:
		return "unknown"
	}
}

// TransferInfo holds metadata and progress for a single active transfer.
type TransferInfo struct {
	ID        string    `json:"id"`
	Direction Direction `json:"direction"`
	Status    Status    `json:"status"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	IsDir     bool      `json:"isDir"`
	Encrypted bool      `json:"encrypted"`
	Public    bool      `json:"public"`
	Completed int       `json:"completed"`
	Total     int       `json:"total"`
	BytesDone int64     `json:"bytesDone"`
	Speed     float64   `json:"speed"`
	StartedAt time.Time `json:"startedAt"`
	Error     string    `json:"error,omitempty"`

	// Fields for daemon-side resume (so user doesn't re-enter paths).
	FilePath  string `json:"filePath"`            // absolute local file path
	OutputDir string `json:"outputDir,omitempty"` // for directory downloads
	DEK       []byte `json:"-"`                   // encryption key (ECDH)
}

// trackedTransfer wraps TransferInfo with internal synchronization and cancel.
type trackedTransfer struct {
	TransferInfo
	cancel    context.CancelFunc
	mu        sync.Mutex
	lastBytes int64     // bytes at last progress update
	lastTime  time.Time // wall-clock at last progress update
}

// Manager is a thread-safe in-memory registry of active transfers.
type Manager struct {
	mu        sync.RWMutex
	transfers map[string]*trackedTransfer
	counter   uint32
}

// NewManager creates a new transfer manager.
func NewManager() *Manager {
	return &Manager{
		transfers: make(map[string]*trackedTransfer),
	}
}

// Register creates a new transfer entry and returns its ID, a cancellable
// context, and a cancel function. The caller should defer cancel() and
// Remove(id) when the transfer completes.
func (m *Manager) Register(dir Direction, key, name, filePath string, size int64, isDir, encrypted, public bool) (string, context.Context, context.CancelFunc) {
	id := fmt.Sprintf("%08x", atomic.AddUint32(&m.counter, 1))
	ctx, cancel := context.WithCancel(context.Background())

	t := &trackedTransfer{
		TransferInfo: TransferInfo{
			ID:        id,
			Direction: dir,
			Status:    Queued,
			Key:       key,
			Name:      name,
			FilePath:  filePath,
			Size:      size,
			IsDir:     isDir,
			Encrypted: encrypted,
			Public:    public,
			StartedAt: time.Now(),
		},
		cancel: cancel,
	}

	m.mu.Lock()
	m.transfers[id] = t
	m.mu.Unlock()

	return id, ctx, cancel
}

// SetStatus changes the status of a transfer.
func (m *Manager) SetStatus(id string, status Status, errMsg string) {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return
	}
	t.mu.Lock()
	t.Status = status
	t.Error = errMsg
	t.mu.Unlock()
}

// UpdateProgress updates chunk progress and speed for a transfer.
// Speed is calculated as an EWMA (exponentially weighted moving average) of
// recent throughput so that the display reacts quickly to speed changes
// instead of showing a lifetime average that barely moves after 30 minutes.
func (m *Manager) UpdateProgress(id string, completed, total int, bytesDone int64) {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return
	}
	t.mu.Lock()
	t.Completed = completed
	t.Total = total
	t.BytesDone = bytesDone

	now := time.Now()
	if t.lastTime.IsZero() {
		// First update — seed with lifetime average so speed is non-zero immediately.
		t.lastTime = t.StartedAt
		t.lastBytes = 0
		dt := now.Sub(t.lastTime).Seconds()
		if dt > 0 && bytesDone > 0 {
			t.Speed = float64(bytesDone) / dt
		}
		t.lastBytes = bytesDone
		t.lastTime = now
	} else {
		dt := now.Sub(t.lastTime).Seconds()
		if dt > 0.5 { // only recalculate when ≥500ms has passed
			instantSpeed := float64(bytesDone-t.lastBytes) / dt
			const alpha = 0.3 // smoothing factor — higher = more reactive
			if t.Speed <= 0 {
				t.Speed = instantSpeed
			} else {
				t.Speed = alpha*instantSpeed + (1-alpha)*t.Speed
			}
			t.lastBytes = bytesDone
			t.lastTime = now
		}
	}

	t.mu.Unlock()
}

// Cancel cancels a transfer, fires context cancellation, and removes it from the registry.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	t, ok := m.transfers[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("transfer %q not found", id)
	}
	delete(m.transfers, id)
	m.mu.Unlock()

	t.mu.Lock()
	t.Status = Failed
	t.Error = "cancelled"
	t.cancel()
	t.mu.Unlock()
	return nil
}

// Pause pauses a transfer by cancelling its context but keeping it in the registry.
// The entry stays with Status=Paused so it can be resumed via Resume().
func (m *Manager) Pause(id string) error {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("transfer %q not found", id)
	}
	t.mu.Lock()
	t.Status = Paused
	t.cancel()
	t.mu.Unlock()
	return nil
}

// Resume re-activates a paused transfer with a fresh context.
// Returns the new context and cancel function for the re-spawned worker.
func (m *Manager) Resume(id string) (context.Context, context.CancelFunc, error) {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("transfer %q not found", id)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Status != Paused {
		return nil, nil, fmt.Errorf("transfer %q is not paused (status: %s)", id, t.Status.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.Status = Active
	t.Error = ""
	return ctx, cancel, nil
}

// SetOutputDir sets the output directory for a directory download transfer.
func (m *Manager) SetOutputDir(id, dir string) {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return
	}
	t.mu.Lock()
	t.OutputDir = dir
	t.mu.Unlock()
}

// SetSize updates the size of a transfer after the actual size is known.
func (m *Manager) SetSize(id string, size int64) {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return
	}
	t.mu.Lock()
	t.Size = size
	t.mu.Unlock()
}

// SetDEK sets the encryption key for a transfer (for resume).
func (m *Manager) SetDEK(id string, dek []byte) {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return
	}
	t.mu.Lock()
	t.DEK = dek
	t.mu.Unlock()
}

// Complete marks a transfer as completed. The entry persists in the registry
// for the daemon's lifetime so the TUI transfers tab can display history.
func (m *Manager) Complete(id string) {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return
	}
	t.mu.Lock()
	t.Status = Completed
	t.mu.Unlock()
}

// Remove removes a transfer from the registry.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	delete(m.transfers, id)
	m.mu.Unlock()
}

// snapshot returns a plain copy of TransferInfo. Caller must hold t.mu.
func (t *trackedTransfer) snapshot() TransferInfo {
	return t.TransferInfo
}

// Get returns a snapshot of a single transfer.
func (m *Manager) Get(id string) (TransferInfo, bool) {
	m.mu.RLock()
	t, ok := m.transfers[id]
	m.mu.RUnlock()
	if !ok {
		return TransferInfo{}, false
	}
	t.mu.Lock()
	s := t.snapshot()
	t.mu.Unlock()
	return s, true
}

// List returns a snapshot of all active transfers.
func (m *Manager) List() []TransferInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]TransferInfo, 0, len(m.transfers))
	for _, t := range m.transfers {
		t.mu.Lock()
		result = append(result, t.snapshot())
		t.mu.Unlock()
	}
	return result
}
