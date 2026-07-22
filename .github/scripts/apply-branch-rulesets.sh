#!/usr/bin/env bash
# Creates the development/main branch rulesets that gate the promotion flow
# (auto-pr.yml -> development -> promote.yml -> main). Without these, nothing
# actually stops a direct push or a squash/rebase merge to main — and a
# squash/rebase merge specifically breaks promote.yml's promote-image job,
# which recovers the promoted development commit via `git rev-parse HEAD^2`
# and requires a real two-parent merge commit to exist.
#
# Adapted from ai-cloud-v2's apply-branch-rulesets.sh. This repo has no
# GitHub Environments / live deploy, so (unlike that version) there's no
# `required_deployments` rule here — required status checks are the whole
# gate.
#
# Requires: gh CLI authenticated with repo admin access.
set -euo pipefail

REPO="gojnimer-labs/ai-cloud-operator"

echo "== development ruleset: PR required, 'lint' + 'helm' + 'test' checks required =="
cat <<'EOF' | gh api --method POST "repos/gojnimer-labs/ai-cloud-operator/rulesets" --input - >/dev/null
{
  "name": "development-protection",
  "target": "branch",
  "enforcement": "active",
  "conditions": { "ref_name": { "include": ["refs/heads/development"], "exclude": [] } },
  "rules": [
    { "type": "deletion" },
    { "type": "non_fast_forward" },
    {
      "type": "pull_request",
      "parameters": {
        "required_approving_review_count": 0,
        "dismiss_stale_reviews_on_push": true,
        "require_code_owner_review": false,
        "require_last_push_approval": false,
        "required_review_thread_resolution": false
      }
    },
    {
      "type": "required_status_checks",
      "parameters": {
        "strict_required_status_checks_policy": false,
        "required_status_checks": [
          { "context": "lint" },
          { "context": "helm" },
          { "context": "test" }
        ]
      }
    }
  ]
}
EOF

echo "== main ruleset: PR required, merge commits only, 'build-dev-image' check required =="
cat <<'EOF' | gh api --method POST "repos/gojnimer-labs/ai-cloud-operator/rulesets" --input - >/dev/null
{
  "name": "main-protection",
  "target": "branch",
  "enforcement": "active",
  "conditions": { "ref_name": { "include": ["refs/heads/main"], "exclude": [] } },
  "rules": [
    { "type": "deletion" },
    { "type": "non_fast_forward" },
    {
      "type": "pull_request",
      "parameters": {
        "required_approving_review_count": 0,
        "dismiss_stale_reviews_on_push": true,
        "require_code_owner_review": false,
        "require_last_push_approval": false,
        "required_review_thread_resolution": false,
        "allowed_merge_methods": ["merge"]
      }
    },
    {
      "type": "required_status_checks",
      "parameters": {
        "strict_required_status_checks_policy": false,
        "required_status_checks": [{ "context": "build-dev-image" }]
      }
    }
  ]
}
EOF

echo "Done. Note: this POSTs new rulesets — if development-protection/main-protection"
echo "already exist, delete them first (gh api repos/$REPO/rulesets to find their IDs,"
echo "then --method DELETE repos/$REPO/rulesets/<id>) or this will create duplicates."
