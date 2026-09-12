package workspacefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/michiTrader/arxi/internal/workspace"
)

type preparationProvisioner struct {
	calls    int
	failAt   int
	sessions map[string]Session
	released int
}

func (p *preparationProvisioner) Provision(_ context.Context, req Request) (Session, error) {
	p.calls++
	if p.calls == p.failAt {
		return nil, errors.New("injected provisioning failure")
	}
	if p.sessions == nil {
		p.sessions = map[string]Session{}
	}
	key := sessionKey(canonicalRequest(req))
	if p.sessions[key] == nil {
		p.sessions[key] = session{id: key}
	}
	return p.sessions[key], nil
}

func (p *preparationProvisioner) Release(_ context.Context, req Request, got Session) error {
	if got == nil || got.Identity() != sessionKey(canonicalRequest(req)) {
		return errors.New("release ownership mismatch")
	}
	p.released++
	return nil
}

func preparationRequest(member string) Request {
	return Request{JobID: "job", Member: member, Mode: workspace.ModeNone,
		ProfileID: workspace.NoToolsProfileID, ProfileIdentity: "profile", ProvisionerVersion: "none-v1",
		Source: workspace.SourceIdentity{Schema: workspace.SchemaV1, Kind: "none"}}
}

func TestPreparationRecordsStartedOnlyAfterProvisionerVerification(t *testing.T) {
	dir := t.TempDir()
	req := preparationRequest("writer")
	provisioner := &preparationProvisioner{failAt: 1}
	if err := Prepare(context.Background(), dir, provisioner, []Request{req}); err == nil {
		t.Fatal("provisioning failure was accepted: started may only claim an external workspace after the provisioner verifies it")
	}
	records, err := readPreparation(filepath.Join(dir, PreparationFile))
	if err != nil || len(records) != 1 || records[0].Phase != "prepared" {
		t.Fatalf("preparation records = %#v / %v: a pre-dispatch failure must remain prepared so recovery cannot mistake intent for external ownership", records, err)
	}
}

func TestPreparationRecoversStartedProvisioningByVerifiedAdoption(t *testing.T) {
	dir := t.TempDir()
	requests := []Request{preparationRequest("a"), preparationRequest("b")}
	first := &preparationProvisioner{failAt: 4}
	if err := Prepare(context.Background(), dir, first, requests); err == nil {
		t.Fatal("injected crash was accepted: preparation must leave a durable started boundary for recovery")
	}
	records, err := readPreparation(filepath.Join(dir, PreparationFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 || records[3].Phase != "finished" || records[4].Phase != "started" {
		t.Fatalf("preparation records = %#v: completed work and the next side-effect boundary must both remain visible", records)
	}
	restarted := &preparationProvisioner{}
	if err := Prepare(context.Background(), dir, restarted, requests); err != nil {
		t.Fatalf("restart could not verify and adopt prepared state: %v", err)
	}
	records, err = readPreparation(filepath.Join(dir, PreparationFile))
	if err != nil || len(records) != 6 || records[len(records)-1].Phase != "finished" {
		t.Fatalf("recovered records = %#v / %v: each request needs one ordered prepared, started, finished history", records, err)
	}
}

func TestAbortPreparationReleasesOnlyRecordedOwnership(t *testing.T) {
	dir := t.TempDir()
	req := preparationRequest("writer")
	provisioner := &preparationProvisioner{}
	if err := Prepare(context.Background(), dir, provisioner, []Request{req}); err != nil {
		t.Fatal(err)
	}
	if err := AbortPreparation(context.Background(), dir, provisioner, []Request{req}); err != nil {
		t.Fatal(err)
	}
	if provisioner.released != 1 {
		t.Fatalf("release calls = %d, want one: rejected acceptance must not orphan a verified external workspace", provisioner.released)
	}
	if _, err := os.Stat(filepath.Join(dir, PreparationFile)); err != nil {
		t.Fatalf("preparation evidence disappeared before the caller removed the unpublished run: %v", err)
	}
}
