// Package logging is the named "claude-swap" logger with the interop log format.
//
// Implements spec 08§12 (logging_config.py) and DESIGN A4. The on-disk line
// format "YYYY-MM-DD HH:MM:SS,mmm - LEVEL - message" (Python %(asctime)s with a
// COMMA before milliseconds) is a hard interop contract: the TUI switch-history
// reader parses it. The log directory is created lazily on the first write, and
// files rotate at 1 MB with 3 backups.
package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
)

// LoggerName is the shared logger name (parity with logging.getLogger).
const LoggerName = "claude-swap"

const (
	maxBytes    = 1024 * 1024 // 1 MB
	backupCount = 3
)

// Level is a log severity, matching Python's numeric ordering.
type Level int

const (
	// LevelDebug is DEBUG (10).
	LevelDebug Level = 10
	// LevelInfo is INFO (20).
	LevelInfo Level = 20
	// LevelWarning is WARNING (30).
	LevelWarning Level = 30
	// LevelError is ERROR (40).
	LevelError Level = 40
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarning:
		return "WARNING"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

func levelFromString(s string) (Level, bool) {
	switch s {
	case "DEBUG":
		return LevelDebug, true
	case "INFO":
		return LevelInfo, true
	case "WARNING":
		return LevelWarning, true
	case "ERROR":
		return LevelError, true
	default:
		return 0, false
	}
}

// Record is one parsed log line.
type Record struct {
	Time    time.Time
	Level   Level
	Message string
}

const timeLayout = "2006-01-02 15:04:05,000"

// FormatLine renders a record as the on-disk contract line (no trailing newline).
func FormatLine(r Record) string {
	return r.Time.Format(timeLayout) + " - " + r.Level.String() + " - " + r.Message
}

// ParseLine parses a contract line back into a Record (used by the TUI history
// reader). It returns ok=false when the line does not match the format. The
// timestamp is interpreted in the local zone, so FormatLine(ParseLine(x)) == x.
func ParseLine(line string) (Record, bool) {
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " - ", 3)
	if len(parts) != 3 {
		return Record{}, false
	}
	t, err := time.ParseInLocation(timeLayout, parts[0], time.Local)
	if err != nil {
		return Record{}, false
	}
	lvl, ok := levelFromString(parts[1])
	if !ok {
		return Record{}, false
	}
	return Record{Time: t, Level: lvl, Message: parts[2]}, true
}

// Logger writes contract-format lines to a rotating file, creating the directory
// lazily on first write. In debug mode it also mirrors records to stderr.
type Logger struct {
	mu      sync.Mutex
	w       *rotatingWriter
	level   Level
	debug   bool
	clk     clock.Clock
	console io.Writer
}

// New configures the "claude-swap" logger writing to dir/claude-swap.log. The
// directory is not created until the first record is written.
func New(dir string, debug bool) *Logger {
	return NewWithClock(dir, debug, clock.System{})
}

// NewWithClock is New with an injectable clock for deterministic timestamps.
func NewWithClock(dir string, debug bool, clk clock.Clock) *Logger {
	lvl := LevelInfo
	if debug {
		lvl = LevelDebug
	}
	l := &Logger{
		w:     newRotatingWriter(filepath.Join(dir, "claude-swap.log"), maxBytes, backupCount),
		level: lvl,
		debug: debug,
		clk:   clk,
	}
	if debug {
		l.console = os.Stderr
	}
	return l
}

func (l *Logger) emit(level Level, msg string) {
	if level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	line := FormatLine(Record{Time: l.clk.Now(), Level: level, Message: msg}) + "\n"
	_ = l.w.write([]byte(line))
	if l.console != nil {
		fmt.Fprintf(l.console, "%s: %s\n", level.String(), msg)
	}
}

// Debug logs at DEBUG (dropped unless the logger is in debug mode).
func (l *Logger) Debug(msg string) { l.emit(LevelDebug, msg) }

// Info logs at INFO.
func (l *Logger) Info(msg string) { l.emit(LevelInfo, msg) }

// Warning logs at WARNING.
func (l *Logger) Warning(msg string) { l.emit(LevelWarning, msg) }

// Error logs at ERROR.
func (l *Logger) Error(msg string) { l.emit(LevelError, msg) }

// Infof / Warningf / Debugf / Errorf are printf-style convenience wrappers.
func (l *Logger) Infof(format string, a ...any)    { l.Info(fmt.Sprintf(format, a...)) }
func (l *Logger) Warningf(format string, a ...any) { l.Warning(fmt.Sprintf(format, a...)) }
func (l *Logger) Debugf(format string, a ...any)   { l.Debug(fmt.Sprintf(format, a...)) }
func (l *Logger) Errorf(format string, a ...any)   { l.Error(fmt.Sprintf(format, a...)) }
