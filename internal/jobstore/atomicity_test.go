package jobstore

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMultiRecordApplyIsPreflightedBeforePublishing(t *testing.T) {
	memory := NewMemory(func() time.Time { return time.Unix(0, 0).UTC() })
	valid := record{Kind: kindSubmission, Data: encodeData(Submission{Key: "key", RequestDigest: "digest", JobID: "job"})}
	invalid := record{Kind: kindReservation, Data: json.RawMessage(`{"future":true}`)}
	memory.mu.Lock()
	_, err := memory.commit([]record{valid, invalid})
	memory.mu.Unlock()
	if err == nil {
		t.Fatal("invalid second record committed: a generated transaction must be fully applicable before any projection is published")
	}
	view := memory.View()
	if view.Revision != 0 || len(view.Submissions) != 0 || len(view.Jobs) != 0 {
		t.Fatalf("failed multi-record transaction left revision %d, submissions %#v, jobs %#v: partial in-memory publication disagrees with journal atomicity", view.Revision, view.Submissions, view.Jobs)
	}
}
