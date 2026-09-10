package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/runconfig"
)

// installSimEffective gives a hand-written run the same immutable execution
// contract produced by `run start --sim`. It returns the run.started fields
// that bind the log to the exact artifact and blueprint bytes.
func installSimEffective(t *testing.T, run, id string, raw []byte) string {
	t.Helper()
	bp, err := blueprint.Load(raw)
	if err != nil {
		t.Fatalf("load fixture blueprint: %v", err)
	}
	a := runconfig.New(id, "sim", bp.SHA, "fixture prompt", "", bp.Config, nil, nil)
	digest, err := runconfig.Publish(run, a)
	if err != nil {
		t.Fatalf("publish fixture effective config: %v", err)
	}
	return `,"blueprint_sha":"` + bp.SHA +
		`","effective_config_schema":"` + runconfig.Schema +
		`","effective_config_path":"` + runconfig.FileName +
		`","effective_config_sha":"` + digest + `"`
}

// installFixtureProgress marks every source event in a hand-written fixture as
// already executed. Tests call it only for logs that claim to be modern and
// resumable; deliberately legacy fixtures must remain without these records.
func installFixtureProgress(t *testing.T, path string) {
	t.Helper()
	rawLog, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawLog), `"type":"exec.step_completed"`) {
		return
	}

	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(rawLog)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		event["seq"] = len(lines) + 1
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))

		typ, _ := event["type"].(string)
		if strings.HasPrefix(typ, "exec.") || typ == "timer.scheduled" ||
			typ == "timer.cancelled" || typ == "timer.fired" {
			continue
		}
		sourceSeq := len(lines)
		sourceID, _ := event["id"].(string)
		marker, err := json.Marshal(map[string]any{
			"seq": sourceSeq + 1, "id": "fixture-step-" + sourceID,
			"type": "exec.step_completed", "source": "runtime",
			"payload": map[string]any{"source_seq": sourceSeq, "source_event_id": sourceID, "work_ids": []string{}},
		})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(marker))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// upgradeFixtureRun converts a legacy hand-written simulated run fixture into a
// modern resumable run without changing its domain history.
func upgradeFixtureRun(t *testing.T, dir, id string) {
	t.Helper()
	run := filepath.Join(dir, "runs", id)
	rawBP, err := os.ReadFile(filepath.Join(run, "blueprint.snapshot.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fields := installSimEffective(t, run, id, rawBP)
	path := filepath.Join(run, "events.ndjson")
	rawLog, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before := string(rawLog)
	after := strings.Replace(before, `"run_id":"`+id+`"`, `"run_id":"`+id+`"`+fields, 1)
	if after == before {
		t.Fatalf("fixture run.started does not carry run_id %q", id)
	}
	if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	installFixtureProgress(t, path)
}

func TestResumeRefusesTamperedExecutionArtifactsBeforeAppending(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, run string)
		want   string
	}{
		{
			name: "effective config bytes",
			tamper: func(t *testing.T, run string) {
				path := filepath.Join(run, runconfig.FileName)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var doc map[string]any
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatal(err)
				}
				doc["prompt"] = "tampered after start"
				raw, err = json.MarshalIndent(doc, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, '\n')
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "effective config digest",
		},
		{
			name: "blueprint bytes",
			tamper: func(t *testing.T, run string) {
				path := filepath.Join(run, "blueprint.snapshot.yaml")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(raw, []byte("# changed\n")...), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "frozen blueprint digest",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := workdir(t)
			id := budgetBlockedRun(t, dir)
			run := filepath.Join(dir, "runs", id)
			logPath := filepath.Join(run, "events.ndjson")
			before, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}

			tc.tamper(t, run)
			got := arxi(t, dir, "run", "unpause", id, "--budget", "5")
			if got.code == 0 {
				t.Fatalf("resume accepted tampered %s:\n%s", tc.name, got.out)
			}
			if !strings.Contains(got.out, tc.want) {
				t.Errorf("refusal does not mention %q:\n%s", tc.want, got.out)
			}
			after, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Error("resume appended after execution-contract validation failed")
			}
			if _, err := os.Stat(filepath.Join(run, "writer.lock")); !os.IsNotExist(err) {
				t.Errorf("resume took or left writer.lock before refusing: %v", err)
			}
		})
	}
}

func TestLegacyResumeIsRefusedWithoutChangingItsLog(t *testing.T) {
	dir := workdir(t)
	id := stagedRun(t, dir)
	path := filepath.Join(dir, "runs", id, "events.ndjson")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	got := arxi(t, dir, "run", "unpause", id)
	if got.code == 0 {
		t.Fatalf("legacy resume was accepted:\n%s", got.out)
	}
	for _, want := range []string{"predates", runconfig.FileName, "remains inspectable"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("legacy refusal does not mention %q:\n%s", want, got.out)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Error("legacy refusal changed events.ndjson")
	}
}

func TestReplayChecksModernConfigButKeepsLegacyInspection(t *testing.T) {
	legacyDir := workdir(t)
	legacy := stagedRun(t, legacyDir)
	if got := arxi(t, legacyDir, "run", "replay", legacy); got.code != 0 {
		t.Fatalf("legacy replay was refused: exit %d\n%s", got.code, got.out)
	}

	modernDir := workdir(t)
	modern := budgetBlockedRun(t, modernDir)
	run := filepath.Join(modernDir, "runs", modern)
	path := filepath.Join(run, runconfig.FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["prompt"] = "tampered after start"
	raw, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	got := arxi(t, modernDir, "run", "replay", modern)
	if got.code == 0 {
		t.Fatalf("modern replay ignored an artifact/log digest mismatch:\n%s", got.out)
	}
	if !strings.Contains(got.out, "effective config digest") {
		t.Errorf("modern replay refusal did not identify the mismatch:\n%s", got.out)
	}
}
