#!/usr/bin/env bash
# Creates a correctly-named, correctly-based feature branch so that pushing
# it triggers auto-pr.yml's automatic PR into development. See SKILL.md in
# this directory for the full convention this enforces.
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "Usage: $0 <short-description>" >&2
  echo "Example: $0 fix-admin-table-widths  ->  feature/fix-admin-table-widths" >&2
  exit 1
fi

slug="$1"
branch="feature/${slug}"

if [[ "$slug" == feature/* ]]; then
  echo "error: pass just the description, not the feature/ prefix (got '$slug')" >&2
  exit 1
fi

git fetch origin --quiet
git checkout development
git pull --ff-only
git checkout -b "$branch"

cat <<EOF

On branch '$branch', based on up-to-date development.
Push it once you have a commit:

    git push -u origin $branch

auto-pr.yml will then open (or reuse) a PR from '$branch' into development
automatically — no need to open one by hand.
EOF
