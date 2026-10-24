package tui

import (
	"io"
	"os"
	"sync"
)

// stdoutMu guards every write this package makes to the real os.Stdout —
// both Bubble Tea's own renderer (via Output, which cmd/canopy must hand to
// tea.NewProgram through tea.WithOutput) and writeOSC52ToStdout's clipboard
// writes (post-v0.1.0 addendum: mouse-driven text selection).
//
// Without this, the two race: Bubble Tea's standardRenderer flushes frames
// to stdout from its own ticker goroutine roughly every 16ms, completely
// unsynchronized with a direct os.Stdout.Write from Update. A single
// write(2) syscall is NOT guaranteed atomic with respect to a concurrent
// write(2) from another goroutine for a tty/pty file descriptor (that
// guarantee — POSIX's PIPE_BUF — only covers pipes), so an OSC 52 clipboard
// sequence can land interleaved with a render frame at the byte level. Most
// terminals silently tolerate the resulting garbage; some (observed: Ptyxis
// on Wayland/GNOME) parse it strictly enough that the corrupted sequence is
// just ignored — the clipboard never updates, even though nothing else
// about the TUI looks wrong, since every other effect of a copy is normal
// Bubble Tea state+render.
var stdoutMu sync.Mutex

// syncStdout wraps os.Stdout so every write to it goes through stdoutMu.
type syncStdout struct{}

func (syncStdout) Write(p []byte) (int, error) {
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	return os.Stdout.Write(p)
}

// Output is the io.Writer cmd/canopy must pass to tea.NewProgram via
// tea.WithOutput so Bubble Tea's renderer writes through the same lock as
// writeOSC52ToStdout's clipboard writes.
func Output() io.Writer {
	return syncStdout{}
}
