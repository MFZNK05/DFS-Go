// Package logging provides structured JSON logging for DFS-Go components.
//
// Every log line is a JSON object containing at minimum:
//
//	{"level":"info","component":"server","time":"...","message":"..."}
//
// Use the Global logger for convenience or create per-component loggers via New.
// Child loggers (via With) inherit all fields from their parent.
//
// Example:
//
//	logging.Init("server", logging.LevelInfo)
//	logging.Global.Info("started", "port", 3000)
//	child := logging.Global.With("key", "notes.pdf")
//	child.Error("store failed", err)
package logging

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// Level controls the minimum severity that is emitted.
type Level int8

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func toZeroLevel(l Level) zerolog.Level {
	switch l {
	case LevelDebug:
		return zerolog.DebugLevel
	case LevelWarn:
		return zerolog.WarnLevel
	case LevelError:
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

// Logger wraps zerolog.Logger to provide a simplified key-value API.
type Logger struct {
	zl zerolog.Logger
}

// Global is the package-level logger. Call Init before using it.
var Global *Logger

// Init initialises Global with the given component name and minimum level.
// It writes JSON to stdout. Safe to call multiple times (overwrites Global).
func Init(component string, level Level) {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zl := zerolog.New(os.Stdout).
		Level(toZeroLevel(level)).
		With().
		Timestamp().
		Str("component", component).
		Logger()
	Global = &Logger{zl: zl}
}

// InitWithWriter creates a Global logger that writes to w (useful in tests).
func InitWithWriter(w io.Writer, component string, level Level) {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zl := zerolog.New(w).
		Level(toZeroLevel(level)).
		With().
		Timestamp().
		Str("component", component).
		Logger()
	Global = &Logger{zl: zl}
}

// New creates a standalone Logger (does not affect Global).
func New(component string, level Level) *Logger {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zl := zerolog.New(os.Stdout).
		Level(toZeroLevel(level)).
		With().
		Timestamp().
		Str("component", component).
		Logger()
	return &Logger{zl: zl}
}

// With returns a child Logger with additional permanent key-value fields.
// fields must be even-length: alternating string keys and any values.
func (l *Logger) With(fields ...interface{}) *Logger {
	ctx := l.zl.With()
	for i := 0; i+1 < len(fields); i += 2 {
		key, _ := fields[i].(string)
		ctx = ctx.Interface(key, fields[i+1])
	}
	return &Logger{zl: ctx.Logger()}
}

// Info logs msg at INFO level with optional key-value pairs.
func (l *Logger) Info(msg string, fields ...interface{}) {
	l.event(l.zl.Info(), msg, fields)
}

// Warn logs msg at WARN level.
func (l *Logger) Warn(msg string, fields ...interface{}) {
	l.event(l.zl.Warn(), msg, fields)
}

// Error logs msg at ERROR level. err may be nil.
func (l *Logger) Error(msg string, err error, fields ...interface{}) {
	e := l.zl.Error()
	if err != nil {
		e = e.Err(err)
	}
	l.event(e, msg, fields)
}

// Debug logs msg at DEBUG level.
func (l *Logger) Debug(msg string, fields ...interface{}) {
	l.event(l.zl.Debug(), msg, fields)
}

// event attaches key-value fields and sends the event.
func (l *Logger) event(e *zerolog.Event, msg string, fields []interface{}) {
	for i := 0; i+1 < len(fields); i += 2 {
		key, _ := fields[i].(string)
		e = e.Interface(key, fields[i+1])
	}
	e.Msg(msg)
}
