# Deploying to a real cluster

This walks through running the operator as an actual in-cluster Deployment (image pulled from GHCR, real Secrets, a real reachable URL) instead of as a local process — the setup used for everything up to this point.

## Prerequisites

- `kubectl` pointed at the target cluster.
- A reachable Convex deployment (self-hosted or cloud) with `ENROLLMENT_SECRET` set on it (see `ai-cloud-v2/convex/operators/http.ts`'s `register` handler — it must match what you set here).
- A pushed `v*` tag on this repo, so `.github/workflows/publish.yml`'s `release` job has produced a consolidated `install.yaml` release asset: `git tag v0.1.0 && git push origin v0.1.0`.
- If you plan to enable the ingress overlay: an ingress controller (k3s ships Traefik by default) and, optionally, cert-manager with a `ClusterIssuer` for automatic TLS.

## Option A — no source checkout needed (recommended)

The image is all CI publishes for code; the manifests are published too, as one file, so a target machine never needs `git clone`.

### 1. Install everything

```sh
kubectl apply -f https://github.com/gojnimer-labs/ai-cloud-operator/releases/latest/download/install.yaml
```

This creates the namespace, CRDs, RBAC, the Deployment (image already pinned to the tag it was built from), the API Service, and a ConfigMap with empty placeholder values.

### 2. Set this instance's config

```sh
kubectl set env deployment/ai-cloud-operator-controller-manager -n ai-cloud-operator-system \
  CONVEX_BASE_URL=https://your-convex-deployment.example.com \
  OPERATOR_NAME=k3s-prod \
  OPERATOR_EXTERNAL_URL=https://operator.example.com
```

`kubectl set env` overrides these directly on the Deployment's pod template (regardless that they were originally wired via `configMapKeyRef`) and triggers a rollout automatically. `OPERATOR_EXTERNAL_URL` must be the URL you'll expose this operator at once step 4 (ingress) is wired up — Convex calls back into it for deploy/delete/catalog/gateway-verify, and browsers use it for `/gw/*` routes.

### 3. Create the Secret

```sh
kubectl create secret generic ai-cloud-operator-env \
  --namespace ai-cloud-operator-system \
  --from-literal=ENROLLMENT_SECRET='<must match Convex ENROLLMENT_SECRET>' \
  --from-literal=GATEWAY_SIGNING_SECRET="$(openssl rand -hex 32)"
kubectl rollout restart deployment/ai-cloud-operator-controller-manager -n ai-cloud-operator-system
```

`ENROLLMENT_SECRET` is only actually used once, at first registration — the operator persists its issued token afterward (see `internal/tokenstore`) and reuses it across restarts. It still must be set at startup or the container fails fast (`cmd/main.go`'s `setupConvexIntegration`).

`GATEWAY_SIGNING_SECRET` is purely local to this operator now — it signs/verifies its own gateway session cookies (see `internal/gateway/token.go`) and is never shared with Convex, so a fresh random value is fine (and each operator instance can have its own).

### 4. Expose it publicly

No repo needed for this either — paste and edit:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ai-cloud-operator-api
  namespace: ai-cloud-operator-system
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod   # remove if not using cert-manager
spec:
  ingressClassName: traefik   # k3s's bundled controller; adjust for other clusters
  rules:
  - host: operator.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: ai-cloud-operator-api
            port:
              name: https
  tls:
  - hosts:
    - operator.example.com
    secretName: ai-cloud-operator-api-tls
EOF
```

This is the same generic `networking.k8s.io/v1 Ingress` as `config/ingress/ingress.yaml` in the repo — works unmodified against k3s's bundled Traefik or a standalone Traefik deployment (both implement standard Ingress); adjust `ingressClassName`/annotations for a different controller.

## Option B — from a source checkout

Needed if you want to hand-edit `config/manager/params.env`/`config/ingress/ingress.yaml` directly before applying, or don't want to wait for a tagged release.

```sh
git clone https://github.com/gojnimer-labs/ai-cloud-operator.git
cd ai-cloud-operator
make manifests && make install                      # CRDs
# edit config/manager/params.env for this cluster
kubectl create secret generic ai-cloud-operator-env -n ai-cloud-operator-system \
  --from-literal=ENROLLMENT_SECRET='...' --from-literal=GATEWAY_SIGNING_SECRET="$(openssl rand -hex 32)"
make deploy IMG=ghcr.io/gojnimer-labs/ai-cloud-operator:latest
# edit config/ingress/ingress.yaml, then uncomment "- ../ingress" in config/default/kustomization.yaml
make deploy IMG=ghcr.io/gojnimer-labs/ai-cloud-operator:latest
```

## Verify (either option)

```sh
kubectl get pods -n ai-cloud-operator-system
kubectl logs deploy/ai-cloud-operator-controller-manager -n ai-cloud-operator-system
```

Look for `"registered with convex"` (first boot) or `"reusing persisted operator token"` (subsequent restarts) in the log. Then confirm the operator shows up as `active` in Convex's `operators` list, and that `curl https://<OPERATOR_EXTERNAL_URL>/healthz` returns `200` from outside the cluster once the ingress is wired up.

## Notes / things to revisit later

- `resources.limits/requests` on the manager container in `config/manager/manager.yaml` are still the kubebuilder scaffold defaults — fine for a single operator instance, worth tuning once you know real usage.
- Each cluster/operator instance needs its own config values and its own Secret — there's deliberately no shared/global config, since `OPERATOR_NAME`/`OPERATOR_EXTERNAL_URL` are inherently per-instance.
- The GHCR package defaults to private on first push regardless of repo visibility — see the one-time manual step noted at the bottom of `.github/workflows/publish.yml`.
