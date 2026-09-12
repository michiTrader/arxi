package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/michiTrader/arxi/internal/app"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/model"
	"github.com/michiTrader/arxi/internal/provider"
	"github.com/michiTrader/arxi/internal/runconfig"
	"github.com/michiTrader/arxi/internal/supervisor"
	"github.com/michiTrader/arxi/internal/toolrun"
)

type cliSubmission struct {
	service    app.AcceptanceServices
	supervisor *supervisor.Supervisor
	prepared   app.PreparedSubmission
}

// prepareCLISubmission composes the exact native CLI artifact and executor. It
// intentionally does not pass through host/v1's text-only adapter.
func prepareCLISubmission(f startFlags, bp *blueprint.Blueprint, announce func(string, kernel.Config)) (cliSubmission, error) {
	cfg := bp.Config
	if f.workspace != "" && f.workspace != "auto" {
		cfg.Workspace = f.workspace
	}
	dir := f.dir
	if dir == "" {
		dir = filepath.Join("runs", f.runID)
	}
	artifact, err := effectiveArtifact(f, bp.SHA, cfg)
	if err != nil {
		return cliSubmission{}, fmt.Errorf("resolve the effective run config: %w", err)
	}
	platform := runtime.GOOS
	if f.sim {
		platform = "simulation"
	}
	sup := supervisor.New("runs", supervisor.Options{
		Now: nowFunc,
		Build: func(dir string, effective runconfig.Artifact) (exec.Executor, error) {
			return runtimeExecutor(dir, effective), nil
		},
	})
	prepared := app.PreparedSubmission{
		JobID: f.runID, Actor: bp.Name, Blueprint: bp.Raw, Artifact: artifact,
		BudgetUSD: f.budget, MaxTurns: f.maxTurns, Location: dir,
		OnAccepted: func(_ app.SubmitResult, dir string, cfg kernel.Config) { announce(dir, cfg) },
	}
	return cliSubmission{service: app.AcceptanceServices{RunsDir: "runs", Lifecycle: sup, Platform: platform}, supervisor: sup, prepared: prepared}, nil
}

func submitAndWaitCLI(ctx context.Context, runtime cliSubmission) (string, exec.Outcome, error) {
	submission, err := runtime.service.SubmitPrepared(ctx, runtime.prepared)
	if err != nil {
		_ = runtime.supervisor.Close(ctx)
		return submission.Dir, exec.Outcome{}, err
	}
	waited, waitErr := app.Wait(ctx, submission, app.WaitStandstill)
	closeErr := runtime.supervisor.Close(ctx)
	if waitErr != nil {
		return submission.Dir, waited.Outcome, waitErr
	}
	if closeErr != nil {
		return submission.Dir, waited.Outcome, closeErr
	}
	return submission.Dir, waited.Outcome, waited.Err
}

type frozenResolver map[string]model.Resolution

func (r frozenResolver) Resolve(ref string) (model.Resolution, error) {
	if got, ok := r[ref]; ok {
		return got, nil
	}
	return model.Resolution{}, fmt.Errorf("model ref %q is absent from the run's frozen routes", ref)
}

func effectiveArtifact(f startFlags, bpSHA string, cfg kernel.Config) (runconfig.Artifact, error) {
	mode := "live"
	if f.sim {
		mode = "sim"
		return runconfig.New(f.runID, mode, bpSHA, f.prompt, f.model, cfg, nil, nil), nil
	}

	policies, err := openPolicies().LoadAll()
	if err != nil {
		return runconfig.Artifact{}, err
	}
	store := openProviders()
	providers, err := store.List()
	if err != nil {
		return runconfig.Artifact{}, err
	}

	refs := map[string]bool{}
	for _, member := range cfg.Members {
		ref := member.Model
		if ref == "" {
			ref = f.model
		}
		if ref != "" {
			refs[ref] = true
		}
	}
	ordered := make([]string, 0, len(refs))
	for ref := range refs {
		ordered = append(ordered, ref)
	}
	sort.Strings(ordered)
	routes := make([]runconfig.Route, 0, len(ordered))
	for _, ref := range ordered {
		res, err := model.Resolve(providers, ref)
		if err != nil {
			return runconfig.Artifact{}, err
		}
		price, ok := model.PriceOf(res.Model)
		if !ok {
			return runconfig.Artifact{}, &model.ErrNoPrice{Ref: res.Model}
		}
		routes = append(routes, runconfig.Route{
			Ref: ref, Provider: res.Provider, Protocol: res.Protocol, Model: res.Model,
			BaseURL: res.BaseURL, APIKeyEnv: res.APIKeyEnv, Price: price,
		})
	}
	return runconfig.New(f.runID, mode, bpSHA, f.prompt, f.model, cfg, routes, policies), nil
}

func loadEffectiveForResume(dir, runID string, events []kernel.Event) (runconfig.Artifact, string, error) {
	a, digest, err := runconfig.Load(dir)
	if os.IsNotExist(err) {
		return runconfig.Artifact{}, "", fmt.Errorf(
			"run %s predates %s, so its exact providers, prices, prompt and policies cannot be reconstructed.\n"+
				"  the run remains inspectable, but execution is refused before writing anything",
			runID, runconfig.FileName)
	}
	if err != nil {
		return runconfig.Artifact{}, "", err
	}
	if a.RunID != runID {
		return runconfig.Artifact{}, "", fmt.Errorf("effective config belongs to run %q, not %q", a.RunID, runID)
	}
	started := runStartedEvent(events)
	if started == nil {
		return runconfig.Artifact{}, "", fmt.Errorf("run %s has no run.started event", runID)
	}
	if got := started.Str("effective_config_schema"); got != runconfig.Schema {
		return runconfig.Artifact{}, "", fmt.Errorf("run.started effective_config_schema %q disagrees with %q", got, runconfig.Schema)
	}
	if got := started.Str("effective_config_path"); got != runconfig.FileName {
		return runconfig.Artifact{}, "", fmt.Errorf("run.started effective_config_path %q disagrees with %q", got, runconfig.FileName)
	}
	if got := started.Str("effective_config_sha"); got != digest {
		return runconfig.Artifact{}, "", fmt.Errorf("effective config digest %s disagrees with run.started %s", digest, got)
	}
	if got := started.Str("blueprint_sha"); got != a.BlueprintSHA {
		return runconfig.Artifact{}, "", fmt.Errorf("effective config blueprint_sha %s disagrees with run.started %s", a.BlueprintSHA, got)
	}
	snap, err := os.ReadFile(filepath.Join(dir, "blueprint.snapshot.yaml"))
	if err != nil {
		return runconfig.Artifact{}, "", fmt.Errorf("read the frozen blueprint: %w", err)
	}
	sum := sha256.Sum256(snap)
	if got := hex.EncodeToString(sum[:]); got != a.BlueprintSHA {
		return runconfig.Artifact{}, "", fmt.Errorf("frozen blueprint digest %s disagrees with effective config %s", got, a.BlueprintSHA)
	}
	return a, digest, nil
}

func runStartedEvent(events []kernel.Event) *kernel.Event {
	for i := range events {
		if events[i].Type == kernel.RunStarted {
			return &events[i]
		}
	}
	return nil
}

func runtimeExecutor(dir string, a runconfig.Artifact) exec.Executor {
	if a.Mode == "sim" {
		fake := exec.NewFake()
		if a.SimVersion == runconfig.SimulationNative {
			fake.NativeReadTool = "read"
		}
		return fake
	}
	resolver := frozenResolver{}
	prices := map[string]model.Price{}
	for _, route := range a.Routes {
		resolver[route.Ref] = model.Resolution{
			Provider: route.Provider, Protocol: route.Protocol, Model: route.Model,
			BaseURL: route.BaseURL, APIKeyEnv: route.APIKeyEnv,
		}
		prices[route.Model] = route.Price
	}
	return &provider.Executor{
		Resolver: resolver, DefaultModel: a.DefaultModel, Members: a.Config.Members,
		Prompt: a.Prompt, Prices: prices, ToolPolicy: a.ToolPolicy,
		Tools: &toolrun.Runner{Root: filepath.Join(dir, "workspace"), Shared: a.Config.Workspace == "shared"},
	}
}
