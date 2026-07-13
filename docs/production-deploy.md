# Deploying to a real cluster

This walks through running the operator as an actual in-cluster Deployment (image pulled from GHCR, real Secrets, a real reachable URL) instead of as a local process — the setup used for everything up to this point.

## Prerequisites

- `kubectl` pointed at the target cluster.
- A reachable Convex deployment (self-hosted or cloud) with `ENROLLMENT_SECRET` set on it (see `ai-cloud-v2/convex/operators/http.ts`'s `register` handler — it must match what you set here).
- If you plan to enable the ingress overlay: an ingress controller (k3s ships Traefik by default) and, optionally, cert-manager with a `ClusterIssuer` for automatic TLS.

## 1. Install the CRDs

```sh
make manifests
make install
```

## 2. Configure this cluster's settings

Edit `config/manager/params.env`:

```
CONVEX_BASE_URL=https://your-convex-deployment.example.com
OPERATOR_NAME=k3s-prod
OPERATOR_EXTERNAL_URL=https://operator.example.com
```

`OPERATOR_EXTERNAL_URL` must be the URL you'll actually expose this operator at once step 5 (ingress) is wired up — Convex calls back into it for deploy/delete/catalog/gateway-verify, and browsers use it for `/gw/*` routes.

## 3. Create the Secret (not checked into git)

```sh
kubectl create namespace ai-cloud-operator-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic ai-cloud-operator-env \
  --namespace ai-cloud-operator-system \
  --from-literal=ENROLLMENT_SECRET='<must match Convex ENROLLMENT_SECRET>' \
  --from-literal=GATEWAY_SIGNING_SECRET="$(openssl rand -hex 32)"
```

`ENROLLMENT_SECRET` is only actually used once, at first registration — the operator persists its issued token afterward (see `internal/tokenstore`) and reuses it across restarts. It still must be set at startup or the container fails fast (`cmd/main.go`'s `setupConvexIntegration`).

`GATEWAY_SIGNING_SECRET` is purely local to this operator now — it signs/verifies its own gateway session cookies (see `internal/gateway/token.go`) and is never shared with Convex, so a fresh random value is fine (and each operator instance can have its own).

## 4. Deploy

```sh
make deploy IMG=ghcr.io/gojnimer-labs/ai-cloud-operator:latest
```

This pushes the CRDs/RBAC/Deployment/Service/ConfigMap via kustomize (`config/default`). CI publishes the image (see `.github/workflows/publish.yml`) — you don't build it yourself.

## 5. Expose it publicly (optional overlay)

Edit `config/ingress/ingress.yaml`'s `TODO(user)` items (host, TLS secret name, ingress class), then either:

```sh
# uncomment the "- ../ingress" line in config/default/kustomization.yaml, then:
make deploy IMG=ghcr.io/gojnimer-labs/ai-cloud-operator:latest
```

or apply it standalone: `kubectl apply -k config/ingress`.

The generic `networking.k8s.io/v1 Ingress` here works unmodified against k3s's bundled Traefik or a standalone Traefik deployment (both implement standard Ingress) — for a different controller (nginx, etc.), just adjust `ingressClassName` and any annotations it needs.

## 6. Verify

```sh
kubectl get pods -n ai-cloud-operator-system
kubectl logs deploy/ai-cloud-operator-controller-manager -n ai-cloud-operator-system
```

Look for `"registered with convex"` (first boot) or `"reusing persisted operator token"` (subsequent restarts) in the log. Then confirm the operator shows up as `active` in Convex's `operators` list, and that `curl https://<OPERATOR_EXTERNAL_URL>/healthz` returns `200` from outside the cluster once step 5 is wired up.

## Notes / things to revisit later

- `resources.limits/requests` on the manager container in `config/manager/manager.yaml` are still the kubebuilder scaffold defaults — fine for a single operator instance, worth tuning once you know real usage.
- Each cluster/operator instance needs its own `config/manager/params.env` values and its own Secret — there's deliberately no shared/global config file for these, since `OPERATOR_NAME`/`OPERATOR_EXTERNAL_URL` are inherently per-instance.
- The GHCR package defaults to private on first push regardless of repo visibility — see the one-time manual step noted at the bottom of `.github/workflows/publish.yml`.
