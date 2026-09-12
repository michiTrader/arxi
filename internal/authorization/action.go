package authorization

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const actionVersion = "arxi.authorization-action/v1"

type ActionInput struct {
	JobID                 string
	RunID                 string
	RequesterPrincipal    string
	SuspendedParentWorkID string
	ProviderCallID        string
	ToolName              string
	ArgumentDigest        string
	ToolSchemaVersion     string
	PolicyVersion         string
	WorkspaceProfileID    string
}

type Action struct {
	version            string
	jobID              string
	runID              string
	requesterPrincipal string
	parentWorkID       string
	providerCallID     string
	toolName           string
	argumentDigest     string
	toolSchemaVersion  string
	policyVersion      string
	workspaceProfileID string
	digest             string
}

func NewAction(input ActionInput) (Action, error) {
	fields := []struct {
		name  string
		value string
	}{
		{"job id", input.JobID},
		{"run id", input.RunID},
		{"requester principal", input.RequesterPrincipal},
		{"suspended parent work id", input.SuspendedParentWorkID},
		{"provider call id", input.ProviderCallID},
		{"tool name", input.ToolName},
		{"tool schema version", input.ToolSchemaVersion},
		{"policy version", input.PolicyVersion},
		{"workspace profile id", input.WorkspaceProfileID},
	}
	for _, field := range fields {
		if field.value == "" {
			return Action{}, fmt.Errorf("%s is required", field.name)
		}
	}
	if !validDigest(input.ArgumentDigest) {
		return Action{}, fmt.Errorf("argument digest must be 64 lowercase hexadecimal characters")
	}

	action := Action{
		version:            actionVersion,
		jobID:              input.JobID,
		runID:              input.RunID,
		requesterPrincipal: input.RequesterPrincipal,
		parentWorkID:       input.SuspendedParentWorkID,
		providerCallID:     input.ProviderCallID,
		toolName:           input.ToolName,
		argumentDigest:     input.ArgumentDigest,
		toolSchemaVersion:  input.ToolSchemaVersion,
		policyVersion:      input.PolicyVersion,
		workspaceProfileID: input.WorkspaceProfileID,
	}
	action.digest = digestParts(
		action.version,
		action.jobID,
		action.runID,
		action.requesterPrincipal,
		action.parentWorkID,
		action.providerCallID,
		action.toolName,
		action.argumentDigest,
		action.toolSchemaVersion,
		action.policyVersion,
		action.workspaceProfileID,
	)
	return action, nil
}

func (a Action) Digest() string {
	return a.digest
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	return value == hex.EncodeToString(decoded)
}

func digestParts(parts ...string) string {
	h := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
