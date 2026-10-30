#!/usr/bin/env bash
# Repo hygiene invariants (executable spec):
#   - Apache-2.0 LICENSE present
#   - README carries the unofficial / not-affiliated-with-Amazon disclaimer
#   - thoughts/ is ignored and never tracked
#   - no tracked file looks like a bundled SDK binary (>1MB blob)
# Reports ALL violations before exiting non-zero.
set -uo pipefail
cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)"

failures=0
fail() { echo "repo-lint: FAIL: $*" >&2; failures=$((failures+1)); }

if [ -s LICENSE ] && grep -q "Apache License" LICENSE && grep -q "Version 2.0, January 2004" LICENSE; then
	:
else
	fail "LICENSE missing, empty, or not Apache-2.0"
fi

if [ -f README.md ] && grep -qi "not affiliated with" README.md && grep -qi "amazon" README.md; then
	:
else
	fail "README lacks the unofficial/not-affiliated-with-Amazon disclaimer"
fi

if git rev-parse --git-dir >/dev/null 2>&1; then
	if [ -n "$(git ls-files thoughts/ 2>/dev/null)" ]; then
		fail "thoughts/ must never be tracked"
	fi
else
	fail "not a git repository"
fi

if grep -q "^thoughts/$" .gitignore 2>/dev/null; then
	:
else
	fail ".gitignore must exclude thoughts/"
fi

# No tracked blobs over 1 MiB (would indicate bundled binaries, e.g. SDK parts).
big=$(git ls-files -z 2>/dev/null | xargs -0 -r stat -c '%s %n' 2>/dev/null | awk '$1 > 1048576 {print}')
if [ -n "$big" ]; then
	fail "tracked files exceed 1 MiB (no bundled binaries allowed): $big"
fi

if [ "$failures" -gt 0 ]; then
	exit 1
fi
echo "repo-lint: OK"
