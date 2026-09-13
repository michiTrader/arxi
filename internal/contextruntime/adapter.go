// Package contextruntime joins the canonical transcript projector and the
// prepared-context preparer to the executor's durable barrier. It exists
// because internal/exec declares the pipeline it needs as an interface: this
// package is the concrete type satisfied at the wiring site, keeping the
// runner's dependency direction pointing downwards.
package contextruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/michiTrader/arxi/internal/contextprep"
	"github.com/michiTrader/arxi/internal/exec"
	"github.com/michiTrader/arxi/internal/transcript"
)

// Adapter implements exec.ContextPipeline with the versioned domain packages.
// Every method is a pure function of its inputs: the same confirmed prefix and
// effect always produce the same bytes, which is what makes the barrier
// reproducible across recovery attempts.
type Adapter struct{}

// Project renders the confirmed prefix into exact transcript artifact bytes.
func (Adapter) Project(req exec.ContextProjection) (exec.ContextTranscript, error) {
	history, err := transcript.Project(req.RunID, req.Subject, req.EffectiveConfigSHA, req.Events, req.Through)
	if err != nil {
		return exec.ContextTranscript{}, err
	}
	body, err := json.Marshal(history)
	if err != nil {
		return exec.ContextTranscript{}, fmt.Errorf("encode transcript artifact: %w", err)
	}
	return exec.ContextTranscript{
		JSON:                 string(body),
		Digest:               byteDigest(body),
		Schema:               history.Schema,
		ProjectorVersion:     history.ProjectorVersion,
		SourceFromSeq:        history.SourceFromSeq,
		SourceThroughEventID: history.SourceThroughEventID,
		ContentDigest:        history.ContentDigest,
	}, nil
}

// Prepare freezes one presentation from a projected transcript.
func (Adapter) Prepare(req exec.ContextPreparation) (exec.PreparedContext, error) {
	var history transcript.Artifact
	if err := json.Unmarshal([]byte(req.History.JSON), &history); err != nil {
		return exec.PreparedContext{}, fmt.Errorf("decode projected transcript: %w", err)
	}
	if history.ContentDigest != req.History.ContentDigest {
		return exec.PreparedContext{}, fmt.Errorf("projected transcript digest %q disagrees with the pipeline binding %q",
			history.ContentDigest, req.History.ContentDigest)
	}
	artifact, err := contextprep.Prepare(req.ContextID, req.RunID, req.ParentWorkID, req.EffectiveConfigSHA, req.Effect, history)
	if err != nil {
		return exec.PreparedContext{}, err
	}
	body, err := json.Marshal(artifact)
	if err != nil {
		return exec.PreparedContext{}, fmt.Errorf("encode prepared-context artifact: %w", err)
	}
	return exec.PreparedContext{
		JSON:                    string(body),
		Digest:                  byteDigest(body),
		Schema:                  artifact.Schema,
		PreparerVersion:         artifact.PreparerVersion,
		ContextID:               artifact.ContextID,
		ParentWorkID:            artifact.ParentWorkID,
		Subject:                 artifact.Subject,
		SourceThroughSeq:        artifact.SourceThroughSeq,
		ContentDigest:           artifact.ContentDigest,
		PresentationDigest:      artifact.PresentationDigest,
		TranscriptContentDigest: artifact.Transcript.ContentDigest,
		Messages:                artifact.Messages,
	}, nil
}

// byteDigest binds the exact artifact bytes the way every other persisted
// digest in this repository does: lowercase hex SHA-256.
func byteDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
