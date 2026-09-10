// Package runread provides lock-free, confirmed-prefix run projections.
package runread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
)

// Run is one projection built from one confirmed read of the event log.
type Run struct {
	Dir             string
	State           kernel.State
	Config          kernel.Config
	Events          []kernel.Event
	Simulated       bool
	ConfirmedOffset int64
}

// Open reads, decodes, and folds one confirmed prefix without taking the writer lock.
func Open(dir string) (Run, error) {
	read, err := ReadConfirmed(dir, 0)
	if err != nil {
		return Run{}, err
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return Run{}, err
	}
	events, err := Decode(dir, read.Bytes)
	if err != nil {
		return Run{}, err
	}
	if err := ValidateSequence(dir, events, 0); err != nil {
		return Run{}, err
	}
	state, _ := kernel.Fold(kernel.State{}, events, cfg)
	return Run{
		Dir: dir, State: state, Config: cfg, Events: events,
		Simulated: Simulated(events), ConfirmedOffset: read.NextOffset,
	}, nil
}

// ReadConfirmed delegates confirmed-prefix selection to logstore.
func ReadConfirmed(dir string, fromOffset int64) (logstore.ConfirmedRead, error) {
	return logstore.ReadConfirmed(dir, fromOffset)
}

// ReadConfirmedLimit reads no more than maxBytes of complete confirmed records.
// Overflow leaves the caller's byte cursor unchanged.
func ReadConfirmedLimit(dir string, fromOffset, maxBytes int64) (logstore.ConfirmedRead, bool, error) {
	return logstore.ReadConfirmedLimit(dir, fromOffset, maxBytes)
}

// LoadConfig reads the immutable blueprint snapshot. Missing snapshots are valid
// for inspection of legacy runs and produce an empty reducer configuration.
func LoadConfig(dir string) (kernel.Config, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "blueprint.snapshot.yaml"))
	if os.IsNotExist(err) {
		return kernel.Config{}, nil
	}
	if err != nil {
		return kernel.Config{}, fmt.Errorf("read the frozen blueprint of %s: %w", dir, err)
	}
	bp, err := blueprint.Load(raw)
	if err != nil {
		return kernel.Config{}, fmt.Errorf("the frozen blueprint of %s does not parse, so this run cannot be folded: %w", dir, err)
	}
	return bp.Config, nil
}

// Decode parses complete NDJSON records. Confirmed reads never contain a partial
// line; accepting an unterminated tail keeps this helper safe for legacy callers.
func Decode(dir string, raw []byte) ([]kernel.Event, error) {
	var out []kernel.Event
	for lineNo := 1; len(raw) > 0; lineNo++ {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line := raw[:i]
		raw = raw[i+1:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event kernel.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("%s line %d of the log does not parse: %w\n  the log is the run's only history, so this is not skipped", dir, lineNo, err)
		}
		out = append(out, event)
	}
	return out, nil
}

// ValidateSequence rejects missing, duplicate, or reordered confirmed records.
func ValidateSequence(dir string, events []kernel.Event, after int64) error {
	expected := after + 1
	for _, event := range events {
		if event.Seq != expected {
			return fmt.Errorf("%s confirmed log sequence is %d, want %d", dir, event.Seq, expected)
		}
		expected++
	}
	return nil
}

// Simulated reports the immutable mode recorded by run.started.
func Simulated(events []kernel.Event) bool {
	for _, event := range events {
		if event.Type == kernel.RunStarted {
			value, _ := event.Payload["simulated"].(bool)
			return value
		}
	}
	return false
}
