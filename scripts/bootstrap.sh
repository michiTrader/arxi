#!/usr/bin/env bash
# Restore a working environment after a sandbox reset.
#
# This exists because the sandbox for this project has been reset repeatedly and
# loses two things every time: the Go toolchain (not preinstalled) and any commit
# that was never pushed. Rebuilding by hand cost a full turn of work once, so the
# recovery is a script instead of a memory exercise.
#
# Safe to run at any time. It never discards uncommitted work: if the tree is
# dirty it says so and stops, because silently resetting over a human's
# in-progress edit is worse than any reset this script recovers from.
#
# Usage:  ./scripts/bootstrap.sh [branch]     (default: genspark_ai_developer)

set -euo pipefail

GO_VERSION="1.22.5"
BRANCH="${1:-genspark_ai_developer}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
ok()   { printf '    \033[32mok\033[0m  %s\n' "$1"; }
warn() { printf '    \033[33m!\033[0m   %s\n' "$1"; }
die()  { printf '\n\033[31mstopped:\033[0m %s\n' "$1" >&2; exit 1; }

cd "$REPO_DIR"

# ---------------------------------------------------------------- Go toolchain
step "Go toolchain"
if command -v go >/dev/null 2>&1 && go version | grep -q "go${GO_VERSION}"; then
	ok "$(go version)"
else
	export PATH="$PATH:/usr/local/go/bin"
	if command -v go >/dev/null 2>&1 && go version | grep -q "go${GO_VERSION}"; then
		ok "$(go version) (already installed, PATH was missing it)"
	else
		warn "installing Go ${GO_VERSION}"
		tarball="go${GO_VERSION}.linux-amd64.tar.gz"
		curl -sSLo "/tmp/${tarball}" "https://go.dev/dl/${tarball}"
		sudo rm -rf /usr/local/go
		sudo tar -C /usr/local -xzf "/tmp/${tarball}"
		rm -f "/tmp/${tarball}"
		ok "$(go version)"
	fi
fi

# PATH is per-shell, so a bare `go` in the next command still fails without this.
if ! grep -q '/usr/local/go/bin' "$HOME/.bashrc" 2>/dev/null; then
	echo 'export PATH=$PATH:/usr/local/go/bin' >> "$HOME/.bashrc"
	ok "added Go to PATH in ~/.bashrc"
fi

# ------------------------------------------------------------------ git state
step "git state"
if [ -n "$(git status --porcelain)" ]; then
	warn "working tree is dirty — NOT touching it"
	git status --short
	echo
	warn "commit or stash first, then re-run. Recovering a reset would discard this."
else
	git fetch origin --quiet
	if git rev-parse --verify --quiet "origin/${BRANCH}" >/dev/null; then
		git checkout -B "$BRANCH" "origin/${BRANCH}" --quiet
		ok "on ${BRANCH} at $(git rev-parse --short HEAD)"
	else
		warn "origin/${BRANCH} does not exist; staying on $(git branch --show-current)"
	fi
fi

unpushed="$(git log --oneline "@{upstream}..HEAD" 2>/dev/null | wc -l | tr -d ' ')"
if [ "$unpushed" != "0" ]; then
	warn "${unpushed} commit(s) not pushed — push now, a local-only commit is as fragile as none"
fi

# -------------------------------------------------------------- verify health
step "build and test"
go build -o /tmp/iash ./cmd/iash
ok "build"

fmt_out="$(gofmt -l .)"
[ -z "$fmt_out" ] || die "gofmt would reformat:\n${fmt_out}"
ok "gofmt"

go vet ./... 2>/dev/null || die "go vet failed"
ok "vet"

if go test -count=1 ./... >/tmp/iash-test.log 2>&1; then
	ok "tests pass ($(grep -c '^ok' /tmp/iash-test.log) packages)"
else
	tail -30 /tmp/iash-test.log
	die "tests failed — see /tmp/iash-test.log"
fi

step "ready"
cat <<'EOF'
    binary:  /tmp/iash
    try:     /tmp/iash why testdata/scenarios/blocked-on-approval.json

    If `go` is not found in a new shell:  export PATH=$PATH:/usr/local/go/bin
EOF
