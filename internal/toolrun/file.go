package toolrun

import (
	"fmt"
	"io"
	"os"

	"github.com/michiTrader/arxi/internal/workspace"
)

// maxReadBytes caps what a read tool may pull into memory.
//
// The number is not about disk or RAM, it is about the log. Tool output becomes
// an event, and an event becomes a line in the run log that somebody replays
// later. A member that reads a 900 MB file produces a run nobody can open, so
// the artefact this project exists to make readable is destroyed by a tool that
// technically succeeded.
const maxReadBytes = 1 << 20 // 1 MiB

// WriteFile writes data to a path inside the workspace.
//
// This exists so that no caller ever holds a resolved path and opens it itself.
// Resolve alone is not the confinement — Resolve plus openNoFollow is — and a
// caller who has one and forgets the other has written the invisible bug this
// package was created to prevent. Handing out an io-capable method and never a
// bare path is what makes forgetting impossible rather than merely discouraged.
// requireWrite refuses a mutating tool against a session whose frozen file
// access is not write, before any path is touched.
//
// This is deliberately redundant with preflight: preflight refuses the
// configuration at acceptance time, this refuses the dispatch at execution
// time. A future code path that grants a mutating tool to a read-only session
// fails closed at the point of use instead of silently succeeding because
// nothing checked. The zero value of fileAccess is FileAccessNone, so an
// unplumbed access refuses writes too: a missing value is not a promise of
// write, and making it one would return read-only-ness to accident status
// (ADR-0017).
func (w *Workspace) requireWrite(path string) error {
	if w.fileAccess == workspace.FileAccessWrite {
		return nil
	}
	stated := string(w.fileAccess)
	if stated == "" {
		stated = "unstated"
	}
	return fmt.Errorf("toolrun: %s is read-only (file access %s) and may not write %q\n"+
		"  this is the session boundary of ADR-0017: preflight refused the configuration at "+
		"acceptance, the session refuses the dispatch at execution, and a future grant that "+
		"misses preflight still cannot mutate the frozen tree", w.Member, stated, path)
}

func (w *Workspace) WriteFile(path string, data []byte) error {
	if err := w.requireWrite(path); err != nil {
		return err
	}
	f, err := w.openRelative(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("toolrun: %s writing %s: %w", w.Member, path, err)
	}
	// Closed explicitly as well as deferred, because a write error surfaces at
	// close for a buffered file and a deferred Close discards it. Reporting
	// success on a write that failed to flush is how a run log ends up claiming
	// a file exists when it does not.
	if err := f.Close(); err != nil {
		return fmt.Errorf("toolrun: %s closing %s: %w", w.Member, path, err)
	}
	return nil
}

// ReadFile reads a path inside the workspace, refusing anything oversized.
//
// The size limit is enforced by reading one byte past it rather than by trusting
// Stat: the size a Stat reports and the size a read produces are two different
// facts about a file that another process may still be writing, and /proc-style
// files report zero while returning content forever.
func (w *Workspace) ReadFile(path string) ([]byte, error) {
	f, err := w.openRelative(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("toolrun: %s reading %s: %w", w.Member, path, err)
	}
	if len(data) > maxReadBytes {
		return nil, fmt.Errorf("toolrun: %s: %s is larger than the %d byte read limit\n"+
			"  tool output becomes an event in the run log, and a run log nobody can "+
			"open is the artefact this project exists to produce\n"+
			"  read a part of it instead", w.Member, path, maxReadBytes)
	}
	return data, nil
}
