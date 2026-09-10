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

	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/runconfig"
)

type lifecycleStub struct {
	launchErr error
	launched  string
}

func (s *lifecycleStub) Launch(_ context.Context, id string) error {
	s.launched = id
	return s.launchErr
}

func TestSubmitPreparedPublishesExactArtifactAndCallbackBoundary(t *testing.T) {
	root := t.TempDir()
	lifecycle := &lifecycleStub{launchErr: errors.New("after acceptance")}
	cfg := kernel.Config{Blueprint: "worker", Workspace: "copy"}.ResolveDefaults()
	blueprintBytes := []byte("exact bytes\n")
	blueprintSum := sha256.Sum256(blueprintBytes)
	artifact := runconfig.New("r1", "sim", hex.EncodeToString(blueprintSum[:]), "exact prompt", "exact-model", cfg,
		[]runconfig.Route{{Ref: "exact-model", Provider: "p", Protocol: model.ProtocolOpenAIChatCompletions, Model: "frozen", BaseURL: "https://example.test"}}, nil)
	callbackCalled := false
	service := AcceptanceServices{RunsDir: root, Lifecycle: lifecycle}
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
