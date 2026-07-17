// Implements spec 08§12 rotation: RotatingFileHandler(maxBytes=1MB,
// backupCount=3, delay=True) with a lazy-dir _open override. The parent dir and
// file are created on the first write, and a record that would push the file to
// or past maxBytes triggers a rollover before it is written.

package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	f        *os.File
	size     int64
}

func newRotatingWriter(path string, maxBytes int64, backups int) *rotatingWriter {
	return &rotatingWriter{path: path, maxBytes: maxBytes, backups: backups}
}

// open lazily creates the parent dir and opens (or reopens) the log file.
func (w *rotatingWriter) open() error {
	if w.f != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.size = fi.Size()
	return nil
}

func (w *rotatingWriter) write(p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.open(); err != nil {
		return err
	}
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) >= w.maxBytes {
		if err := w.rollover(); err != nil {
			return err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return err
}

// rollover shifts path.(n) → path.(n+1) down to path → path.1, discarding the
// oldest, then reopens a fresh log file (mirroring RotatingFileHandler.doRollover).
func (w *rotatingWriter) rollover() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	for i := w.backups - 1; i >= 1; i-- {
		sfn := fmt.Sprintf("%s.%d", w.path, i)
		dfn := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(sfn); err == nil {
			_ = os.Remove(dfn)
			_ = os.Rename(sfn, dfn)
		}
	}
	dfn := w.path + ".1"
	_ = os.Remove(dfn)
	_ = os.Rename(w.path, dfn)
	return w.open()
}
