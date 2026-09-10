package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	hostv1 "github.com/michiTrader/arxi/host/v1"
	"github.com/michiTrader/arxi/internal/job"
	"github.com/michiTrader/arxi/internal/jobstore"
)

const hostLeaseDuration = 30 * time.Second

type hostCoordination struct {
	store jobstore.Store
}

func openHostCoordination(runsRoot string) (*hostCoordination, error) {
	store, err := jobstore.Open(filepath.Join(filepath.Dir(runsRoot), ".arxi", "coordination"), nowFunc)
	if err != nil {
		return nil, err
	}
	return &hostCoordination{store: store}, nil
}

func (c *hostCoordination) BindSubmission(_ context.Context, wanted hostv1.SubmissionBinding) (hostv1.SubmissionBinding, error) {
	for {
		view := c.store.View()
		bound, _, err := c.store.BindSubmission(view.Revision, jobstore.Submission{
			Key: wanted.Key, RequestDigest: job.Digest(wanted.RequestDigest), JobID: job.JobID(wanted.JobID),
		})
		if errors.Is(err, jobstore.ErrRevision) {
			continue
		}
		if err != nil {
			return hostv1.SubmissionBinding{}, err
		}
		return hostv1.SubmissionBinding{Key: bound.Key, RequestDigest: string(bound.RequestDigest), JobID: hostv1.JobID(bound.JobID)}, nil
	}
}

func (c *hostCoordination) Claim(_ context.Context, id hostv1.JobID) (hostv1.ExecutionClaim, error) {
	for {
		view := c.store.View()
		claim, _, err := c.store.Claim(view.Revision, job.JobID(id), "host", hostLeaseDuration)
		if errors.Is(err, jobstore.ErrRevision) {
			continue
		}
		if errors.Is(err, jobstore.ErrConflict) {
			return hostv1.ExecutionClaim{}, hostv1.ErrStorageConflict
		}
		if err != nil {
			return hostv1.ExecutionClaim{}, err
		}
		return publicExecutionClaim(claim), nil
	}
}

func (c *hostCoordination) Validate(_ context.Context, claim hostv1.ExecutionClaim) error {
	view := c.store.View()
	current, ok := view.Claims[job.JobID(claim.JobID)]
	if !ok || current.AttemptID != job.AttemptID(claim.AttemptID) || uint64(current.Fence) != claim.Fence ||
		!nowFunc().Before(current.ExpiresAt) {
		return hostv1.ErrStorageConflict
	}
	return nil
}

func (c *hostCoordination) Heartbeat(_ context.Context, claim hostv1.ExecutionClaim) error {
	for {
		view := c.store.View()
		_, _, err := c.store.Heartbeat(view.Revision, job.JobID(claim.JobID), job.AttemptID(claim.AttemptID), job.Fence(claim.Fence), hostLeaseDuration)
		if errors.Is(err, jobstore.ErrRevision) {
			continue
		}
		return err
	}
}

func (c *hostCoordination) Checkpoint(_ context.Context, checkpoint hostv1.ExecutionCheckpoint) error {
	for {
		view := c.store.View()
		_, err := c.store.Checkpoint(view.Revision, job.Checkpoint{JobID: job.JobID(checkpoint.Claim.JobID),
			AttemptID: job.AttemptID(checkpoint.Claim.AttemptID), Fence: job.Fence(checkpoint.Claim.Fence),
			RunRevision: uint64(checkpoint.RunRevision), CompletedCursor: uint64(checkpoint.CompletedCursor), CreatedAt: nowFunc().UTC()})
		if errors.Is(err, jobstore.ErrRevision) {
			continue
		}
		return err
	}
}

func (c *hostCoordination) Complete(_ context.Context, claim hostv1.ExecutionClaim, outcome hostv1.ExecutionOutcome) error {
	attemptState, jobState := job.AttemptFailed, job.JobFailed
	switch outcome {
	case hostv1.ExecutionSucceeded:
		attemptState, jobState = job.AttemptSucceeded, job.JobSucceeded
	case hostv1.ExecutionCancelled:
		attemptState, jobState = job.AttemptCancelled, job.JobCancelled
	case hostv1.ExecutionUnknown:
		attemptState, jobState = job.AttemptUnknown, job.JobUnknown
	}
	for {
		view := c.store.View()
		_, err := c.store.Complete(view.Revision, jobstore.Completion{JobID: job.JobID(claim.JobID),
			AttemptID: job.AttemptID(claim.AttemptID), Fence: job.Fence(claim.Fence), AttemptState: attemptState, JobState: jobState})
		if errors.Is(err, jobstore.ErrRevision) {
			continue
		}
		return err
	}
}

func (c *hostCoordination) Close() error { return c.store.Close() }

func publicExecutionClaim(claim job.Claim) hostv1.ExecutionClaim {
	return hostv1.ExecutionClaim{JobID: hostv1.JobID(claim.JobID), AttemptID: string(claim.AttemptID), Fence: uint64(claim.Fence)}
}

var _ hostv1.Coordination = (*hostCoordination)(nil)
