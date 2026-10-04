#!/usr/bin/env bash
# setup-fixture.sh — creates the throwaway repo the smoke runs against, and
# registers it as an AO project.
#
# It CREATES ONLY. It never deletes a project, a worktree, a branch, a run or
# any evidence — not on success, not on failure, not on interrupt. Everything
# this leaves behind is deliberate: a failed smoke is only diagnosable from what
# it left lying around, and a cleanup step that runs on failure destroys exactly
# the state somebody needs to read.
#
# Teardown, when it eventually happens, is a human decision. See TEARDOWN.md.

set -euo pipefail

STAMP="$(date +%Y%m%d-%H%M%S)"
ROOT="${SMOKE_ROOT:-$HOME/.ao/smoke}"
DIR="$ROOT/prux-$STAMP"
PROJECT_ID="ao-smoke-prux-$STAMP"

if [ -e "$DIR" ]; then
	echo "refusing to reuse an existing fixture dir: $DIR" >&2
	exit 1
fi

mkdir -p "$DIR"
cd "$DIR"

git init -q
git config user.email "smoke@local"
git config user.name  "AO smoke"
# Do not inherit a global signing config: this fixture must commit without a key.
git config commit.gpgsign false

# The Verify command. Deliberately trivial and hermetic: no network, no package
# install, nothing that can fail for a reason unrelated to what is under test.
# If this smoke fails, it must be because AO did something wrong.
cat > check.sh <<'EOF'
#!/usr/bin/env bash
# The structured check the smoke asks AO to run.
set -e
test -f smoke.txt
echo "check.sh: smoke.txt present"
EOF
chmod +x check.sh

cat > README.md <<EOF
# AO smoke fixture — product reliability UX

Created $(date -u +%Y-%m-%dT%H:%M:%SZ) by test/smoke/product-reliability-ux.
Throwaway. Not cleaned up automatically, on purpose. See TEARDOWN.md.
EOF

git add -A
git commit -qm "smoke fixture"

echo "fixture path : $DIR"
echo "project id   : $PROJECT_ID"
echo
echo "Register it as an AO project (this is the only mutating command here):"
echo
echo "  ao project add --path '$DIR' --id '$PROJECT_ID' --name 'AO smoke $STAMP'"
echo
echo "Then record both values; run.md needs them."
