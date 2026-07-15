# Deploying via Helm + ArgoCD

The Helm chart at `charts/ai-cloud-operator` is a straight port of `config/*`
(see `hack/render-helm-chart.sh` — it's regenerated from the kustomize
manifests via [helmify](https://github.com/arttor/helmify), not hand-written,
so `config/*` stays the single source of truth). It's published as an OCI
artifact to GHCR alongside the image, so ArgoCD can pull it directly with no
extra Helm repository to register.

For the non-Helm, non-ArgoCD path (plain `kubectl apply`), see
[production-deploy.md](./production-deploy.md) or
[k3s-quickstart.md](./k3s-quickstart.md) instead — this doc assumes you're
specifically using Helm/ArgoCD.

## 1. Make sure the chart package is public

Same one-time step the image needs (see `.github/workflows/publish.yml`'s
comment): after the first `v*` tag push, go to
`https://github.com/orgs/gojnimer-labs/packages` (or this repo's "Packages"
sidebar) → `ai-cloud-operator` (the chart package, distinct from the image
package of the same name) → Package settings → Visibility → Public.
`GITHUB_TOKEN` can't flip this itself, and until it's done ArgoCD's anonymous
`helm pull` gets a 404.

If you'd rather not make it public, register the registry as an
authenticated ArgoCD repository instead — see step 4.

## 2. Values you'll actually need to set

Everything else in `charts/ai-cloud-operator/values.yaml` has a sane
default; these are the ones every instance sets:

| Value | Same as | Notes |
|---|---|---|
| `params.convexBaseUrl` | `CONVEX_BASE_URL` | No trailing slash. |
| `params.operatorName` | `OPERATOR_NAME` | Unique per Convex deployment. |
| `params.operatorExternalUrl` | `OPERATOR_EXTERNAL_URL` | Must match `ingress.host` below if you enable the chart's ingress. |
| `enrollmentSecret.existingSecretName` | — | Name of a Secret (key `ENROLLMENT_SECRET` by default, see `enrollmentSecret.existingSecretKey`) you manage outside the chart — SealedSecret, External Secrets Operator, or a one-off `kubectl create secret`. **Preferred** for anything going through GitOps. |
| `enrollmentSecret.value` | — | Plaintext fallback the chart templates into a Secret itself, only used when `existingSecretName` is empty. Fine for a throwaway test install; don't commit a real value here. |

`GATEWAY_SIGNING_SECRET` has no corresponding value — the operator generates
and persists it itself on first boot (see `internal/gateway.KeyStore` and
`production-deploy.md`), same as a plain `kubectl apply` install.

Optional:

| Value | Default | Notes |
|---|---|---|
| `ingress.enabled` | `false` | Mirrors the optional `config/ingress` kustomize overlay. |
| `ingress.host` | `""` | Required if `ingress.enabled` — must match `params.operatorExternalUrl`. |
| `ingress.className` | `traefik` | Adjust for non-Traefik clusters. |
| `ingress.tls.secretName` | `<release>-api-tls` | Only relevant if `ingress.tls.enabled`. |
| `crds.enabled` | `true` | The Workload CRD ships as a regular template (not Helm's native `crds/` dir) so schema changes roll out on every ArgoCD sync, carrying a `helm.sh/resource-policy: keep` annotation so `helm uninstall`/pruning still can't delete it. |
| `controllerManager.manager.image.tag` | `""` (→ `.Chart.AppVersion`) | Only set to pin an older image than the chart version. |

**Release name matters for one thing**: several resource names get long
static suffixes (e.g. `-controller-manager-metrics-service`), and Kubernetes
object names cap at 63 characters. Naming the Application/Helm release so it
contains the chart name — `ai-cloud-operator`, `ai-cloud-operator-prod`, etc.
— keeps the generated names comfortably under that limit (the `fullname`
helper collapses to just the release name in that case, dropping the
`<release>-ai-cloud-operator-...` double prefix you'd get with an unrelated
release name).

## 3. ArgoCD Application

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ai-cloud-operator
  namespace: argocd
spec:
  project: default
  source:
    repoURL: ghcr.io/gojnimer-labs/charts
    chart: ai-cloud-operator
    targetRevision: 0.1.3   # matches the vX.Y.Z tag pushed to this repo
    helm:
      values: |
        params:
          convexBaseUrl: https://your-convex-deployment.example.com
          operatorName: prod
          operatorExternalUrl: https://operator.example.com
        enrollmentSecret:
          existingSecretName: ai-cloud-operator-enrollment
        ingress:
          enabled: true
          host: operator.example.com
  destination:
    server: https://kubernetes.default.svc
    namespace: ai-cloud-operator-system
  syncPolicy:
    syncOptions:
      - CreateNamespace=true
```

The chart doesn't template a Namespace resource (same as `helm install
--create-namespace` — Helm convention leaves that to the installer), so
`CreateNamespace=true` above (or a pre-existing namespace) is required.

`enrollmentSecret.existingSecretName`'s Secret has to exist in
`ai-cloud-operator-system` before the Application syncs — create it however
you already manage secrets in this cluster (SealedSecret, External Secrets
Operator, or for a quick test: `kubectl create secret generic
ai-cloud-operator-enrollment -n ai-cloud-operator-system
--from-literal=ENROLLMENT_SECRET='...'`). Rotating it later is just editing
that Secret in place — the operator polls it and re-registers on its own
within one heartbeat interval (`internal/convexclient`'s
`EnrollmentSecretWatcher`), no ArgoCD sync or pod restart needed.

## 4. If the chart package stays private

Register the registry as an authenticated Helm repository in ArgoCD instead
of relying on anonymous pull — declaratively, a `Secret` labeled as an
ArgoCD repository:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ghcr-ai-cloud-operator-charts
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  name: ghcr-ai-cloud-operator-charts
  url: ghcr.io/gojnimer-labs/charts
  type: helm
  enableOCI: "true"
  username: <gh-username>
  password: <gh-pat-with-read:packages-scope>
```

(equivalently, `argocd repo add ghcr.io/gojnimer-labs/charts --type helm
--enable-oci --username <gh-username> --password <gh-pat>` via the CLI). The
Application spec above doesn't change either way — ArgoCD matches it to this
repository by `repoURL`.

## 5. Verify

```sh
helm show values oci://ghcr.io/gojnimer-labs/charts/ai-cloud-operator --version 0.1.3
helm template ai-cloud-operator oci://ghcr.io/gojnimer-labs/charts/ai-cloud-operator --version 0.1.3 -f my-values.yaml
```

Then, same as the other deploy paths: `kubectl logs` for `"registered with
convex"`, and confirm the operator shows up as `active` in Convex's
`operators` list.

## Keeping the chart in sync with config/*

`charts/ai-cloud-operator` is generated, not hand-maintained — after
changing anything under `config/*`, run:

```sh
make helm-chart
```

and commit the result. CI (`lint.yml`'s `helm` job, and `helm-release.yml`
on every tag) regenerates the chart and fails if it doesn't exactly match
what's committed, so a stale chart can't ship silently. See
`hack/render-helm-chart.sh` for exactly what's regenerated vs. hand-maintained
(mainly `templates/deployment.yaml`, `values.yaml`, and the CRD's
resource-policy annotation — `templates/ingress.yaml` and
`templates/secret.yaml` are fully hand-authored since neither resource is
part of `config/*`'s kustomize output).
