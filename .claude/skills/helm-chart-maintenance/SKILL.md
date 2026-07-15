---
name: helm-chart-maintenance
description: Use when config/* (kustomize manifests) has changed and charts/ai-cloud-operator needs updating, or when asked to "regenerate the helm chart", "update the chart for config/* changes", "add a value to the helm chart", "add a new template to the chart", fix a "chart is out of sync" / drift CI failure, or otherwise touch anything under charts/ai-cloud-operator or hack/helm-overrides. Not for docs/argocd-helm-deploy.md content (values reference for end users) or catalog/Convex-integration skills.
---

# Helm chart maintenance

`charts/ai-cloud-operator` is generated from `config/*` by
`hack/render-helm-chart.sh` (invoked via `make helm-chart`), using
[helmify](https://github.com/arttor/helmify). It is **not** hand-written.
Never hand-edit a helmify-generated file directly — regeneration will
silently discard the edit. Instead, edit the right override source (below)
and rerun the script.

## Core workflow

After **any** change under `config/*` (RBAC, CRD schema, manager Deployment,
params, etc.):

```sh
make helm-chart          # runs hack/render-helm-chart.sh
git add charts hack/helm-overrides
git commit
```

This is safe because regeneration doesn't just overwrite blindly — the
script reapplies a small, documented set of overrides on top of every fresh
helmify run (strips kustomize's configMap hash suffix, drops the unused
`kubernetesClusterDomain` value, merges `values-overlay.yaml`, inserts the
CRD's `resource-policy: keep` annotation, copies in the hand-maintained
`deployment.yaml`). Files helmify never generated in the first place
(`ingress.yaml`, `secret.yaml`) aren't touched at all.

If you forget: `.github/workflows/lint.yml`'s `helm` job and
`.github/workflows/helm-release.yml` both run `make helm-chart` and then
`git diff --exit-code -- charts hack/helm-overrides`, failing the build if
the committed chart doesn't exactly match a fresh regeneration. There is no
way to ship a stale/hand-drifted chart — CI catches it every push and every
release tag.

## What's regenerated vs. hand-maintained

| Category | Files | On every `make helm-chart` run |
|---|---|---|
| Fully regenerated | `values.yaml` (pre-overlay), `templates/api.yaml`, `*-rbac.yaml`, `metrics-service.yaml`, `params.yaml`, `serviceaccount.yaml`, `_helpers.tpl` | Overwritten from scratch by helmify from `kustomize build config/default`. Any manual edit here is lost next run. |
| Regenerated, then patched | `values.yaml` (final), `templates/workload-crd.yaml` | helmify generates it, then the script patches it: `values.yaml` gets `kubernetesClusterDomain` deleted and `hack/helm-overrides/values-overlay.yaml` merged on top via `yq`; `workload-crd.yaml` gets a `helm.sh/resource-policy: keep` annotation `sed`-inserted (helmify has no flag for it). |
| Hand-maintained, reapplied every run | `templates/deployment.yaml` | Fully written by hand at `hack/helm-overrides/templates/deployment.yaml`, then `cp`'d over whatever helmify just generated. Deployment is part of kustomize's output, so helmify regenerates its own version every run — the override file is the one that actually ships. |
| One-time hand-authored, never touched by the script | `templates/ingress.yaml`, `templates/secret.yaml` | Neither Ingress nor the out-of-band `ENROLLMENT_SECRET` Secret exist in `kustomize build config/default`'s output, so helmify never generated them and never will — the script doesn't list, copy, or patch them. Edit them directly in the chart, like any hand-written template. |

## How to add something new, by category

- **New value that isn't derived from a kustomize resource** (e.g. a new
  toggle like `ingress.enabled`, or overriding the image repo): add it to
  `hack/helm-overrides/values-overlay.yaml`, not `charts/.../values.yaml`
  directly — the latter gets clobbered by helmify every run, the former is
  `yq`-merged on top afterward. Add a comment explaining why it's not
  derivable from `config/*`.

- **New resource kind that `kustomize build config/default` will never
  produce** (same situation as Ingress/Secret — e.g. a NetworkPolicy, a
  second opt-in Secret, anything sourced from an optional kustomize overlay
  kustomize itself doesn't render into the default build): add the template
  directly under `charts/ai-cloud-operator/templates/`, hand-authored, using
  the existing `_helpers.tpl` helpers for naming/labels. Do **not** reference
  it anywhere in `hack/render-helm-chart.sh` — the whole point is helmify
  never owns it, so it survives regeneration untouched, same as
  `ingress.yaml`/`secret.yaml`.

- **`config/manager/manager.yaml` changes shape** (new env var, new probe,
  new volume, resource/security context changes): helmify will regenerate
  its own `templates/deployment.yaml` reflecting the change, but that copy
  gets discarded — you must hand-port the same change into
  `hack/helm-overrides/templates/deployment.yaml`. Workflow: run
  `make helm-chart`, then before the script's final `cp` step overwrites it
  (or by re-running helmify standalone into a scratch dir), diff
  helmify's freshly generated `deployment.yaml` against
  `hack/helm-overrides/templates/deployment.yaml` to see exactly what's new,
  and apply the equivalent change by hand (translating hard-coded values to
  `.Values.controllerManager.manager.*` references, matching the existing
  style). Quick way to get helmify's raw output for diffing:
  ```sh
  bin/kustomize build config/default \
    | sed -E 's/(ai-cloud-operator-params)-[a-z0-9]+/\1/g' \
    | bin/helmify -optional-crds -generate-defaults /tmp/chart-scratch
  diff /tmp/chart-scratch/templates/deployment.yaml \
       hack/helm-overrides/templates/deployment.yaml
  ```

- **New CRD or CRD schema change**: this flows through automatically
  (`workload-crd.yaml` is regenerated-then-patched), just run
  `make helm-chart`. Only touch `hack/render-helm-chart.sh` itself if a
  *new* CRD also needs a `resource-policy: keep` annotation added by name —
  currently the `sed` targets the first `annotations:` block, which works
  for the single existing CRD.

- **Tool version bumps** (helmify/yq/kustomize): pinned in the `Makefile` as
  `HELMIFY_VERSION`, `YQ_VERSION`, `KUSTOMIZE_VERSION` (near the `helmify:`,
  `yq:`, `kustomize:` targets, using the same `go-install-tool` /
  version-suffixed-binary-plus-symlink pattern as `controller-gen`). Bump the
  version variable; the tool re-downloads on next `make helm-chart` because
  the versioned binary path changes.

## Verification loop

```sh
make helm-chart
helm lint charts/ai-cloud-operator --set enrollmentSecret.value=x
helm template test charts/ai-cloud-operator --set enrollmentSecret.value=x
```

`enrollmentSecret.value` (or `enrollmentSecret.existingSecretName`) must be
set for lint/template to pass — `templates/secret.yaml` has a `required`
guard on it.

Expected, harmless `helm lint` warning: a metrics-service resource name
exceeding 63 characters under the fake release name `test-release` used by
`helm lint`/`helm template test`. This is cosmetic, not a regression — it
only happens because `test`/`test-release` don't contain `ai-cloud-operator`
and so don't collapse via the `fullname` helper (see
`docs/argocd-helm-deploy.md`'s "Release name matters for one thing" note).
Real releases named to contain `ai-cloud-operator` (e.g. `ai-cloud-operator`,
`ai-cloud-operator-prod`) don't hit this. Don't try to "fix" this warning by
changing chart templates.

For end-user-facing values documentation (what to set for a real ArgoCD
deploy), see `docs/argocd-helm-deploy.md` — don't duplicate it here.
