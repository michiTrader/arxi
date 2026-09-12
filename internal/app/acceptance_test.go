package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/job"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/runconfig"
)

type lifecycleStub struct {
	launchErr error
	launched  string
	launches  int
}

func (s *lifecycleStub) Launch(_ context.Context, id string) error {
	s.launched = id
	s.launches++
	return s.launchErr
}

type submissionCoordinatorStub struct {
	binding SubmissionBinding
	err     error
	calls   int
}

func (s *submissionCoordinatorStub) BindSubmission(value SubmissionBinding) (SubmissionBinding, error) {
	s.calls++
	if s.err != nil {
		return SubmissionBinding{}, s.err
	}
	if s.binding.Key == "" {
		s.binding = value
	}
	if s.binding.Key != value.Key || s.binding.RequestDigest != value.RequestDigest {
		return SubmissionBinding{}, ErrSubmissionConflict
	}
	return s.binding, nil
}

func TestPreparedWorkspaceCrashReconcilesBeforeAcceptance(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blueprintBytes := []byte("name: worker\n")
	sum := sha256.Sum256(blueprintBytes)
	artifact := runconfig.New("r1", "sim", hex.EncodeToString(sum[:]), "work", "", kernel.Config{Blueprint: "worker"}.ResolveDefaults(), nil, nil)
	prepared := 0
	callback := false
	lifecycle := &lifecycleStub{launchErr: errors.New("stop after recovery")}
	service := AcceptanceServices{RunsDir: root, Lifecycle: lifecycle}
	submission, err := service.SubmitPrepared(context.Background(), PreparedSubmission{
		JobID: "r1", Actor: "worker", Blueprint: blueprintBytes, Artifact: artifact, BudgetUSD: 1,
		Prepare:    func(context.Context, string) error { prepared++; return nil },
		OnAccepted: func(SubmitResult, string, kernel.Config) { callback = true },
	})
	if err == nil || prepared != 1 || submission.Result.AcceptedSeq != 1 || !callback || lifecycle.launches != 1 {
		t.Fatalf("submission/error/prepares/callback/launches = %#v/%v/%d/%v/%d: a preaccept crash must reconcile preparation and publish exactly one accepted run before lifecycle dispatch", submission, err, prepared, callback, lifecycle.launches)
	}
	run, inspectErr := NewReadService(root).Inspect(context.Background(), "r1")
	if inspectErr != nil || run.Sequence != 1 {
		t.Fatalf("reconciled run = %#v/%v: preparation recovery must publish one confirmed run.started event", run, inspectErr)
	}
}

func TestPrepareFailureLeavesNoAcceptedRunCallbackOrLaunch(t *testing.T) {
	root := t.TempDir()
	lifecycle := &lifecycleStub{}
	blueprintBytes := []byte("name: worker\n")
	sum := sha256.Sum256(blueprintBytes)
	artifact := runconfig.New("r1", "sim", hex.EncodeToString(sum[:]), "work", "", kernel.Config{Blueprint: "worker"}.ResolveDefaults(), nil, nil)
	callback := false
	prepared := 0
	aborted := 0
	service := AcceptanceServices{RunsDir: root, Lifecycle: lifecycle}
	_, err := service.SubmitPrepared(context.Background(), PreparedSubmission{
		JobID: "r1", Actor: "worker", Blueprint: blueprintBytes, Artifact: artifact, BudgetUSD: 1,
		Prepare:      func(context.Context, string) error { prepared++; return errors.New("workspace unavailable") },
		AbortPrepare: func(context.Context, string) error { aborted++; return nil },
		OnAccepted:   func(SubmitResult, string, kernel.Config) { callback = true },
	})
	if err == nil || prepared != 1 || aborted != 1 {
		t.Fatalf("error/prepared/aborted = %v/%d/%d: failed provisioning must be attempted once and cleaned before acceptance", err, prepared, aborted)
	}
	if callback || lifecycle.launches != 0 {
		t.Fatalf("callback/launches = %v/%d: no accepted callback or provider-capable lifecycle may run after preparation failure", callback, lifecycle.launches)
	}
	if _, statErr := os.Stat(filepath.Join(root, "r1")); !os.IsNotExist(statErr) {
		t.Fatalf("failed preparation left an accepted run directory: %v", statErr)
	}
}

func TestWorkspacePreflightRejectsBeforePublishingOrLaunching(t *testing.T) {
	for _, test := range []struct {
		name      string
		blueprint string
		want      string
	}{
		{name: "copy provisioner", blueprint: "name: worker\nworkspace: copy\nmembers:\n  - {name: writer, tools: [write]}\n", want: "copy"},
		{name: "worktree provisioner", blueprint: "name: worker\nworkspace: worktree\nmembers:\n  - {name: writer, tools: [write]}\n", want: "worktree"},
		{name: "process containment", blueprint: "name: worker\nworkspace: shared\nmembers:\n  - {name: shell, tools: [bash]}\n", want: "shared"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			lifecycle := &lifecycleStub{}
			service := AcceptanceServices{RunsDir: root, Lifecycle: lifecycle, Platform: "linux", NewID: func() string { return "refused" }}
			_, err := service.Submit(context.Background(), SubmitRequest{Blueprint: []byte(test.blueprint), Prompt: "work", BudgetUSD: 1, Simulated: true})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("preflight error = %v, want %q: unsupported guarantees must be named before acceptance so the operator can select a realizable contract", err, test.want)
			}
			if lifecycle.launches != 0 {
				t.Fatalf("lifecycle launched %d times after workspace refusal: provider dispatch can spend or mutate before the promised workspace exists", lifecycle.launches)
			}
			entries, readErr := os.ReadDir(root)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("run root after workspace refusal = %v / %v: preflight must happen before a run directory, log, or effective artifact becomes visible", entries, readErr)
			}
		})
	}
}

func TestAcceptedTextOnlyRunFreezesNoToolsProfileForAuthorization(t *testing.T) {
	root := t.TempDir()
	lifecycle := &lifecycleStub{launchErr: errors.New("stop after acceptance")}
	service := AcceptanceServices{RunsDir: root, Lifecycle: lifecycle, Platform: "windows", NewID: func() string { return "text-only" }}
	_, err := service.Submit(context.Background(), SubmitRequest{
		Blueprint: []byte("name: worker\nmembers:\n  - {name: text}\n"), Prompt: "work", BudgetUSD: 1, Simulated: true,
	})
	if err == nil {
		t.Fatal("lifecycle failure was hidden: this test must inspect the artifact after the acceptance boundary")
	}
	artifact, _, loadErr := runconfig.Load(filepath.Join(root, "text-only"))
	if loadErr != nil {
		t.Skipf("native Windows cannot fsync directories in this test environment: %v", loadErr)
	}
	if artifact.WorkspaceContract == nil || artifact.WorkspaceContract.Decisions[0].Platform != "windows" ||
		artifact.WorkspaceProfileID != artifact.WorkspaceContract.Decisions[0].ProfileIdentity {
		t.Fatalf("frozen workspace identity = profile %q contract %#v: exact authorization must bind the selected profile and platform decision, not the legacy label", artifact.WorkspaceProfileID, artifact.WorkspaceContract)
	}
}

func TestSubmitPreparedPublishesExactArtifactAndCallbackBoundary(t *testing.T) {
	root := t.TempDir()
	lifecycle := &lifecycleStub{launchErr: errors.New("after acceptance")}
	cfg := kernel.Config{Blueprint: "worker", Workspace: "copy"}.ResolveDefaults()
	blueprintBytes := []byte("exact bytes\n")
	blueprintSum := sha256.Sum256(blueprintBytes)
	artifact := runconfig.New("r1", "sim", hex.EncodeToString(blueprintSum[:]), "exact prompt", "exact-model", cfg,
		[]runconfig.Route{{Ref: "exact-model", Provider: "p", Protocol: model.ProtocolOpenAIChatCompletions, Model: "frozen", BaseURL: "https://example.test"}}, nil)
	service := AcceptanceServices{RunsDir: root, Lifecycle: lifecycle}
	artifact, err := service.freezeWorkspace(artifact)
	if err != nil {
		t.Fatalf("freeze prepared workspace contract: %v", err)
	}
	callbackCalled := false
	submission, err := service.SubmitPrepared(context.Background(), PreparedSubmission{
		JobID: "r1", Actor: "worker", Blueprint: blueprintBytes, Artifact: artifact,
		BudgetUSD: 2, MaxTurns: 3,
		OnAccepted: func(result SubmitResult, dir string, got kernel.Config) {
			callbackCalled = true
			if result.AcceptedSeq != 1 || dir != filepath.Join(root, "r1") || got.Workspace != "copy" {
				t.Fatalf("callback = %#v, %q, %#v", result, dir, got)
			}
		},
	})
	if err == nil || !callbackCalled || submission.Result.AcceptedSeq != 1 {
		t.Fatalf("submission/error/callback = %#v / %v / %v", submission, err, callbackCalled)
	}
	got, _, loadErr := runconfig.Load(filepath.Join(root, "r1"))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !reflect.DeepEqual(got, artifact) {
		t.Fatalf("published artifact = %#v, want %#v", got, artifact)
	}
	raw, readErr := os.ReadFile(filepath.Join(root, "r1", "blueprint.snapshot.yaml"))
	if readErr != nil || string(raw) != "exact bytes\n" {
		t.Fatalf("snapshot = %q / %v", raw, readErr)
	}
}

func TestSubmitPreparedRejectsBlueprintDigestMismatchBeforePublication(t *testing.T) {
	root := t.TempDir()
	cfg := kernel.Config{Blueprint: "worker", Workspace: "copy"}.ResolveDefaults()
	artifact := runconfig.New("r1", "sim", strings.Repeat("a", 64), "prompt", "", cfg, nil, nil)
	service := AcceptanceServices{RunsDir: root, Lifecycle: &lifecycleStub{}}
	_, err := service.SubmitPrepared(context.Background(), PreparedSubmission{
		JobID: "r1", Blueprint: []byte("different bytes\n"), Artifact: artifact, BudgetUSD: 1,
	})
	var serviceErr *Error
	if !errors.As(err, &serviceErr) || serviceErr.Kind != InvalidArgument {
		t.Fatalf("mismatch error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "r1")); !os.IsNotExist(statErr) {
		t.Fatalf("mismatch published a visible run: %v", statErr)
	}
}

func TestIdempotentSubmissionFaultBoundaries(t *testing.T) {
	blueprintBytes := []byte("name: worker\n")
	newService := func(root string, lifecycle *lifecycleStub, coordinator SubmissionCoordinator) AcceptanceServices {
		return AcceptanceServices{
			RunsDir: root, Lifecycle: lifecycle, Submissions: coordinator,
			NewID: func() string { return "candidate" },
			Now:   func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) },
		}
	}

	t.Run("failure before binding publishes nothing", func(t *testing.T) {
		coordinator := &submissionCoordinatorStub{}
		lifecycle := &lifecycleStub{}
		_, err := newService(t.TempDir(), lifecycle, coordinator).Submit(context.Background(), SubmitRequest{
			Blueprint: []byte("name: [invalid\n"), Prompt: "work", BudgetUSD: 1, IdempotencyKey: "key",
		})
		if err == nil || coordinator.calls != 0 || lifecycle.launches != 0 {
			t.Fatalf("error/bindings/launches = %v/%d/%d: invalid input must fail before durable identity or lifecycle work", err, coordinator.calls, lifecycle.launches)
		}
	})

	t.Run("binding without publication recovers under the bound identity", func(t *testing.T) {
		root := t.TempDir()
		bp, loadErr := blueprint.Load(blueprintBytes)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		artifact := runconfig.New("ignored-candidate", "sim", bp.SHA, "work", "", bp.Config, nil, nil)
		digest, digestErr := submissionDigest("worker", bp.Raw, artifact, 1, 0)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		coordinator := &submissionCoordinatorStub{binding: SubmissionBinding{Key: "key", RequestDigest: digest, JobID: "reserved-job"}}
		service := newService(root, &lifecycleStub{}, coordinator)
		service.NewID = func() string { return "ignored-candidate" }
		result, err := service.Submit(context.Background(), SubmitRequest{
			Blueprint: blueprintBytes, Prompt: "work", BudgetUSD: 1, Simulated: true, IdempotencyKey: "key",
		})
		if err != nil || result.JobID != "reserved-job" {
			t.Fatalf("recovered result/error = %#v/%v: a binding that survived publication must retain its deterministic job identity", result, err)
		}
		if _, statErr := os.Stat(filepath.Join(root, "ignored-candidate")); !os.IsNotExist(statErr) {
			t.Fatalf("candidate run exists after recovery: reservation-to-publication recovery created a second identity: %v", statErr)
		}
	})

	t.Run("binding survives publication gap and retry uses its job", func(t *testing.T) {
		root := t.TempDir()
		coordinator := &submissionCoordinatorStub{}
		failing := &submissionCoordinatorStub{err: errors.New("journal unavailable")}

		_, err := newService(root, &lifecycleStub{}, failing).Submit(context.Background(), SubmitRequest{
			Blueprint: blueprintBytes, Prompt: "work", BudgetUSD: 1, Simulated: true, IdempotencyKey: "key",
		})
		if err == nil {
			t.Fatal("coordination failure was accepted: publication without a durable key binding can create duplicate jobs")
		}
		firstLifecycle := &lifecycleStub{launchErr: errors.New("handoff unavailable")}
		first := newService(root, firstLifecycle, coordinator)

		first.NewID = func() string { return "scheduled-job" }
		result, err := first.Submit(context.Background(), SubmitRequest{
			Blueprint: blueprintBytes, Prompt: "work", BudgetUSD: 1, Simulated: true, IdempotencyKey: "key",
		})
		if err == nil || result.JobID != "scheduled-job" {
			t.Fatalf("first result/error = %#v/%v: publication must remain accepted across lifecycle handoff failure", result, err)
		}
		retryLifecycle := &lifecycleStub{}
		retry := newService(root, retryLifecycle, coordinator)

		retry.NewID = func() string { return "different-candidate" }
		repeated, err := retry.Submit(context.Background(), SubmitRequest{
			Blueprint: blueprintBytes, Prompt: "work", BudgetUSD: 1, Simulated: true, IdempotencyKey: "key",
		})
		if err != nil || repeated.JobID != "scheduled-job" || retryLifecycle.launched != "scheduled-job" {
			t.Fatalf("retry result/error/launch = %#v/%v/%q: the bound published job must be verified and adopted", repeated, err, retryLifecycle.launched)
		}
		projection, inspectErr := NewReadService(root).Inspect(context.Background(), "scheduled-job")
		if inspectErr != nil || projection.Sequence != 1 {
			t.Fatalf("adopted projection/error = %#v/%v: recovery created or appended a second run", projection, inspectErr)
		}
		if coordinator.calls != 2 {
			t.Fatalf("submission binding calls = %d: retry must observe one durable identity rather than creating another", coordinator.calls)
		}

	})

	t.Run("conflicting retry and published artifacts fail closed", func(t *testing.T) {
		root := t.TempDir()
		coordinator := &submissionCoordinatorStub{}
		service := newService(root, &lifecycleStub{}, coordinator)
		if _, err := service.Submit(context.Background(), SubmitRequest{
			Blueprint: blueprintBytes, Prompt: "work", BudgetUSD: 1, Simulated: true, IdempotencyKey: "key",
		}); err != nil {
			t.Fatalf("seed submission failed: %v", err)
		}
		_, err := service.Submit(context.Background(), SubmitRequest{
			Blueprint: blueprintBytes, Prompt: "different", BudgetUSD: 1, Simulated: true, IdempotencyKey: "key",
		})
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Kind != Conflict {
			t.Fatalf("conflicting digest error = %v: key reuse for another request must fail closed", err)
		}
		path := filepath.Join(root, string(coordinator.binding.JobID), "blueprint.snapshot.yaml")
		if writeErr := os.WriteFile(path, []byte("name: attacker\n"), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		_, err = service.Submit(context.Background(), SubmitRequest{
			Blueprint: blueprintBytes, Prompt: "work", BudgetUSD: 1, Simulated: true, IdempotencyKey: "key",
		})
		if !errors.As(err, &appErr) || appErr.Kind != Conflict {
			t.Fatalf("artifact conflict error = %v: a bound job may only be adopted after immutable verification", err)
		}
	})
}

func TestPreparedSubmissionRejectsIncorrectCanonicalDigest(t *testing.T) {
	blueprintBytes := []byte("name: worker\n")
	sum := sha256.Sum256(blueprintBytes)
	artifact := runconfig.New("r1", "sim", hex.EncodeToString(sum[:]), "work", "", kernel.Config{Blueprint: "worker"}.ResolveDefaults(), nil, nil)
	service := AcceptanceServices{RunsDir: t.TempDir(), Lifecycle: &lifecycleStub{}, Submissions: &submissionCoordinatorStub{}}
	_, err := service.SubmitPrepared(context.Background(), PreparedSubmission{
		JobID: "r1", Actor: "worker", Blueprint: blueprintBytes, Artifact: artifact,
		BudgetUSD: 1, IdempotencyKey: "key", RequestDigest: job.Digest("wrong"),
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Kind != Conflict {
		t.Fatalf("incorrect prepared digest error = %v: callers cannot bind noncanonical request content", err)
	}
}

func TestSubmitFaultBoundary(t *testing.T) {
	blueprint := []byte("name: worker\n")
	t.Run("pre-boundary failure leaves no visible job", func(t *testing.T) {
		root := t.TempDir()
		lifecycle := &lifecycleStub{}
		service := AcceptanceServices{RunsDir: root, Lifecycle: lifecycle, NewID: func() string { return "r1" }}
		_, err := service.Submit(context.Background(), SubmitRequest{
			Blueprint: []byte("name: [not valid\n"), Prompt: "work", BudgetUSD: 1,
		})
		if err == nil {
			t.Fatal("invalid blueprint was accepted")
		}
		if _, statErr := os.Stat(filepath.Join(root, "r1")); !os.IsNotExist(statErr) {
			t.Fatalf("pre-boundary run directory remains: %v", statErr)
		}
		if lifecycle.launched != "" {
			t.Fatalf("lifecycle launched %q before acceptance", lifecycle.launched)
		}
	})

	t.Run("post-boundary launch failure retains inspectable job", func(t *testing.T) {
		root := t.TempDir()
		lifecycle := &lifecycleStub{launchErr: errors.New("supervisor unavailable")}
		service := AcceptanceServices{
			RunsDir: root, Lifecycle: lifecycle, NewID: func() string { return "r1" },
			Now: func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) },
		}
		result, err := service.Submit(context.Background(), SubmitRequest{
			Blueprint: blueprint, Prompt: "work", BudgetUSD: 1, Simulated: true,
		})
		if err == nil || result.JobID != "r1" || result.AcceptedSeq != 1 {
			t.Fatalf("result/error = %#v / %v", result, err)
		}
		job, inspectErr := NewReadService(root).Inspect(context.Background(), "r1")
		if inspectErr != nil {
			t.Fatalf("accepted job is not inspectable: %v", inspectErr)
		}
		if job.ID != "r1" || job.Sequence != 1 || job.Status != string(kernel.StatusRunning) {
			t.Fatalf("accepted projection = %#v", job)
		}
		if _, _, loadErr := runconfig.Load(filepath.Join(root, "r1")); loadErr != nil {
			t.Fatalf("effective config was not published: %v", loadErr)
		}
	})
}
