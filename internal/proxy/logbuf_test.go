package proxy

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"
)

// logBuffer is a bytes.Buffer that the logger's goroutine and the test's own
// can touch at once. The turn line and the panic line are written by handlers
// that run after the client has been answered, so a test that reads a plain
// buffer to wait for them is a data race, and the race detector fails the
// package on the runs where the write lands during the read.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs redirects the default logger to a JSON buffer at debug level for
// the duration of the test, and returns the buffer.
func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	return captureLogsLevel(t, slog.LevelDebug)
}

// captureLogsLevel is captureLogs with an explicit level, for the tests that
// assert what stays out of the log rather than what lands in it.
func captureLogsLevel(t *testing.T, level slog.Level) *logBuffer {
	t.Helper()
	logs := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}
