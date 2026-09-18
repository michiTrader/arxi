#!/usr/bin/env bash
# Run every check that would gate a merge, in one command.
#
# # Why this is a script and not a workflow
#
# It is not a preference. The GitHub App this project is developed through
# cannot write `.github/workflows/`, and that was measured rather than assumed:
#
#   ! [remote rejected] refusing to allow a GitHub App to create or update
#     workflow `.github/workflows/verify.yml` without `workflows` permission
#
# The refusal is scoped to that one directory. Nothing stops a checked-in
# script anywhere else in the tree, which means the situation was never "CI is
# impossible" -- it was "a workflow file is impossible". That distinction went
# unreported for five turns while the checks stayed manual, which is the same
# half-measured-claim shape this repository keeps correcting in its own
# documents: a blocker was verified once at its narrowest point and then
# generalised to the whole capability.
#
# So the runner is missing, not the verification. A human or an agent runs this
# before proposing a merge, and when someone with `workflows` permission adds
# the YAML, it should call this script rather than restate the checks -- two
# copies of a check list is how the list drifts.
#
# # What it runs, and why each one is load-bearing here
#
# gofmt   TestGofmtIsClean exists in-tree, but a formatting failure should stop
#         a merge before the suite spends two minutes proving it.
# vet     It has already caught a real defect in this repository: an fmt.Errorf
#         with a %q and no argument, in memorystore's scope validator.
# build   cmd/arxi must link. A package can compile while the binary does not.
# test    The suite is part of the architecture, not a recommendation: Go gives
#         no exhaustive match, so a switch over Effect missing a variant
#         compiles cleanly and only a test catches it (ADR-0007).
# audit   A pull request reporting MERGED is not evidence that its code
#         shipped. Four have been stranded that way. Opt-in because it needs
#         network and `gh`.
#
# Usage:
#   ./scripts/verify.sh              # gofmt, vet, build, test
#   ./scripts/verify.sh --race       # also run the suite under -race
#   ./scripts/verify.sh --audit      # also audit merged pull requests
#   ./scripts/verify.sh --quick      # skip the suite (formatting and vet only)

set -uo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
ok()   { printf '    \033[32mok\033[0m  %s\n' "$1"; }
bad()  { printf '    \033[31mFAIL\033[0m %s\n' "$1"; }
warn() { printf '    \033[33m!\033[0m   %s\n' "$1"; }

RACE=0
AUDIT=0
QUICK=0
for arg in "$@"; do
	case "$arg" in
	--race) RACE=1 ;;
	--audit) AUDIT=1 ;;
	--quick) QUICK=1 ;;
	-h | --help)
		sed -n '/^# Usage:/,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		printf 'verify: unknown option %s\n' "$arg" >&2
		exit 2
		;;
	esac
done

# Failures are collected rather than fatal, so one run reports everything that
# is wrong. `set -e` would stop at the first failing check and hide the rest,
# which turns fixing three problems into three full runs.
FAILED=()

if ! command -v go >/dev/null 2>&1; then
	if [ -x /usr/local/go/bin/go ]; then
		export PATH="$PATH:/usr/local/go/bin"
	else
		printf '\n\033[31mstopped:\033[0m go is not installed. Run ./scripts/bootstrap.sh first.\n' >&2
		exit 1
	fi
fi

step "toolchain"
ok "$(go version)"

step "gofmt"
# Captured rather than piped to a count: the names of the offending files are
# the actionable part, and `gofmt -l . | wc -l` throws them away.
unformatted="$(gofmt -l . 2>&1)"
if [ -n "$unformatted" ]; then
	bad "not gofmt-clean:"
	printf '        %s\n' $unformatted
	FAILED+=("gofmt")
else
	ok "every file is gofmt-clean"
fi

step "go vet"
if vet_out="$(go vet ./... 2>&1)"; then
	ok "vet found nothing"
else
	bad "vet reported problems:"
	printf '%s\n' "$vet_out" | sed 's/^/        /'
	FAILED+=("vet")
fi

step "build"
if build_out="$(go build -o /tmp/arxi-verify ./cmd/arxi 2>&1)"; then
	ok "cmd/arxi links"
	rm -f /tmp/arxi-verify
else
	bad "build failed:"
	printf '%s\n' "$build_out" | sed 's/^/        /'
	FAILED+=("build")
fi

if [ "$QUICK" -eq 1 ]; then
	warn "--quick: the suite was skipped, so this run is not merge evidence"
else
	step "go test"
	test_log="$(mktemp)"
	if go test -count=1 ./... >"$test_log" 2>&1; then
		ok "$(grep -c '^ok' "$test_log") packages pass"
	else
		bad "the suite failed:"
		# Only the failures, and the assertion text under them. A full log of
		# 39 passing packages buries the one that matters.
		grep -E '^(FAIL|---|\s+[a-z_]+_test\.go:)' "$test_log" | head -40 | sed 's/^/        /'
		FAILED+=("test")
	fi
	rm -f "$test_log"

	if [ "$RACE" -eq 1 ]; then
		step "go test -race"
		race_log="$(mktemp)"
		if go test -race -count=1 ./... >"$race_log" 2>&1; then
			ok "no data races detected"
		else
			bad "the race detector or the suite reported problems:"
			grep -E '^(FAIL|WARNING: DATA RACE|---)' "$race_log" | head -20 | sed 's/^/        /'
			FAILED+=("race")
		fi
		rm -f "$race_log"
	fi
fi

if [ "$AUDIT" -eq 1 ]; then
	step "merged pull request audit"
	if ./scripts/audit-merged-prs.sh >/dev/null 2>&1; then
		ok "every verifiable merged pull request is in main"
	else
		bad "a merged pull request is not in main -- run ./scripts/audit-merged-prs.sh"
		FAILED+=("audit")
	fi
fi

step "result"
if [ "${#FAILED[@]}" -ne 0 ]; then
	bad "failed: ${FAILED[*]}"
	printf '\n    Nothing merges while any of these fails. The test suite in\n'
	printf '    particular is not advisory: Go has no exhaustive match, so the\n'
	printf '    suite is the only thing that catches a missing Effect variant.\n'
	exit 1
fi

if [ "$QUICK" -eq 1 ]; then
	warn "formatting and vet pass; run without --quick before proposing a merge"
	exit 0
fi

ok "every check passed"
exit 0
