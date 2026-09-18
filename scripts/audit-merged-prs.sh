#!/usr/bin/env bash
# Report merged pull requests whose content never reached main.
#
# This exists because the failure it detects has now happened twice, and prose
# did not stop the second occurrence.
#
# GitHub marks a pull request "MERGED" when its merge commit lands on its base
# branch -- not when that content reaches main. In a stacked pair (B based on
# A, both open), merging A into main does NOT re-point B; GitHub only does that
# when A's branch is deleted. Merging B afterwards writes a merge commit onto
# the now-orphaned A, which no longer leads anywhere. Both PRs report success
# and main receives only A.
#
#   #33 / #34 -- stranded this way; rescued later by #35, which diagnosed the
#                mechanism in its own description.
#   #73       -- stranded the same way anyway, while the ADR it implements was
#                already published on main. The decision was visible and the
#                defect it forbids stayed live.
#
# A PR reporting MERGED is therefore not evidence that its code shipped. This
# script asks git instead of asking GitHub: for every merged PR, is the merge
# commit an ancestor of origin/main?
#
# Two outcomes are distinguished, because they are not equally urgent:
#
#   STRANDED  the merge commit is not in main and neither is its content.
#             Work is missing from the product. This fails the script.
#   ORPHANED  the merge commit is not in main but every commit it introduced
#             is reachable from main anyway -- content rescued by a later PR,
#             leaving only a dangling merge node. Reported, does not fail.
#
# The question is always asked against origin/main -- what is actually shipped
# -- never against the local HEAD. So running this from a branch that fixes a
# stranded PR still reports it as stranded, correctly: the fix has not landed
# yet. It clears once that branch is merged.
#
# It audits EVERY merged pull request by default, deliberately. The first
# version defaulted to the 40 most recent, which reported "every verifiable
# merged pull request is in main" while never looking at #30 -- an orphaned PR
# sitting at position 45. A bounded check that presents itself as a complete
# one is the same class of defect this script exists to catch: a boundary that
# looks present and is absent. Passing an explicit limit is still allowed, but
# then the result says so instead of implying full coverage.
#
# Usage:  ./scripts/audit-merged-prs.sh [limit]     (default: all merged PRs)
# Exit:   0 nothing stranded   1 something stranded   2 cannot run

set -uo pipefail

# 0 or "all" means no bound; gh needs a concrete number, so use one above any
# plausible repository size rather than paginating.
LIMIT="${1:-all}"
case "$LIMIT" in
all | 0) GH_LIMIT=100000; BOUNDED=0 ;;
*[!0-9]*) die_early="limit must be a positive integer, 0, or 'all' (got: $LIMIT)" ;;
*) GH_LIMIT="$LIMIT"; BOUNDED=1 ;;
esac
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
ok()   { printf '    \033[32mok\033[0m  %s\n' "$1"; }
bad()  { printf '    \033[31m!!\033[0m  %s\n' "$1"; }
warn() { printf '    \033[33m!\033[0m   %s\n' "$1"; }
die()  { printf '\n\033[31mcannot run:\033[0m %s\n' "$1" >&2; exit 2; }

cd "$REPO_DIR"

[ -n "${die_early:-}" ] && die "$die_early"

command -v gh >/dev/null 2>&1 || die "gh is not installed; this audit reads pull request state from GitHub"
gh auth status >/dev/null 2>&1 || die "gh is not authenticated (run: gh auth login)"

step "fetching main"
git fetch -q origin main || die "could not fetch origin/main"
ok "origin/main at $(git rev-parse --short origin/main)"

if [ "$BOUNDED" -eq 1 ]; then
	step "auditing the $LIMIT most recently merged pull requests"
else
	step "auditing every merged pull request"
fi

numbers="$(gh pr list --state merged --limit "$GH_LIMIT" --json number -q '.[].number')" \
	|| die "could not list merged pull requests"
[ -n "$numbers" ] || die "no merged pull requests returned"
total="$(printf '%s\n' "$numbers" | grep -c .)"

stranded=0
orphaned=0
checked=0

for n in $numbers; do
	fields="$(gh pr view "$n" --json baseRefName,mergeCommit,title \
		-q '[.baseRefName, (.mergeCommit.oid // "none"), .title] | @tsv')" || {
		warn "#$n could not be read; skipped"
		continue
	}
	base="$(printf '%s' "$fields" | cut -f1)"
	oid="$(printf '%s' "$fields" | cut -f2)"
	title="$(printf '%s' "$fields" | cut -f3)"
	checked=$((checked + 1))

	# A squash or rebase merge leaves no merge commit we can trace; say so
	# rather than reporting a clean result we did not establish.
	if [ "$oid" = "none" ]; then
		warn "#$n has no merge commit recorded (squash/rebase merge?) -- not verifiable: $title"
		continue
	fi

	git cat-file -e "$oid^{commit}" 2>/dev/null || git fetch -q origin "$oid" 2>/dev/null
	if ! git cat-file -e "$oid^{commit}" 2>/dev/null; then
		warn "#$n merge commit ${oid:0:8} is unavailable locally -- not verifiable: $title"
		continue
	fi

	if git merge-base --is-ancestor "$oid" origin/main 2>/dev/null; then
		continue
	fi

	# Not in main. Distinguish missing content from a dangling merge node:
	# list the commits this merge introduced that main cannot reach, ignoring
	# the merge commit itself.
	missing="$(git log --format='%H %s' "origin/main..$oid" --no-merges 2>/dev/null)"
	if [ -z "$missing" ]; then
		orphaned=$((orphaned + 1))
		warn "#$n ORPHANED -- merged into '$base', content already in main: $title"
	else
		stranded=$((stranded + 1))
		bad "#$n STRANDED -- merged into '$base', NOT in main: $title"
		printf '%s\n' "$missing" | sed 's/^/          missing: /'
	fi
done

step "result"
printf '    checked %s of %s merged pull requests against origin/main (%s)\n' \
	"$checked" "$total" "$(git rev-parse --short origin/main)"

if [ "$orphaned" -gt 0 ]; then
	printf '    %s orphaned (merge node only, content present)\n' "$orphaned"
fi

# State the coverage, not just the verdict. A bounded run that says only "ok"
# reads as a full audit and is not one.
if [ "$BOUNDED" -eq 1 ]; then
	warn "bounded to the $LIMIT most recent -- older merged pull requests were NOT examined"
fi

if [ "$stranded" -gt 0 ]; then
	bad "$stranded pull request(s) report merged but their content is not in main"
	cat <<'EOF'

    To land stranded work, merge the stranded merge commit into main directly:

        git checkout -b fix/land-stranded origin/main
        git merge --no-ff <stranded-merge-commit>

    Merging (rather than cherry-picking) keeps the original commit identity,
    so the work does not reappear as a new change with a new history.

    To stop it recurring: do not leave a stacked base branch alive after
    merging it. Either merge the stack top-down, or delete each base branch on
    merge so GitHub re-points the pull request above it.
EOF
	exit 1
fi

ok "every verifiable merged pull request is in main"
exit 0
