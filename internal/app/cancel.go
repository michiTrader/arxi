package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/michiTrader/arxi/internal/inbox"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
)

// Cancel appends the same confirmed run.cancelled event used by the CLI.
func (s MutationServices) Cancel(jobID, reason string) (MutationResult, error) {
	return s.cancel(jobID, reason, nil)
}

// CancelStore applies Cancel through an already-open run writer.
func (s MutationServices) CancelStore(store *logstore.Store, jobID, reason string) (MutationResult, error) {
	return s.cancel(jobID, reason, store)
}

func (s MutationServices) cancel(jobID, reason string, store *logstore.Store) (MutationResult, error) {
	const op = "cancel"
	if strings.TrimSpace(jobID) == "" {
		return MutationResult{}, &Error{Kind: InvalidArgument, Op: op, JobID: jobID, Cause: fmt.Errorf("job is required")}
	}
	dir, err := s.jobDir(jobID)
	if err != nil {
		kind := NotFound
		if errors.Is(err, ErrInvalidArgument) {
			kind = InvalidArgument
		} else if !os.IsNotExist(err) {
			kind = StorageUnavailable
		}
		return MutationResult{}, &Error{Kind: kind, Op: op, JobID: jobID, Cause: err}
	}

	if store != nil && filepath.Clean(store.Dir()) != filepath.Clean(dir) {
		return MutationResult{}, &Error{Kind: InvalidArgument, Op: op, JobID: jobID,
			Cause: fmt.Errorf("supplied store belongs to %s, not job %s", store.Dir(), jobID)}
	}
	if store == nil {
		store, err = logstore.Open(dir)
		if err != nil {
			kind := StorageUnavailable
			var locked *logstore.LockedError
			if errors.As(err, &locked) {
				kind = Conflict
			}
			return MutationResult{}, &Error{Kind: kind, Op: op, JobID: jobID, Cause: err}
		}
		defer store.Close()
	}

	run, err := inbox.OpenRun(dir)
	if err != nil {
		return MutationResult{}, &Error{Kind: StorageUnavailable, Op: op, JobID: jobID, Cause: err}
	}
	st, err := store.Fold(run.Config(), 0)
	if err != nil {
		return MutationResult{}, &Error{Kind: StorageUnavailable, Op: op, JobID: jobID, Cause: err}
	}
	if st.Status.Terminal() {
		return MutationResult{}, &Error{Kind: AlreadyTerminal, Op: op, JobID: jobID,
			Cause: fmt.Errorf("job is already %s", st.Status)}
	}
	payload := map[string]any{}
	if reason = strings.TrimSpace(reason); reason != "" {
		payload["reason"] = reason
	}
	ev := kernel.Event{
		ID: "cancel-" + strconv.FormatInt(store.Head()+1, 10), Type: kernel.RunCancelled,
		Source: kernel.SourceHuman, Scope: "run:" + jobID,
		Ts: s.now().UTC().Format("2006-01-02T15:04:05Z07:00"), Payload: payload,
	}
	if _, err := store.AppendIfSeq(st.Seq, []kernel.Event{ev}); err != nil {
		kind := StorageUnavailable
		var cas *logstore.CASError
		if errors.As(err, &cas) {
			kind = Conflict
		}
		return MutationResult{}, &Error{Kind: kind, Op: op, JobID: jobID, Cause: err}
	}
	return s.load(op, jobID)
}
