package job

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"time"
)

const (
	occurrenceIdentityVersion = "arxi.occurrence/v1"
	attemptIdentityVersion    = "arxi.attempt/v1"
	dispatchIdentityVersion   = "arxi.dispatch/v1"
	requestDigestVersion      = "arxi.request/v1"
	scheduledJobVersion       = "arxi.scheduled-job/v1"
)

// OccurrenceIdentity binds scheduling to the nominal UTC slot rather than the
// scheduler wake time, so delayed and duplicate ticks still name one record.
func OccurrenceIdentity(triggerID TriggerID, nominalAt time.Time) OccurrenceID {
	return OccurrenceID(hashParts(occurrenceIdentityVersion, string(triggerID), canonicalInstant(nominalAt)))
}

// ScheduledJobIdentity binds the one run created for a scheduled occurrence to
// that occurrence. Retries and replacement scheduler processes therefore publish
// or adopt the same run rather than minting another identity.
func ScheduledJobIdentity(occurrenceID OccurrenceID) JobID {
	return JobID(hashParts(scheduledJobVersion, string(occurrenceID)))
}

// AttemptIdentity survives process replacement while distinguishing every
// claim of the same job.
func AttemptIdentity(jobID JobID, attemptNumber uint64) AttemptID {
	return AttemptID(hashParts(attemptIdentityVersion, string(jobID), strconv.FormatUint(attemptNumber, 10)))
}

// RequestDigest binds an idempotency key to already-canonical request bytes.
func RequestDigest(canonicalRequest []byte) Digest {
	return Digest(hashParts(requestDigestVersion, string(canonicalRequest)))
}

// OutcomeDigest binds receipt evidence to already-canonical outcome bytes.
func CanonicalOutcomeDigest(canonicalOutcome []byte) Digest {
	return Digest(hashParts("arxi.outcome/v1", string(canonicalOutcome)))
}

// DispatchIdentity remains stable across attempts because only immutable job,
// work and request identities participate.
func DispatchIdentity(jobID JobID, workID WorkID, requestDigest Digest) DispatchKey {
	return DispatchKey(hashParts(dispatchIdentityVersion, string(jobID), string(workID), string(requestDigest)))
}

func canonicalInstant(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func hashParts(parts ...string) string {
	h := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
