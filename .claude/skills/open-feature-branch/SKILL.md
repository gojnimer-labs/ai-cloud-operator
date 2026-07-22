---
name: open-feature-branch
description: How to open a feature branch in this repo so it auto-generates a PR into development. Use when starting new work, "create a feature branch," "start a new branch," or before the first commit of a change.
---

# Open a feature branch

This repo's pipeline (`.github/workflows/auto-pr.yml`, `promote.yml`, `test.yml`/`lint.yml`/`test-e2e.yml`) auto-generates pull requests for you — but only if the branch is named and based correctly. Get either wrong and the automation silently does nothing; you'll end up opening the PR by hand and wondering why nobody reviewed it.

## The rule

1. **Branch off `development`, not `main`.** `main` only ever receives code via the automated `development` -> `main` promotion PR (see `promote.yml`) — a feature branch based on `main` will diverge and can't merge into `development` cleanly.
2. **Name it `feature/<short-description>`.** `auto-pr.yml` triggers on `push: branches: ["feature/**"]` — anything not under `feature/` (e.g. `fix-typo`, `wip`, `my-branch`) gets no automatic PR at all. `test.yml`/`lint.yml`/`test-e2e.yml` only trigger on `pull_request: branches: [development]`, so without that auto-opened PR you also get no CI feedback until you open one by hand.
3. **Push it.** The first push (with at least one commit ahead of `development`) triggers `auto-pr.yml`, which opens — or, on later pushes, reuses — a PR from your branch into `development`. You never run `gh pr create` yourself for this leg.

Direct pushes to `development`/`main` are blocked by branch rulesets anyway (`.github/scripts/apply-branch-rulesets.sh` is the source of truth for what those require) — a `feature/*` branch + PR is the only path in.

Once you're on the branch, see the `conventional-commits` skill for how to write the commits themselves — they're what drives this repo's automatic versioning, changelog, and tagging on release.

## Do this, not `git checkout -b`

```bash
.claude/skills/open-feature-branch/new-feature-branch.sh <short-description>
```

Example:

```bash
$ .claude/skills/open-feature-branch/new-feature-branch.sh fix-admin-table-widths
Switched to branch 'development'
Already up to date.
Switched to a new branch 'feature/fix-admin-table-widths'

On branch 'feature/fix-admin-table-widths', based on up-to-date development.
Push it once you have a commit:

    git push -u origin feature/fix-admin-table-widths

auto-pr.yml will then open (or reuse) a PR from 'feature/fix-admin-table-widths' into development
automatically — no need to open one by hand.
```

The script: fetches, checks out `development`, fast-forwards it, then branches off that — so you can't accidentally base new work on a stale `development` or on `main`. It also rejects passing the `feature/` prefix yourself (`fix-x`, not `feature/fix-x` — the script adds the prefix) to avoid an accidental `feature/feature/fix-x`.

## After pushing

Once your branch's checks (`lint` + `helm` + `test`, required by the `development` ruleset) are green, merge the auto-generated PR yourself — merges aren't automated, only the PR creation is.

Merging into `development` also triggers `promote.yml`'s `build-dev-image`/`build-dev-chart` jobs, which publish a real prerelease image + chart (`vX.Y.Z-dev.<sha>`) to GHCR and open (or update) a `chore: release vX.Y.Z` promotion PR into `main`. That PR must be merged as a real merge commit — squash/rebase breaks the digest-based promotion in `promote.yml`'s `promote-image` job (see that job's comments).

## Gotchas

- **Pushing before you have a commit does nothing.** `gh pr create` has nothing to diff against `development`, so `auto-pr.yml` logs "No PR created" and exits cleanly rather than failing loudly. If you don't see a PR appear, check you actually committed something first.
- **A second push to the same branch does not open a second PR.** `auto-pr.yml` checks for an existing open PR from that branch first and no-ops if one's already there — just keep pushing to the same branch.
- **Branching off `main` instead of `development` won't error immediately** — you'll only discover it when the resulting PR into `development` is full of unrelated diff noise (everything `development` has that `main` doesn't). Use the script; don't `git checkout -b feature/x main`.
