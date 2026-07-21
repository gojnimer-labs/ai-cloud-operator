# k3s VM quickstart (domain + Let's Encrypt)

A concrete walkthrough for deploying ai-cloud-operator on a k3s VM you have a
domain pointed at, with cert-manager not yet installed — no source checkout
needed. For the fuller reference (from-source option, more detail) see
[production-deploy.md](./production-deploy.md).

**Port to expose on the VM: 80 and 443 only.** k3s's bundled Traefik listens
on those by default — 443 serves real traffic, 80 handles Let's Encrypt's
HTTP-01 challenge. The operator's own port (9443) stays internal-only,
reached via Ingress → Service → pod.

**Prerequisite:** a `v*` tag must have been pushed to the repo already, so
`.github/workflows/publish.yml`'s `release` job has produced the
`install.yaml` asset this guide applies. `.github/workflows/auto-tag.yml`
pushes one automatically on every merge to `main`, so this is usually
already true — check the repo's Releases page if unsure.

## 1. DNS + firewall

- Point an A record (e.g. `operator.yourdomain.com`) at the VM's public IP.
- Open inbound 80/443 in whatever firewall/security group sits in front of
  the VM.

## 2. Confirm kubectl access

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl get nodes
```

(add that `export` to your shell profile so it persists)

## 3. Install cert-manager

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available --timeout=120s -n cert-manager deployment --all
```

Then a `ClusterIssuer` for Let's Encrypt:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: you@yourdomain.com
    privateKeySecretRef:
      name: letsencrypt-prod-key
    solvers:
    - http01:
        ingress:
          ingressClassName: traefik
EOF
```

## 4. Install the operator

```sh
kubectl apply -f https://github.com/gojnimer-labs/ai-cloud-operator/releases/latest/download/install.yaml
```

This creates the namespace, CRDs, RBAC, the Deployment (image already
pinned to the tag it was built from), and the API Service.

**Checkpoint:** if the GHCR package hasn't been made public yet (one-time
step after the first CI run — GitHub → repo → Packages →
`ai-cloud-operator` → Package settings → visibility → Public), the pod will
fail to pull the image. Worth checking now before continuing.

## 5. Configure this instance

```sh
kubectl set env deployment/ai-cloud-operator-controller-manager -n ai-cloud-operator-system \
  CONVEX_BASE_URL=https://your-convex-deployment.example.com \
  OPERATOR_NAME=k3s-vm \
  OPERATOR_EXTERNAL_URL=https://operator.yourdomain.com
```

## 6. Create the Secret

```sh
kubectl create secret generic ai-cloud-operator-env \
  --namespace ai-cloud-operator-system \
  --from-literal=ENROLLMENT_SECRET='<same value as Convex ENROLLMENT_SECRET>'
kubectl rollout restart deployment/ai-cloud-operator-controller-manager -n ai-cloud-operator-system
```

(`ENROLLMENT_SECRET` must match what's set on your Convex deployment — check
with `npx convex env get ENROLLMENT_SECRET` from `ai-cloud-v2` if unsure.)

No `GATEWAY_SIGNING_SECRET` to create — the operator generates and persists
its own on first boot. And if you ever need to rotate `ENROLLMENT_SECRET`
later, `kubectl create secret ... -o yaml --dry-run=client | kubectl apply
-f -` (or `kubectl edit secret`) is enough on its own — this Secret is
mounted into the pod as a volume, and the operator re-checks it on every
heartbeat, so it re-registers automatically within a couple of minutes, no
`rollout restart` needed.

## 7. Wire the ingress

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ai-cloud-operator-api
  namespace: ai-cloud-operator-system
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
spec:
  ingressClassName: traefik
  rules:
  - host: operator.yourdomain.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: ai-cloud-operator-api
            port:
              name: http
  tls:
  - hosts:
    - operator.yourdomain.com
    secretName: ai-cloud-operator-api-tls
EOF
```

## 8. Verify

```sh
kubectl get pods -n ai-cloud-operator-system
kubectl logs deploy/ai-cloud-operator-controller-manager -n ai-cloud-operator-system
kubectl get certificate -n ai-cloud-operator-system   # wait for READY=True
curl https://operator.yourdomain.com/healthz          # expect 200
```

Look for `"registered with convex"` in the log, then confirm it shows up as
`active` in Convex's operators list.
