---
name: conventional-commits
description: How to write commit messages in this repo so releases version and tag themselves correctly. Use when committing code, opening a PR, or asking about versioning, releases, tags, or the CHANGELOG.
---

# Commit messages drive versioning — write them as Conventional Commits

Nobody manually runs `git tag` in this repo. Every `development` -> `main` promotion PR (`promote.yml`'s `promote-pr` job) reads the commit messages being promoted, computes the next version from them, and titles the PR `chore: release vX.Y.Z` with a generated changelog. Merging that PR is what actually tags the release (`promote.yml`'s `tag-if-main` job, on push to `main`) and publishes it as a GitHub Release with `install.yaml` attached. The commit message is the only input to all of that — get it wrong and the version bump or changelog section is wrong, silently.

See `.github/actions/compute-next-version/action.yml` for the exact logic below; this skill is the human-facing summary of what it does.

## The format

```
<type>[(optional scope)]: <description>
```

Examples: `feat(webhook): validate profile name is required`, `fix: don't report a transient reconcile conflict as failed`, `chore: bump deps`.

## What each type does to the version

| Commit looks like | Bump | Changelog section |
| --- | --- | --- |
| `feat: ...` or `feat(scope): ...` | minor | Features |
| `fix: ...` or `fix(scope): ...` | patch | Bug Fixes |
| `feat!: ...`, `fix!: ...`, or any type with `!` before the `:` | **major** | Same section, marked **BREAKING** |
| A `BREAKING CHANGE:` footer in the commit body (even without `!`) | **major** | — |
| Anything else (`chore:`, `docs:`, `refactor:`, no prefix at all, merge commits are excluded entirely) | patch (if nothing higher-severity exists) | Other Changes |

The bump for a whole promotion is the **highest severity found** across all commits being promoted — one `feat` among ten `fix`es still bumps minor, not patch. Non-conventional commits don't break anything; they just land in "Other Changes" and can never push the bump higher than patch on their own.

## What you don't need to do

- **Don't hand-write a version number anywhere.** There's no `VERSION` file and nothing in the Makefile computes one — the version lives entirely in git tags, computed fresh each time.
- **Don't tag anything yourself.** `git tag` from a human would just create a tag `compute-next-version` doesn't know about, and the next real release would recompute from the last _real_ tag, ignoring it.
- **Don't worry about squashing this repo's history.** The `development -> main` promotion PR must merge as a real merge commit (`promote.yml`'s `promote-image` job depends on `HEAD^2` to find the exact prerelease image it's promoting — squash/rebase breaks that), so every individual commit's message survives and gets scanned — write good messages on each commit, not just the PR title.

## Gotchas specific to this repo

- **Two kinds of build come out of one set of commits.** A push to `development` publishes a real `vX.Y.Z-dev.<sha>` prerelease image + chart to GHCR (no GitHub Release, no `latest`) using the same computed `X.Y.Z` the eventual stable release will have. Merging the promotion PR doesn't rebuild anything — it retags that exact image by digest and repackages the chart under the plain `vX.Y.Z`. If the version your commits computed looks wrong on the prerelease build, fix the commit message and push again before promoting, not after.
- **The first-ever release under this scheme scans the entire project history** if no prior `v*` tag exists. Expected, not a bug — every release after that only scans back to the previous tag.
- **A `feat` that lands after a promotion PR is already open updates it in place**, including bumping the proposed version (e.g. patch -> minor) — `promote.yml`'s `promote-pr` job edits the existing PR rather than leaving it stale, so don't assume the PR's title is fixed once opened.
- **Merge commits are excluded from the scan** (`git log --no-merges`) — the auto-generated "Merge pull request #N..." commits are noise, not meaningful changes, so they never appear in a changelog or affect the bump.
