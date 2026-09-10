package v1

// Capability is one Phase 1 operation that can be installed and authorized.
type Capability string

const (
	CapabilitySubmit    Capability = "job.submit"
	CapabilityInspect   Capability = "job.inspect"
	CapabilityCancel    Capability = "job.cancel"
	CapabilityApprove   Capability = "decision.approve"
	CapabilityReject    Capability = "decision.reject"
	CapabilityAnswer    Capability = "decision.answer"
	CapabilityWait      Capability = "job.wait"
	CapabilitySubscribe Capability = "event.subscribe"
)

// CapabilitySet is an immutable-by-convention effective capability snapshot.
// A host returns fresh slices so callers cannot mutate its installed set.
type CapabilitySet struct {
	Capabilities []Capability `json:"capabilities"`
}

// Has reports whether a capability is in the snapshot.
func (s CapabilitySet) Has(want Capability) bool {
	for _, capability := range s.Capabilities {
		if capability == want {
			return true
		}
	}
	return false
}
