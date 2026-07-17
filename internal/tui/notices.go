// notices.go — the threadsafe bridge that carries the oauth persist-failure
// warning from the seam it is written to (potentially off the Update goroutine,
// e.g. the auto-switch engine's refresh) into the TUI's toast/notice model.
//
// FINDING 6: while the TUI holds the alt-screen, oauth.Output is pointed at a
// noticeCollector instead of io.Discard, because the persist-failure warning is
// the only user-visible surface for a lost-refresh-token condition (04§1.25).
// The Update goroutine drains the collector on each poll tick and after each
// mutating action, turning each collected line into a warning toast.
package tui

import (
	"regexp"
	"strings"
	"sync"
)

// ansiSeq matches the SGR escape sequences printer.Yellowed wraps the warning
// in when colour is enabled, so a collected line renders cleanly inside a toast
// (which applies its own severity colour).
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")

// noticeCollector is an io.Writer that accumulates whole lines written to it and
// hands them to the Update goroutine on drain. Every method is safe to call from
// any goroutine.
type noticeCollector struct {
	mu    sync.Mutex
	lines []string
}

// Write records each non-empty line in p. fmt.Fprintln delivers one warning per
// Write call, but splitting on newlines keeps a multi-line write intact too.
func (c *noticeCollector) Write(p []byte) (int, error) {
	n := len(p)
	for _, raw := range strings.Split(string(p), "\n") {
		line := strings.TrimRight(ansiSeq.ReplaceAllString(raw, ""), "\r")
		if line == "" {
			continue
		}
		c.mu.Lock()
		c.lines = append(c.lines, line)
		c.mu.Unlock()
	}
	return n, nil
}

// drain returns and clears the collected lines.
func (c *noticeCollector) drain() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) == 0 {
		return nil
	}
	out := c.lines
	c.lines = nil
	return out
}
