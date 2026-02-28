package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Faizan2005/DFS-Go/Observability/logging"
)

// captureLogger returns a Logger that writes JSON to a buffer.
func captureLogger(component string, level logging.Level) (*logging.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logging.InitWithWriter(&buf, component, level)
	return logging.Global, &buf
}

// parseJSON parses the last non-empty line of buf as a JSON map.
func parseJSON(t *testing.T, buf *bytes.Buffer) map[string]interface{} {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	last := lines[len(lines)-1]
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(last), &m); err != nil {
		t.Fatalf("output is not valid JSON: %s\nerr: %v", last, err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Info / Warn / Error / Debug
// ---------------------------------------------------------------------------

func TestInfoEmitsCorrectLevel(t *testing.T) {
	l, buf := captureLogger("test", logging.LevelDebug)
	l.Info("hello")
	m := parseJSON(t, buf)
	if m["level"] != "info" {
		t.Errorf("expected level=info, got %v", m["level"])
	}
	if m["message"] != "hello" {
		t.Errorf("expected message=hello, got %v", m["message"])
	}
}

func TestWarnEmitsCorrectLevel(t *testing.T) {
	l, buf := captureLogger("test", logging.LevelDebug)
	l.Warn("oops")
	m := parseJSON(t, buf)
	if m["level"] != "warn" {
		t.Errorf("expected level=warn, got %v", m["level"])
	}
}

func TestErrorIncludesErrField(t *testing.T) {
	l, buf := captureLogger("test", logging.LevelDebug)
	l.Error("failed", errors.New("disk full"))
	m := parseJSON(t, buf)
	if m["level"] != "error" {
		t.Errorf("expected level=error, got %v", m["level"])
	}
	if m["error"] != "disk full" {
		t.Errorf("expected error=disk full, got %v", m["error"])
	}
}

func TestErrorNilErrNoErrField(t *testing.T) {
	l, buf := captureLogger("test", logging.LevelDebug)
	l.Error("partial failure", nil)
	m := parseJSON(t, buf)
	if _, ok := m["error"]; ok {
		t.Errorf("expected no error field when err=nil, got %v", m["error"])
	}
}

func TestDebugEmittedAtDebugLevel(t *testing.T) {
	l, buf := captureLogger("test", logging.LevelDebug)
	l.Debug("verbose detail")
	m := parseJSON(t, buf)
	if m["level"] != "debug" {
		t.Errorf("expected level=debug, got %v", m["level"])
	}
}

func TestDebugSuppressedAtInfoLevel(t *testing.T) {
	l, buf := captureLogger("test", logging.LevelInfo)
	l.Debug("should not appear")
	if buf.Len() > 0 {
		t.Errorf("expected no output at Info level for Debug message, got: %s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Component field
// ---------------------------------------------------------------------------

func TestComponentFieldPresent(t *testing.T) {
	l, buf := captureLogger("hashring", logging.LevelDebug)
	l.Info("ready")
	m := parseJSON(t, buf)
	if m["component"] != "hashring" {
		t.Errorf("expected component=hashring, got %v", m["component"])
	}
}

// ---------------------------------------------------------------------------
// Key-value fields
// ---------------------------------------------------------------------------

func TestKeyValueFieldsAttached(t *testing.T) {
	l, buf := captureLogger("test", logging.LevelDebug)
	l.Info("stored", "key", "notes.pdf", "bytes", 1024)
	m := parseJSON(t, buf)
	if m["key"] != "notes.pdf" {
		t.Errorf("expected key=notes.pdf, got %v", m["key"])
	}
	// JSON numbers unmarshal as float64
	if m["bytes"].(float64) != 1024 {
		t.Errorf("expected bytes=1024, got %v", m["bytes"])
	}
}

// ---------------------------------------------------------------------------
// Child logger (With)
// ---------------------------------------------------------------------------

func TestWithInheritsParentFields(t *testing.T) {
	l, buf := captureLogger("server", logging.LevelDebug)
	child := l.With("peer", "192.168.1.2")
	child.Info("connected")
	m := parseJSON(t, buf)
	if m["peer"] != "192.168.1.2" {
		t.Errorf("expected peer field inherited, got %v", m["peer"])
	}
	if m["component"] != "server" {
		t.Errorf("expected component inherited in child, got %v", m["component"])
	}
}

func TestWithDoesNotMutateParent(t *testing.T) {
	l, buf := captureLogger("server", logging.LevelDebug)
	_ = l.With("peer", "192.168.1.2")
	// Parent logs independently
	buf.Reset()
	l.Info("parent log")
	m := parseJSON(t, buf)
	if _, ok := m["peer"]; ok {
		t.Errorf("parent should not have peer field, got %v", m["peer"])
	}
}

// ---------------------------------------------------------------------------
// New (standalone logger, does not affect Global)
// ---------------------------------------------------------------------------

func TestNewDoesNotOverwriteGlobal(t *testing.T) {
	logging.InitWithWriter(&bytes.Buffer{}, "global-comp", logging.LevelInfo)
	globalBefore := logging.Global

	_ = logging.New("standalone", logging.LevelDebug)

	if logging.Global != globalBefore {
		t.Error("New() must not overwrite Global")
	}
}

// ---------------------------------------------------------------------------
// Timestamp present
// ---------------------------------------------------------------------------

func TestTimestampPresent(t *testing.T) {
	l, buf := captureLogger("test", logging.LevelDebug)
	l.Info("ts check")
	m := parseJSON(t, buf)
	if _, ok := m["time"]; !ok {
		t.Errorf("expected time field in JSON output, got %v", m)
	}
}
