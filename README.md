# ai-cloud-operator

[![Tests](https://github.com/gojnimer-labs/ai-cloud-operator/actions/workflows/test.yml/badge.svg)](https://github.com/gojnimer-labs/ai-cloud-operator/actions/workflows/test.yml)
[![Lint](https://github.com/gojnimer-labs/ai-cloud-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/gojnimer-labs/ai-cloud-operator/actions/workflows/lint.yml)
[![Latest release](https://img.shields.io/github/v/release/gojnimer-labs/ai-cloud-operator)](https://github.com/gojnimer-labs/ai-cloud-operator/releases/latest)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](./LICENSE)

A Kubernetes operator that reconciles a `Workload` custom resource into a
Deployment + Service, and integrates with a [Convex](https://convex.dev)
backend for ownership tracking and browser-facing access:

- **Register/heartbeat/deploy contract** — the operator registers itself
  with Convex on startup, heartbeats on an interval, and exposes an inbound
  HTTP API Convex calls to deploy/delete/inspect `Workload`s
  (`internal/api`, `internal/convexclient`).
- **Ownership sync** — every Workload create/update/delete is reported back
  to Convex (`internal/controller`), so its records stay in sync even for
  changes made directly with `kubectl`, bypassing Convex entirely. Delivery
  is retried until it succeeds, tracked via a `ConvexSynced` status
  condition.
- **Browser-facing gateway** — `/gw/{name}/{entrypoint}/...` reverse-proxies
  into a running workload via the Kubernetes API server's `services/proxy`
  subresource, authenticated by a round trip to Convex followed by a
  self-issued, per-workload signed session cookie (`internal/gateway`).
- **Catalog templates** — `Workload.spec.templateName` selects a
  pre-built container spec (`nginx`, `firefox`, `chrome` today) instead of
  a raw image, so common workload types don't need their spec hand-written
  (`internal/catalog`). See `.claude/skills/workload-template` for how to
  add a new one.

## Install

### Option 1 — plain `kubectl` (no Helm needed)

```sh
kubectl apply -f https://github.com/gojnimer-labs/ai-cloud-operator/releases/latest/download/install.yaml
kubectl set env deployment/ai-cloud-operator-controller-manager -n ai-cloud-operator-system \
  CONVEX_BASE_URL=https://your-convex-deployment.example.com \
  OPERATOR_NAME=my-cluster \
  OPERATOR_EXTERNAL_URL=https://operator.example.com
kubectl create secret generic ai-cloud-operator-env -n ai-cloud-operator-system \
  --from-literal=ENROLLMENT_SECRET='<must match Convex ENROLLMENT_SECRET>'
kubectl rollout restart deployment/ai-cloud-operator-controller-manager -n ai-cloud-operator-system
```

Full walkthrough (DNS, ingress, cert-manager): see
[docs/k3s-quickstart.md](./docs/k3s-quickstart.md) or the fuller reference
[docs/production-deploy.md](./docs/production-deploy.md).

### Option 2 — Helm / ArgoCD

The chart is published as an OCI artifact alongside the image — no separate
Helm repository to add.

```sh
helm install ai-cloud-operator oci://ghcr.io/gojnimer-labs/charts/ai-cloud-operator \
  --namespace ai-cloud-operator-system --create-namespace \
  --set params.convexBaseUrl=https://your-convex-deployment.example.com \
  --set params.operatorName=my-cluster \
  --set params.operatorExternalUrl=https://operator.example.com \
  --set enrollmentSecret.existingSecretName=ai-cloud-operator-enrollment
```

(omitting `--version` pulls the latest published chart — pass one explicitly to pin, e.g. `--version 0.1.7`)

(create that Secret first: `kubectl create secret generic
ai-cloud-operator-enrollment -n ai-cloud-operator-system
--from-literal=ENROLLMENT_SECRET='...'`)

For the ArgoCD `Application` spec, the full values reference, and running
without Helm's CLI at all, see
[docs/argocd-helm-deploy.md](./docs/argocd-helm-deploy.md).

**No `GATEWAY_SIGNING_SECRET` to set in either option** — the operator
generates and persists its own on first boot
(`internal/gateway.KeyStore`); it never needs to be shared with Convex or
anyone else.

### Try it

```sh
kubectl apply -f config/samples/apps_v1alpha1_workload.yaml
kubectl get workloads
```

or a real one, without a template:

```yaml
apiVersion: apps.aicloud.dev/v1alpha1
kind: Workload
metadata:
  name: hello-nginx
spec:
  image: nginx:1.27
  containerPort: 80
```

## Releases

Any `feature/**` branch auto-opens a PR into `development` (`auto-pr.yml`).
Every push to `development` builds and publishes a real,
pullable prerelease image + chart tagged `vX.Y.Z-dev.<sha>` (no GitHub
Release, no `latest`), and opens or updates a `chore: release vX.Y.Z`
promotion PR into `main` with a changelog (`promote.yml`). Merging that PR
(as a real merge commit — squash/rebase isn't supported here) promotes the
exact same image by digest to the stable `vX.Y.Z` tag and `latest` — it's
never rebuilt — repackages the chart under that version, and cuts the
`vX.Y.Z` git tag and GitHub Release with `install.yaml` attached.

The version bump (`vX.Y.(Z+1)` vs. a minor/major bump) is computed from
Conventional Commits since the last release
(`.github/actions/compute-next-version`) rather than always being a patch.
There's no manual tagging step at any stage.

## Development

```sh
make manifests generate  # regenerate CRDs/RBAC/DeepCopy from +kubebuilder markers
make helm-chart           # regenerate charts/ai-cloud-operator from config/* (see hack/render-helm-chart.sh)
make test                 # unit + envtest (real kube-apiserver, no real cluster needed)
make test-e2e              # e2e against a throwaway Kind cluster
make lint                 # golangci-lint
make run                  # run the manager locally against your current kubeconfig context
```

See [AGENTS.md](./AGENTS.md) for the fuller kubebuilder project-structure
reference, and `.claude/skills/` for repo-specific how-tos (adding a
workload catalog template, regenerating the Helm chart, extending the
Convex integration).

## License

[Apache License 2.0](./LICENSE)
