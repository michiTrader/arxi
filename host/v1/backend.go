package v1

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/runconfig"
)

type storageBackend struct {
	storage      JobStorage
	coordination Coordination
	heartbeat    time.Duration
	provider     TextProvider
	now          func() time.Time
	capabilities *capabilityResolver

	mu      sync.Mutex
	workers map[JobID]*storageWorker
	closed  bool
}

type storedJobMetadata struct {
	Effective runconfig.Artifact `json:"effective"`
	Simulated bool               `json:"simulated"`
}

func newBackend(options Options) backend {
	installed := map[Capability]any{}
	if options.Storage != nil {
		for _, capability := range []Capability{
			CapabilityInspect, CapabilityCancel, CapabilityApprove, CapabilityReject,
			CapabilityAnswer, CapabilitySubscribe,
		} {
			installed[capability] = capability
		}
		if options.Provider != nil {
			if options.Coordination == nil {
				installed[CapabilitySubmit], installed[CapabilityWait] = CapabilitySubmit, CapabilityWait
			} else if _, safe := options.Storage.(CoordinatedJobStorageV1); safe {
				installed[CapabilitySubmit], installed[CapabilityWait] = CapabilitySubmit, CapabilityWait
				installed[CapabilityRecover] = CapabilityRecover
			}
		}
	}
	resolver, err := newCapabilityResolver(installed, options.Authorizer)
	if err != nil {
		panic(err)
	}
	heartbeat := options.CoordinationHeartbeat
	if heartbeat <= 0 {
		heartbeat = 10 * time.Second
	}
	return &storageBackend{
		storage: options.Storage, coordination: options.Coordination, heartbeat: heartbeat,
		provider: options.Provider, now: options.Now, capabilities: resolver, workers: map[JobID]*storageWorker{},
	}
}

func (b *storageBackend) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if err := b.authorize(ctx, req.Principal, CapabilitySubmit, ""); err != nil {
		return SubmitResult{}, err
	}
	if b.storage == nil || b.provider == nil {
		return SubmitResult{}, unavailable(CapabilitySubmit)
	}
	if err := ctx.Err(); err != nil {
		return SubmitResult{}, err
	}
	if strings.TrimSpace(req.Prompt) == "" || req.BudgetUSD <= 0 || req.MaxTurns < 0 {
		return SubmitResult{}, invalidArgument(CapabilitySubmit, "prompt and a positive budget are required")
	}
	bp, err := blueprint.Load([]byte(req.Blueprint))
	if err != nil {
		return SubmitResult{}, invalidArgument(CapabilitySubmit, "invalid blueprint: "+err.Error())
	}
	id := JobID(newStorageJobID(b.clock()))
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = bp.Name
	}
	requestDigest, err := canonicalSubmitDigest(req, actor, bp.SHA)
	if err != nil {
		return SubmitResult{}, invalidArgument(CapabilitySubmit, "canonicalize submission: "+err.Error())
	}
	if key := strings.TrimSpace(req.IdempotencyKey); key != "" {
		if b.coordination == nil {
			return SubmitResult{}, invalidArgument(CapabilitySubmit, "idempotency key requires durable coordination")
		}
		bound, bindErr := b.coordination.BindSubmission(ctx, SubmissionBinding{
			Key: key, RequestDigest: requestDigest, JobID: id,
		})
		if bindErr != nil {
			return SubmitResult{}, adaptCoordinationError(CapabilitySubmit, id, bindErr)
		}
		id = bound.JobID
	}
	if b.coordination != nil {
		if _, safe := b.storage.(CoordinatedJobStorageV1); !safe {
			return SubmitResult{}, unavailable(CapabilitySubmit)
		}
		if registerErr := b.coordination.RegisterJob(ctx, id); registerErr != nil {
			return SubmitResult{}, adaptCoordinationError(CapabilitySubmit, id, registerErr)
		}
	}
	mode := "live"
	if req.Simulated {
		mode = "sim"
	}
	effective := runconfig.New(string(id), mode, bp.SHA, req.Prompt, "host-text", bp.Config, []runconfig.Route{{
		Ref: "host-text", Provider: "host", Model: "host-text",
	}}, nil)
	metadata, err := json.Marshal(storedJobMetadata{Effective: effective, Simulated: req.Simulated})
	if err != nil {
		return SubmitResult{}, adaptStorageError(CapabilitySubmit, id, 0, err)
	}
	start := kernel.Event{ID: "ev-start", Ts: b.clock().UTC().Format(time.RFC3339Nano), Type: kernel.RunStarted,
		Scope: "run:" + string(id), Source: kernel.SourceHuman, Payload: map[string]any{
			"run_id": string(id), "actor": actor, "blueprint_sha": bp.SHA,
			"budget_usd": req.BudgetUSD, "max_turns": float64(req.MaxTurns),
			"prompt": req.Prompt, "workspace": bp.Config.Workspace, "simulated": req.Simulated,
		}}
	encoded, err := encodeStoredEvent(start)
	if err != nil {
		return SubmitResult{}, adaptStorageError(CapabilitySubmit, id, 0, err)
	}
	created, err := b.storage.Create(ctx, CreateJob{
		Record:    JobRecord{ID: id, Data: metadata},
		Artifacts: []Artifact{{Name: "blueprint", MediaType: "application/yaml", Digest: bp.SHA, Data: append([]byte(nil), bp.Raw...)}},
		Records:   []StoredRecord{{Data: encoded}},
	})
	out := SubmitResult{JobID: id, AcceptedSeq: 1, Status: JobRunning}
	if err != nil && strings.TrimSpace(req.IdempotencyKey) != "" && errors.Is(err, ErrStorageConflict) {
		job, loadErr := b.inspect(ctx, CapabilitySubmit, id)
		if loadErr == nil {
			return SubmitResult{JobID: id, AcceptedSeq: 1, Status: job.Status}, nil
		}
	}
	if err != nil {
		return out, adaptStorageError(CapabilitySubmit, id, 0, err)
	}
	if created.Writer == nil {
		return out, adaptStorageError(CapabilitySubmit, id, 0, errors.New("storage returned no exclusive writer"))
	}
	var executionClaim ExecutionClaim
	if b.coordination != nil {
		_ = created.Writer.Close()
		claim, claimErr := b.coordination.Claim(ctx, id)
		if claimErr != nil {
			return out, adaptCoordinationError(CapabilitySubmit, id, claimErr)
		}
		storage, safe := b.storage.(CoordinatedJobStorageV1)
		if !safe {
			return out, adaptCoordinationError(CapabilitySubmit, id, errors.New("job storage cannot fence claimed writers"))
		}
		writer, openErr := storage.OpenClaimedWriter(ctx, claim)
		if openErr != nil {
			return out, adaptStorageError(CapabilitySubmit, id, 0, openErr)
		}
		executionClaim = claim
		created.Writer = writer
	}
	worker := newStorageWorker(id, created.Record, created.Writer, b.provider, b.now, start)
	if b.coordination != nil {
		worker.coordination = &workerCoordination{port: b.coordination, claim: executionClaim}
		worker.heartbeat = b.heartbeat
	}
	if err := b.installWorker(worker); err != nil {
		_ = created.Writer.Close()
		return out, adaptStorageError(CapabilitySubmit, id, 0, err)
	}
	worker.start()
	return out, nil
}

func (b *storageBackend) Inspect(ctx context.Context, req InspectRequest) (Job, error) {
	if err := b.authorize(ctx, req.Principal, CapabilityInspect, req.JobID); err != nil {
		return Job{}, err
	}
	return b.inspect(ctx, CapabilityInspect, req.JobID)
}

func (b *storageBackend) inspect(ctx context.Context, op Capability, id JobID) (Job, error) {
	if b.storage == nil {
		return Job{}, unavailable(op)
	}
	record, err := b.storage.Load(ctx, id)
	if err != nil {
		return Job{}, adaptStorageError(op, id, 0, err)
	}
	events, _, err := b.readEvents(ctx, id)
	if err != nil {
		return Job{}, adaptStorageError(op, id, 0, err)
	}
	projection, err := projectStoredJob(record, events)
	if err != nil {
		return Job{}, adaptStorageError(op, id, 0, err)
	}
	return projection, nil
}

func (b *storageBackend) Cancel(ctx context.Context, req CancelRequest) (Job, error) {
	if err := b.authorize(ctx, req.Principal, CapabilityCancel, req.JobID); err != nil {
		return Job{}, err
	}
	return b.mutate(ctx, CapabilityCancel, req.JobID, "", func(events []kernel.Event) (kernel.Event, error) {
		state, _ := kernel.Fold(kernel.State{}, events, kernel.Config{})
		if state.Status.Terminal() {
			return kernel.Event{}, errAlreadyTerminal
		}
		payload := map[string]any{}
		if reason := strings.TrimSpace(req.Reason); reason != "" {
			payload["reason"] = reason
		}
		return kernel.Event{ID: "cancel-" + strconv.FormatInt(state.Seq+1, 10), Type: kernel.RunCancelled,
			Ts: b.clock().UTC().Format(time.RFC3339Nano), Source: kernel.SourceHuman,
			Scope: "run:" + string(req.JobID), Payload: payload}, nil
	})
}

func (b *storageBackend) Approve(ctx context.Context, req ApproveRequest) (Job, error) {
	if err := b.authorize(ctx, req.Principal, CapabilityApprove, req.JobID); err != nil {
		return Job{}, err
	}
	return b.decide(ctx, CapabilityApprove, req.JobID, req.ItemID, "approve", "")
}

func (b *storageBackend) Reject(ctx context.Context, req RejectRequest) (Job, error) {
	if err := b.authorize(ctx, req.Principal, CapabilityReject, req.JobID); err != nil {
		return Job{}, err
	}
	if strings.TrimSpace(req.Reason) == "" {
		return Job{}, mutationError(CodeInvalidArgument, CapabilityReject, req.JobID, req.ItemID, errors.New("rejection reason is required"))
	}
	return b.decide(ctx, CapabilityReject, req.JobID, req.ItemID, "reject", req.Reason)
}

func (b *storageBackend) Answer(ctx context.Context, req AnswerRequest) (Job, error) {
	if err := b.authorize(ctx, req.Principal, CapabilityAnswer, req.JobID); err != nil {
		return Job{}, err
	}
	if strings.TrimSpace(req.Text) == "" {
		return Job{}, mutationError(CodeInvalidArgument, CapabilityAnswer, req.JobID, req.ItemID, errors.New("answer text is required"))
	}
	return b.decide(ctx, CapabilityAnswer, req.JobID, req.ItemID, "answer", req.Text)
}

func (b *storageBackend) decide(ctx context.Context, op Capability, id JobID, itemID ItemID, decision, text string) (Job, error) {
	return b.mutate(ctx, op, id, itemID, func(events []kernel.Event) (kernel.Event, error) {
		state, _ := kernel.Fold(kernel.State{}, events, kernel.Config{})
		for _, item := range state.Inbox {
			if item.ID != string(itemID) {
				continue
			}
			if item.Replied {
				return kernel.Event{}, errAlreadyDecided
			}
			approval := item.Kind == "tool_approval"
			if approval != (decision == "approve" || decision == "reject") {
				return kernel.Event{}, errWrongDecisionKind
			}
			if state.Status.Terminal() {
				return kernel.Event{}, errAlreadyTerminal
			}
			return kernel.Event{ID: "inbox-reply-" + string(itemID), Type: kernel.InboxReplied,
				Ts: b.clock().UTC().Format(time.RFC3339Nano), Source: kernel.SourceHuman,
				Payload: map[string]any{"inbox_id": string(itemID), "decision": decision, "text": text}}, nil
		}
		return kernel.Event{}, errItemNotFound
	})
}

func (b *storageBackend) mutate(ctx context.Context, op Capability, id JobID, itemID ItemID,
	makeEvent func([]kernel.Event) (kernel.Event, error)) (Job, error) {
	if b.storage == nil {
		return Job{}, unavailable(op)
	}
	if worker := b.worker(id); worker != nil {
		err := worker.command(ctx, func(events []kernel.Event) (kernel.Event, error) { return makeEvent(events) })
		if err != nil {
			return Job{}, adaptMutationFailure(op, id, itemID, err)
		}
		return b.inspect(ctx, op, id)
	}
	writer, err := b.storage.OpenWriter(ctx, id)
	if err != nil {
		return Job{}, adaptStorageError(op, id, 0, err)
	}
	defer writer.Close()
	record, err := b.storage.Load(ctx, id)
	if err != nil {
		return Job{}, adaptStorageError(op, id, 0, err)
	}
	events, _, err := b.readEvents(ctx, id)
	if err != nil {
		return Job{}, adaptStorageError(op, id, 0, err)
	}
	event, err := makeEvent(events)
	if err != nil {
		return Job{}, adaptMutationFailure(op, id, itemID, err)
	}
	encoded, err := encodeStoredEvent(event)
	if err != nil {
		return Job{}, adaptStorageError(op, id, 0, err)
	}
	if _, err = writer.Append(ctx, AppendBatch{Expected: record.Revision, Records: []StoredRecord{{Data: encoded}}}); err != nil {
		return Job{}, adaptStorageError(op, id, 0, err)
	}
	return b.inspect(ctx, op, id)
}

func (b *storageBackend) Wait(ctx context.Context, req WaitRequest) (Job, error) {
	if err := b.authorize(ctx, req.Principal, CapabilityWait, req.JobID); err != nil {
		return Job{}, err
	}
	if b.storage == nil || b.provider == nil {
		return Job{}, unavailable(CapabilityWait)
	}
	for {
		job, err := b.inspect(ctx, CapabilityWait, req.JobID)
		if err != nil || job.Terminal {
			return job, err
		}
		if worker := b.worker(req.JobID); worker != nil {
			if err := worker.wait(ctx); err != nil {
				return Job{}, err
			}
			continue
		}
		if b.coordination != nil {
			claimed, claimErr := b.claimWorker(ctx, req.JobID)
			if claimErr == nil {
				if err := claimed.wait(ctx); err != nil && !errors.Is(err, errAlreadyTerminal) {
					return Job{}, adaptCoordinationError(CapabilityWait, req.JobID, err)
				}
				continue
			}
			if isCoordinationConflict(claimErr) {
				timer := time.NewTimer(25 * time.Millisecond)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return Job{}, ctx.Err()
				case <-timer.C:
					continue
				}
			}
			return Job{}, adaptCoordinationError(CapabilityWait, req.JobID, claimErr)
		}
		return Job{}, adaptStorageError(CapabilityWait, req.JobID, job.Sequence,
			errors.New("job is not resident in this host"))
	}
}

func (b *storageBackend) Subscribe(ctx context.Context, req SubscribeRequest) (Subscription, error) {
	if err := b.authorize(ctx, req.Principal, CapabilitySubscribe, req.JobID); err != nil {
		return nil, err
	}
	if b.storage == nil {
		return nil, unavailable(CapabilitySubscribe)
	}
	if req.AfterSeq < 0 {
		return nil, invalidArgument(CapabilitySubscribe, "after sequence must not be negative")
	}
	if _, err := b.storage.Load(ctx, req.JobID); err != nil {
		return nil, adaptStorageError(CapabilitySubscribe, req.JobID, req.AfterSeq, err)
	}
	return &storageSubscription{storage: b.storage, jobID: req.JobID, after: req.AfterSeq,
		filter: cloneEventFilter(req.Filter), closed: make(chan struct{})}, nil
}

func (b *storageBackend) Capabilities(ctx context.Context, req CapabilitiesRequest) (CapabilitySet, error) {
	snapshot, err := b.capabilities.snapshot(ctx, req.Principal)
	if err != nil {
		return CapabilitySet{}, newError(CodeInternal, "capabilities", "authorization failed", err)
	}
	return snapshot.set(), nil
}

func (b *storageBackend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	workers := make([]*storageWorker, 0, len(b.workers))
	for _, worker := range b.workers {
		workers = append(workers, worker)
	}
	b.mu.Unlock()
	var closeErr error
	for _, worker := range workers {
		if err := worker.close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	if b.coordination != nil {
		if err := b.coordination.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (b *storageBackend) readEvents(ctx context.Context, id JobID) ([]kernel.Event, Revision, error) {
	var out []kernel.Event
	var after int64
	var continuation Continuation
	var revision Revision
	for {
		batch, err := b.storage.ReadConfirmed(ctx, id, ConfirmedRead{
			AfterSequence: after, Continuation: continuation, Limit: 256,
		})
		if err != nil {
			return nil, revision, err
		}
		for _, record := range batch.Records {
			event, err := decodeStoredEvent(record)
			if err != nil {
				return nil, revision, err
			}
			out = append(out, event)
		}
		if batch.AfterSequence < after {
			return nil, revision, errors.New("storage confirmed sequence moved backwards")
		}
		after, continuation, revision = batch.AfterSequence, batch.Continuation, batch.Revision
		if batch.End {
			return out, revision, nil
		}
		if len(batch.Records) == 0 && continuation == "" {
			return nil, revision, errors.New("storage returned a non-terminal batch without progress")
		}
	}
}

func (b *storageBackend) authorize(ctx context.Context, principal Principal, capability Capability, id JobID) error {
	snapshot, err := b.capabilities.snapshot(ctx, principal)
	if err != nil {
		return newError(CodeInternal, string(capability), "authorization failed", err)
	}
	_, code, err := snapshot.resolve(ctx, capability, id)
	if code == "" {
		return nil
	}
	message := "permission denied"
	if code == CodeNotFound {
		message = "resource not found"
	} else if code == CodeCapabilityUnavailable {
		message = "capability is not installed"
	}
	out := newError(code, string(capability), message, err)
	out.JobID = id
	return out
}

func (b *storageBackend) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *storageBackend) claimWorker(ctx context.Context, id JobID) (*storageWorker, error) {
	storage, ok := b.storage.(CoordinatedJobStorageV1)
	if !ok {
		return nil, errors.New("job storage cannot fence claimed writers")
	}
	claim, err := b.coordination.Claim(ctx, id)
	if err != nil {
		return nil, err
	}
	writer, err := storage.OpenClaimedWriter(ctx, claim)
	if err != nil {
		return nil, err
	}
	record, err := b.storage.Load(ctx, id)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	events, _, err := b.readEvents(ctx, id)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	worker, err := newRecoveredStorageWorker(id, record, writer, b.provider, b.now, events,
		&workerCoordination{port: b.coordination, claim: claim}, b.heartbeat)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := b.installWorker(worker); err != nil {
		_ = writer.Close()
		if existing := b.worker(id); existing != nil {
			return existing, nil
		}
		return nil, err
	}
	worker.start()
	return worker, nil
}

func (b *storageBackend) installWorker(worker *storageWorker) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("host is closed")
	}
	if b.workers[worker.id] != nil {
		return errors.New("job already has a resident writer")
	}
	b.workers[worker.id] = worker
	return nil
}

func (b *storageBackend) worker(id JobID) *storageWorker {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.workers[id]
}

func projectStoredJob(record JobRecord, events []kernel.Event) (Job, error) {
	metadata, err := decodeStoredMetadata(record)
	if err != nil {
		return Job{}, err
	}
	state, _ := kernel.Fold(kernel.State{}, events, metadata.Effective.Config)
	out := Job{
		ID: JobID(state.RunID), Actor: state.Actor, Status: JobStatus(state.Status),
		Terminal: state.Status.Terminal(), Sequence: state.Seq, Stage: state.Stage,
		StageIndex: state.StageIndex, Turns: state.Turns, MaxTurns: state.MaxTurns,
		SpentUSD: state.SpentUSD, TreeSpentUSD: state.TreeSpentUSD, BudgetUSD: state.BudgetUSD,
		Simulated: metadata.Simulated, Result: state.Result,
	}
	if out.ID == "" {
		out.ID = record.ID
	}
	for _, member := range state.Members {
		out.Members = append(out.Members, Member{Name: member.Name, Role: member.Role,
			State: string(member.State), Detail: member.Detail, Submitted: member.Submitted,
			Busy: member.Busy(), Runnable: member.Runnable(), SpentUSD: member.SpentUSD, Turns: member.Turns})
	}
	for _, item := range state.Inbox {
		if item.Replied {
			continue
		}
		kind := DecisionQuestion
		if item.Kind == "tool_approval" {
			kind = DecisionApproval
		}
		out.Pending = append(out.Pending, PendingDecision{ID: ItemID(item.ID), Kind: kind,
			Question: item.Question, Actor: item.Agent})
	}
	if recovery, err := exec.Recover(events); err == nil {
		out.UnknownWork = len(recovery.Unknown)
	}
	return out, nil
}

func decodeStoredMetadata(record JobRecord) (storedJobMetadata, error) {
	var metadata storedJobMetadata
	if err := json.Unmarshal(record.Data, &metadata); err != nil {
		return metadata, fmt.Errorf("decode job metadata: %w", err)
	}
	return metadata, nil
}

func encodeStoredEvent(event kernel.Event) (json.RawMessage, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func decodeStoredEvent(record StoredRecord) (kernel.Event, error) {
	var event kernel.Event
	if err := json.Unmarshal(record.Data, &event); err != nil {
		return event, fmt.Errorf("decode confirmed record %d: %w", record.Sequence, err)
	}
	if record.Sequence <= 0 {
		return event, fmt.Errorf("confirmed record has invalid sequence %d", record.Sequence)
	}
	if event.Seq != 0 && event.Seq != record.Sequence {
		return event, fmt.Errorf("confirmed record sequence %d disagrees with event sequence %d", record.Sequence, event.Seq)
	}
	event.Seq = record.Sequence
	return event, nil
}

func canonicalSubmitDigest(req SubmitRequest, actor, blueprintSHA string) (string, error) {
	body, err := json.Marshal(struct {
		Schema       string  `json:"schema"`
		PrincipalID  string  `json:"principal_id"`
		Actor        string  `json:"actor"`
		BlueprintSHA string  `json:"blueprint_sha"`
		Prompt       string  `json:"prompt"`
		BudgetUSD    float64 `json:"budget_usd"`
		MaxTurns     int     `json:"max_turns"`
		Simulated    bool    `json:"simulated"`
	}{"arxi.host.submit/v1", req.Principal.ID, actor, blueprintSHA, req.Prompt, req.BudgetUSD, req.MaxTurns, req.Simulated})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func isCoordinationConflict(err error) bool {
	if errors.Is(err, ErrStorageConflict) {
		return true
	}
	var hostErr *Error
	return errors.As(err, &hostErr) && hostErr.Code == CodeConflict
}

func adaptCoordinationError(op Capability, id JobID, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	out := newError(CodeConflict, string(op), err.Error(), err)
	out.JobID = id
	return out
}

func newStorageJobID(now time.Time) string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err == nil {
		return "r" + strconv.FormatInt(now.UTC().UnixMilli(), 36) + "-" + hex.EncodeToString(suffix[:])
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(now.UnixNano(), 10)))
	return "r" + strconv.FormatInt(now.UTC().UnixMilli(), 36) + "-" + hex.EncodeToString(sum[:4])
}

var (
	errAlreadyTerminal   = errors.New("job is already terminal")
	errAlreadyDecided    = errors.New("item is already decided")
	errWrongDecisionKind = errors.New("decision verb does not match item kind")
	errItemNotFound      = errors.New("item was not found")
)

func adaptMutationFailure(op Capability, id JobID, itemID ItemID, err error) error {
	code := CodeStorageUnavailable
	switch {
	case errors.Is(err, errAlreadyTerminal):
		code = CodeAlreadyTerminal
	case errors.Is(err, errAlreadyDecided):
		code = CodeAlreadyDecided
	case errors.Is(err, errWrongDecisionKind):
		code = CodeWrongDecisionKind
	case errors.Is(err, errItemNotFound):
		code = CodeNotFound
	}
	return mutationError(code, op, id, itemID, err)
}

func mutationError(code ErrorCode, op Capability, id JobID, itemID ItemID, err error) error {
	out := newError(code, string(op), err.Error(), err)
	out.JobID, out.ItemID = id, itemID
	return out
}

func adaptStorageError(op Capability, id JobID, after int64, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	code := CodeStorageUnavailable
	if errors.Is(err, ErrJobNotFound) {
		code = CodeNotFound
	} else if errors.Is(err, ErrStorageConflict) {
		code = CodeConflict
	}
	out := newError(code, string(op), err.Error(), err)
	out.JobID, out.AfterSeq = id, after
	return out
}

func invalidArgument(op Capability, message string) error {
	return newError(CodeInvalidArgument, string(op), message, nil)
}

func clonePrincipal(principal Principal) Principal {
	out := Principal{ID: principal.ID}
	if principal.Attributes != nil {
		out.Attributes = make(map[string]string, len(principal.Attributes))
		for key, value := range principal.Attributes {
			out.Attributes[key] = value
		}
	}
	return out
}

func adaptReadError(operation string, jobID JobID, after int64, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	out := newError(CodeStorageUnavailable, operation, err.Error(), err)
	out.JobID, out.AfterSeq = jobID, after
	return out
}

func unavailable(capability Capability) error {
	return newError(CodeCapabilityUnavailable, string(capability), "capability is not installed", nil)
}
