package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/inbox"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
	"github.com/michiTrader/arxi/internal/runread"
)

// MutationServices owns shared lifecycle mutations over the existing run store.
type MutationServices struct {
	RunsDir string
	Now     func() time.Time
}

// MutationResult is the state needed to produce the updated public projection.
type MutationResult struct {
	State       kernel.State
	Simulated   bool
	UnknownWork int
}

// Decision identifies one exact human response scoped to a job.
type Decision struct {
	JobID     string
	ItemID    string
	Text      string
	Principal string
}

// Inspect refolds the confirmed log and returns its current projection.
func (s MutationServices) Inspect(jobID string) (MutationResult, error) {
	return s.load("inspect", jobID)
}

// Approve approves exactly one approval item.
func (s MutationServices) Approve(req Decision) (MutationResult, error) {
	return s.decide("approve", req, inbox.DecisionApprove, nil)
}

// Reject rejects exactly one approval item.
func (s MutationServices) Reject(req Decision) (MutationResult, error) {
	return s.decide("reject", req, inbox.DecisionReject, nil)
}

// Answer answers exactly one question item.
func (s MutationServices) Answer(req Decision) (MutationResult, error) {
	return s.decide("answer", req, inbox.DecisionAnswer, nil)
}

// ApproveStore applies Approve through an already-open run writer.
func (s MutationServices) ApproveStore(store *logstore.Store, req Decision) (MutationResult, error) {
	return s.decide("approve", req, inbox.DecisionApprove, store)
}

// RejectStore applies Reject through an already-open run writer.
func (s MutationServices) RejectStore(store *logstore.Store, req Decision) (MutationResult, error) {
	return s.decide("reject", req, inbox.DecisionReject, store)
}

// AnswerStore applies Answer through an already-open run writer.
func (s MutationServices) AnswerStore(store *logstore.Store, req Decision) (MutationResult, error) {
	return s.decide("answer", req, inbox.DecisionAnswer, store)
}

func (s MutationServices) decide(op string, req Decision, decision string, store *logstore.Store) (MutationResult, error) {
	if strings.TrimSpace(req.JobID) == "" || strings.TrimSpace(req.ItemID) == "" {
		return MutationResult{}, serviceError(InvalidArgument, op, req, errors.New("job and item are required"))
	}
	if decision == inbox.DecisionReject && strings.TrimSpace(req.Text) == "" {
		return MutationResult{}, serviceError(InvalidArgument, op, req, errors.New("rejection reason is required"))
	}
	if decision == inbox.DecisionAnswer && strings.TrimSpace(req.Text) == "" {
		return MutationResult{}, serviceError(InvalidArgument, op, req, errors.New("answer text is required"))
	}

	dir, err := s.jobDir(req.JobID)
	if err != nil {
		kind := NotFound
		if errors.Is(err, ErrInvalidArgument) {
			kind = InvalidArgument
		}
		return MutationResult{}, serviceError(kind, op, req, err)
	}
	if store != nil && filepath.Clean(store.Dir()) != filepath.Clean(dir) {
		return MutationResult{}, serviceError(InvalidArgument, op, req,
			fmt.Errorf("supplied store belongs to %s, not job %s", store.Dir(), req.JobID))
	}
	reply := inbox.Reply{Decision: decision, Text: req.Text, Principal: req.Principal}
	if store == nil {
		_, err = inbox.AnswerExact(dir, req.ItemID, reply)
	} else {
		_, err = inbox.DecideExactStore(store, req.ItemID, reply, s.now())
	}
	if err != nil {
		kind := StorageUnavailable
		switch {
		case errors.Is(err, inbox.ErrNoSuchItem):
			kind = NotFound
		case errors.Is(err, inbox.ErrAlreadyAnswered):
			kind = AlreadyDecided
		case errors.Is(err, inbox.ErrRunOver):
			kind = AlreadyTerminal
		case errors.Is(err, inbox.ErrWrongDecisionKind):
			kind = WrongDecisionKind
		case errors.Is(err, inbox.ErrAuthorizationBinding), errors.Is(err, inbox.ErrInvalidPrincipal), errors.Is(err, inbox.ErrAuthorizationExpired):
			kind = InvalidArgument
		default:
			var locked *logstore.LockedError
			if errors.As(err, &locked) {
				kind = Conflict
			}
		}
		return MutationResult{}, serviceError(kind, op, req, err)
	}
	return s.load(op, req.JobID)
}

func serviceError(kind ErrorKind, op string, req Decision, cause error) *Error {
	return &Error{Kind: kind, Op: op, JobID: req.JobID, ItemID: req.ItemID, Cause: cause}
}

func (s MutationServices) jobDir(jobID string) (string, error) {
	id := strings.TrimSpace(jobID)
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return "", fmt.Errorf("%w: invalid job id", ErrInvalidArgument)
	}
	if strings.TrimSpace(s.RunsDir) == "" {
		return "", fmt.Errorf("%w: no run root configured", ErrInvalidArgument)
	}
	dir := filepath.Join(s.RunsDir, id)
	if _, err := os.Stat(logstore.EventsPath(dir)); err != nil {
		return "", err
	}
	return dir, nil
}

func (s MutationServices) load(op, jobID string) (MutationResult, error) {
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
	run, err := runread.Open(dir)
	if err != nil {
		return MutationResult{}, &Error{Kind: StorageUnavailable, Op: op, JobID: jobID, Cause: err}
	}
	recovery, err := exec.Recover(run.Events)
	if err != nil {
		return MutationResult{}, &Error{Kind: StorageUnavailable, Op: op, JobID: jobID, Cause: err}
	}
	return MutationResult{
		State: run.State, Simulated: run.Simulated, UnknownWork: len(recovery.Unknown),
	}, nil
}

func (s MutationServices) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
