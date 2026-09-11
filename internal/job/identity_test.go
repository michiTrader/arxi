package job

import (
	"strings"
	"testing"
	"time"
)

func TestOccurrenceIdentityUsesCanonicalNominalUTCInstant(t *testing.T) {
	utc := time.Date(2026, 3, 4, 5, 6, 7, 800, time.UTC)
	offset := time.FixedZone("offset", -5*60*60)
	sameInstant := utc.In(offset)

	first := OccurrenceIdentity("nightly", utc)
	second := OccurrenceIdentity("nightly", sameInstant)
	if first != second {
		t.Fatalf("one nominal slot produced two occurrence identities across time zones: duplicate scheduler ticks could create duplicate jobs; canonicalize the supplied instant to UTC before hashing (%s != %s)", first, second)
	}
	if len(first) != 64 {
		t.Fatalf("occurrence identity has %d hexadecimal characters, want 64: stores cannot rely on the promised SHA-256 identity; return the full digest", len(first))
	}
	if first == OccurrenceIdentity("nightly", utc.Add(time.Nanosecond)) {
		t.Fatal("adjacent nominal slots share an occurrence identity: distinct scheduled work would be silently discarded; include the RFC3339Nano nominal instant in the digest")
	}
}

func TestCanonicalIdentitiesFrameFieldsWithoutConcatenationAmbiguity(t *testing.T) {
	left := DispatchIdentity("ab", "c", RequestDigest([]byte("request")))
	right := DispatchIdentity("a", "bc", RequestDigest([]byte("request")))
	if left == right {
		t.Fatal("different job and work boundaries produced one dispatch key: an external result could be attached to the wrong work; length-frame every identity field")
	}

	attemptOne := AttemptIdentity("job", 1)
	attemptTwo := AttemptIdentity("job", 2)
	if attemptOne == attemptTwo {
		t.Fatal("successive claims produced one attempt identity: takeover would be invisible and stale workers could appear current; include the monotonically increasing attempt number")
	}
}

func TestDispatchIdentitySurvivesAttemptsAndBindsRequest(t *testing.T) {
	digest := RequestDigest([]byte(`{"model":"fixed","prompt":"work"}`))
	firstAttempt := DispatchIdentity("job-1", "work-4", digest)
	secondAttempt := DispatchIdentity("job-1", "work-4", digest)
	if firstAttempt != secondAttempt {
		t.Fatal("the same prepared work changed dispatch key across attempts: an idempotent retry could execute twice; derive the key only from job, work and canonical request digest")
	}
	changed := DispatchIdentity("job-1", "work-4", RequestDigest([]byte(`{"model":"fixed","prompt":"other"}`)))
	if firstAttempt == changed {
		t.Fatal("different canonical requests share a dispatch key: a provider could return an outcome for different work; bind the request digest into dispatch identity")
	}
}

func TestDigestHelpersReturnLowercaseSHA256(t *testing.T) {
	for name, digest := range map[string]Digest{
		"request": RequestDigest([]byte("request")),
		"outcome": CanonicalOutcomeDigest([]byte("outcome")),
	} {
		if len(digest) != 64 || digest != Digest(strings.ToLower(string(digest))) {
			t.Errorf("%s digest %q is not lowercase SHA-256: persisted idempotency comparisons could disagree across adapters; return 64 lowercase hexadecimal characters", name, digest)
		}
	}
}

func TestAmountNormalizationHasOneRepresentationPerValue(t *testing.T) {
	if got, want := NewAmount(12300, 4), (Amount{Coefficient: 123, Scale: 2}); got != want {
		t.Fatalf("NewAmount(12300, 4) = %+v, want %+v: equal ledger values could compare unequal and overspend a ceiling; remove trailing decimal zeroes", got, want)
	}
	if NewAmount(0, 8) != (Amount{}) {
		t.Fatal("zero retained a nonzero decimal scale: equal zero reservations could compare unequal; normalize zero to coefficient and scale zero")
	}
	if (Amount{Coefficient: 10, Scale: 2}).Canonical() {
		t.Fatal("an amount with a removable trailing zero claims to be canonical: ledger equality would have multiple encodings; reject non-normalized coefficients")
	}
}
