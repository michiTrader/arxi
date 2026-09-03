package kernel

// KnownTools is the closed list of tool names a member may be granted.
//
// It lives in the kernel because MemberConfig.Tools is a kernel field, and the
// legal values of a field belong with the field. Every other home for it was
// tried and each one costs something: in internal/tool it cannot be reached by
// the blueprint loader, which internal/arch_test.go holds to kernel-only
// dependencies for good reasons that have nothing to do with tool names; in the
// loader it cannot be reached by the policy resolver; duplicated in both, the
// two drift and the drift is silent in the direction that matters -- a name the
// loader accepts and the resolver has never heard of resolves to a policy for a
// tool that does not exist.
//
// It is a slice and not a map because two callers want it in order: the loader
// prints it in a refusal, and `agent show` lists it. A map would make both of
// them sort it, and one of them would eventually forget.
//
// Nothing here executes a tool. The kernel names what may be granted; the runner
// in internal/toolrun decides what happens when one is called, and the two must
// agree -- a name in this list with no arm in that switch is a grant that
// resolves to allow and then fails at the call, which is the failure this list
// exists to prevent. TestEveryDeclaredToolHasABodyBehindIt holds them together,
// reaching this list through internal/tool.Known, which is built from it.
var KnownTools = []string{"bash", "edit", "grep", "read", "write"}

// ToolIsKnown reports whether name is a grantable tool.
//
// Linear over five entries, deliberately. The alternative is a package-level map
// built at init, which buys nothing measurable here and costs the property that
// makes KnownTools worth having: one declaration, visible in full, with no
// second copy of it built somewhere else that could disagree.
func ToolIsKnown(name string) bool {
	for _, t := range KnownTools {
		if t == name {
			return true
		}
	}
	return false
}
