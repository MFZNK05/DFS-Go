package transfer

import (
	"testing"
	"time"
)

func TestRegisterAndList(t *testing.T) {
	m := NewManager()

	id1, _, cancel1 := m.Register(Upload, "fp/file1", "file1", "/tmp/file1", 1024, false, false, true)
	defer cancel1()
	id2, _, cancel2 := m.Register(Download, "fp/file2", "file2", "/tmp/file2", 2048, false, true, false)
	defer cancel2()

	if id1 == id2 {
		t.Fatal("expected unique IDs")
	}

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 transfers, got %d", len(list))
	}
}

func TestGet(t *testing.T) {
	m := NewManager()

	id, _, cancel := m.Register(Upload, "fp/test", "test", "/tmp/test", 100, false, false, false)
	defer cancel()

	info, ok := m.Get(id)
	if !ok {
		t.Fatal("expected to find transfer")
	}
	if info.Key != "fp/test" {
		t.Fatalf("expected key fp/test, got %s", info.Key)
	}
	if info.FilePath != "/tmp/test" {
		t.Fatalf("expected filePath /tmp/test, got %s", info.FilePath)
	}
	if info.Status != Queued {
		t.Fatalf("expected Queued, got %s", info.Status.String())
	}
}

func TestUpdateProgress(t *testing.T) {
	m := NewManager()

	id, _, cancel := m.Register(Upload, "fp/test", "test", "/tmp/test", 1<<20, false, false, false)
	defer cancel()

	m.SetStatus(id, Active, "")
	m.UpdateProgress(id, 5, 10, 512*1024)

	info, ok := m.Get(id)
	if !ok {
		t.Fatal("expected to find transfer")
	}
	if info.Completed != 5 || info.Total != 10 {
		t.Fatalf("expected 5/10, got %d/%d", info.Completed, info.Total)
	}
	if info.BytesDone != 512*1024 {
		t.Fatalf("expected %d bytes, got %d", 512*1024, info.BytesDone)
	}
	if info.Speed <= 0 {
		t.Fatal("expected positive speed")
	}
}

func TestCancel(t *testing.T) {
	m := NewManager()

	id, ctx, _ := m.Register(Upload, "fp/test", "test", "/tmp/test", 100, false, false, false)
	m.SetStatus(id, Active, "")

	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}

	// Context should be cancelled.
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected context to be cancelled")
	}

	// Cancel removes from registry.
	_, ok := m.Get(id)
	if ok {
		t.Fatal("expected transfer to be removed after cancel")
	}
}

func TestPause(t *testing.T) {
	m := NewManager()

	id, ctx, _ := m.Register(Download, "fp/test", "test", "/tmp/test", 100, false, false, false)
	m.SetStatus(id, Active, "")

	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected context to be cancelled on pause")
	}

	// Pause keeps entry in registry.
	info, ok := m.Get(id)
	if !ok {
		t.Fatal("expected transfer to remain in registry after pause")
	}
	if info.Status != Paused {
		t.Fatalf("expected Paused, got %s", info.Status.String())
	}
}

func TestResume(t *testing.T) {
	m := NewManager()

	id, _, _ := m.Register(Upload, "fp/test", "test", "/tmp/test", 100, false, false, false)
	m.SetStatus(id, Active, "")

	// Pause first.
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}

	// Resume.
	ctx, cancel, err := m.Resume(id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	// New context should not be cancelled.
	select {
	case <-ctx.Done():
		t.Fatal("new context should not be cancelled")
	default:
	}

	info, ok := m.Get(id)
	if !ok {
		t.Fatal("expected transfer to exist")
	}
	if info.Status != Active {
		t.Fatalf("expected Active after resume, got %s", info.Status.String())
	}
}

func TestResumeNotPaused(t *testing.T) {
	m := NewManager()

	id, _, cancel := m.Register(Upload, "fp/test", "test", "/tmp/test", 100, false, false, false)
	defer cancel()
	m.SetStatus(id, Active, "")

	_, _, err := m.Resume(id)
	if err == nil {
		t.Fatal("expected error when resuming non-paused transfer")
	}
}

func TestResumeNotFound(t *testing.T) {
	m := NewManager()
	_, _, err := m.Resume("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent ID")
	}
}

func TestRemove(t *testing.T) {
	m := NewManager()

	id, _, cancel := m.Register(Upload, "fp/test", "test", "/tmp/test", 100, false, false, false)
	defer cancel()

	m.Remove(id)

	_, ok := m.Get(id)
	if ok {
		t.Fatal("expected transfer to be removed")
	}
	if len(m.List()) != 0 {
		t.Fatal("expected empty list after remove")
	}
}

func TestCancelNotFound(t *testing.T) {
	m := NewManager()
	if err := m.Cancel("nonexistent"); err == nil {
		t.Fatal("expected error for non-existent ID")
	}
}

func TestPauseNotFound(t *testing.T) {
	m := NewManager()
	if err := m.Pause("nonexistent"); err == nil {
		t.Fatal("expected error for non-existent ID")
	}
}

func TestDirectionString(t *testing.T) {
	if Upload.String() != "upload" {
		t.Fatalf("expected 'upload', got %q", Upload.String())
	}
	if Download.String() != "download" {
		t.Fatalf("expected 'download', got %q", Download.String())
	}
}

func TestStatusString(t *testing.T) {
	cases := map[Status]string{
		Queued: "queued",
		Active: "active",
		Paused: "paused",
		Failed: "failed",
	}
	for s, expected := range cases {
		if s.String() != expected {
			t.Fatalf("expected %q, got %q", expected, s.String())
		}
	}
}

func TestIDsAreSequential(t *testing.T) {
	m := NewManager()
	id1, _, c1 := m.Register(Upload, "a", "a", "/tmp/a", 0, false, false, false)
	defer c1()
	id2, _, c2 := m.Register(Upload, "b", "b", "/tmp/b", 0, false, false, false)
	defer c2()

	if id1 >= id2 {
		t.Fatalf("expected sequential IDs: %s < %s", id1, id2)
	}
}

func TestStartedAtSet(t *testing.T) {
	m := NewManager()
	before := time.Now()
	id, _, cancel := m.Register(Upload, "fp/test", "test", "/tmp/test", 100, false, false, false)
	defer cancel()

	info, _ := m.Get(id)
	if info.StartedAt.Before(before) {
		t.Fatal("expected StartedAt to be set")
	}
}

func TestPauseAndResumePreservesMetadata(t *testing.T) {
	m := NewManager()

	id, _, _ := m.Register(Upload, "fp/video", "video", "/home/user/video.mp4", 1<<30, false, true, true)
	m.SetStatus(id, Active, "")
	m.UpdateProgress(id, 50, 100, 500<<20)

	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}

	info, ok := m.Get(id)
	if !ok {
		t.Fatal("expected transfer to exist after pause")
	}
	if info.FilePath != "/home/user/video.mp4" {
		t.Fatalf("expected file path preserved, got %s", info.FilePath)
	}
	if info.Key != "fp/video" {
		t.Fatalf("expected key preserved, got %s", info.Key)
	}
	if info.Completed != 50 {
		t.Fatalf("expected progress preserved, got %d", info.Completed)
	}

	// Resume and verify metadata still intact.
	_, cancel, err := m.Resume(id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	info2, _ := m.Get(id)
	if info2.FilePath != "/home/user/video.mp4" {
		t.Fatalf("expected file path preserved after resume, got %s", info2.FilePath)
	}
}
