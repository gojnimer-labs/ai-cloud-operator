---
name: convex-integration
description: Use when adding or changing how this operator talks to Convex, or when the operator needs to persist its own Kubernetes Secret. Covers two patterns — (1) operator-managed Secrets with narrowly-scoped RBAC (Load/Save, LoadOrGenerate, read-only Current), (2) adding a new Convex HTTP endpoint to internal/convexclient. Trigger phrases: "add a new Convex endpoint", "operator needs to call a new Convex API", "add a self-managed Secret", "new RBAC-scoped secret", "persist a token/key in a Secret".
---

# Convex integration patterns

Two recurring, related patterns in this operator: self-managed Kubernetes
Secrets with minimal RBAC, and Convex HTTP client endpoints. Read the real
files before copying — this doc is a map, not a replacement.

## 1. Operator-managed Secrets with narrowly-scoped RBAC

Three existing variants, all sharing the `labels.ManagedBy`/`ManagedByValue`
constants from `internal/labels/labels.go` (extracted after a duplication bug
— never inline this label pair again):

| Variant | File | Secret name | Verbs | When to use |
|---|---|---|---|---|
| `Load` / `Save` | `internal/tokenstore/tokenstore.go` | `ai-cloud-operator-token` | `create`; `get;update` scoped | Value is issued by an external party (Convex) and MUST be persisted exactly as given back, so the operator doesn't re-register every restart. |
| `LoadOrGenerate` | `internal/gateway/keystore.go` | `ai-cloud-operator-gateway-key` | `create`; `get;update` scoped | Nothing outside this operator instance needs to know the value — the operator mints it itself on first use rather than making a human provision it. Must handle the create-race between replicas (see below). |
| `Current` (read-only) | `internal/convexclient/enrollment.go` | `ai-cloud-operator-env` | `get` scoped only, no `create`/`update` | Value is human/GitOps-managed out-of-band (never checked into git). The operator only ever reads it, polling so a rotation doesn't require a pod restart. |

### The RBAC convention

`create` on `secrets` **cannot** be scoped with `resourceNames` — the object
doesn't exist yet at authorization time — so it's granted unscoped:

```go
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create
```

`get`/`update` (or `get`-only for read-only cases) **are** scoped to the one
Secret name the package owns, deliberately narrower than one shared
cluster-admin-ish credential:

```go
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;update,resourceNames=ai-cloud-operator-gateway-key
```

Verified against `config/rbac/role.yaml` (regenerated, not hand-edited): each
package gets its own `resourceNames`-scoped rule, and the unscoped `create`
verbs across all secret-owning packages collapse into one shared rule (kubebuilder/controller-gen merges identical apiGroup+verb combos — it also
merged the `secrets` and `pods/exec` unscoped `create` rules together). Other
`+kubebuilder:rbac` markers exist beyond the three Secret examples —
`internal/podexec/exec.go` (`pods` get/list/watch, `pods/exec` create),
`internal/gateway/proxy.go` (`services/proxy`), and
`internal/controller/workload_controller.go` (Workload CR, Deployments,
Services, Events) — same convention, just not Secret-shaped.

### Adding a fourth Secret-backed store

1. New package (or file in an existing one) with a `SecretName`/`KeySecretName` const and a `Store`/`Watcher` struct holding `client client.Client` + `namespace string`.
2. Pick the variant: issued-and-must-persist → `Load`/`Save`; self-generate → `LoadOrGenerate` (handle `apierrors.IsAlreadyExists` on the create race — see keystore.go's comment on replicas converging on whichever create wins); human/GitOps-owned, read-only → `Current`.
3. Stamp `labels.ManagedBy: labels.ManagedByValue` on any Secret this operator creates (not on ones it only reads, like enrollment).
4. Add the `+kubebuilder:rbac` marker comment directly above the type, verbs split exactly as above:
   ```go
   // +kubebuilder:rbac:groups="",resources=secrets,verbs=create
   // +kubebuilder:rbac:groups="",resources=secrets,verbs=get;update,resourceNames=<your-secret-name>
   ```
   (drop the `create` line and use `verbs=get` only for a read-only watcher).
5. Run `make manifests` to regenerate `config/rbac/role.yaml` from the markers.
6. Run `make helm-chart` afterward too — the Helm chart under `charts/ai-cloud-operator` is generated from the same kustomize/RBAC output and must stay in sync (see the helm-chart-maintenance skill for how that works; don't hand-edit the chart's RBAC templates).

### Skeleton (LoadOrGenerate-style)

```go
const SecretName = "ai-cloud-operator-<thing>"
const keyValue = "value"

// +kubebuilder:rbac:groups="",resources=secrets,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;update,resourceNames=ai-cloud-operator-<thing>

type Store struct {
    client    client.Client
    namespace string
}

func New(c client.Client, namespace string) *Store {
    return &Store{client: c, namespace: namespace}
}

func (s *Store) LoadOrGenerate(ctx context.Context) ([]byte, error) {
    var secret corev1.Secret
    err := s.client.Get(ctx, client.ObjectKey{Name: SecretName, Namespace: s.namespace}, &secret)
    if err == nil {
        if v := secret.Data[keyValue]; len(v) > 0 {
            return v, nil
        }
    } else if !apierrors.IsNotFound(err) {
        return nil, fmt.Errorf("getting secret: %w", err)
    }

    v := generate()
    created := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name: SecretName, Namespace: s.namespace,
            Labels: map[string]string{labels.ManagedBy: labels.ManagedByValue},
        },
        Type: corev1.SecretTypeOpaque,
        Data: map[string][]byte{keyValue: v},
    }
    if err := s.client.Create(ctx, created); err != nil {
        if apierrors.IsAlreadyExists(err) {
            // Lost the create race to another replica — re-read and use its value.
            var existing corev1.Secret
            if getErr := s.client.Get(ctx, client.ObjectKey{Name: SecretName, Namespace: s.namespace}, &existing); getErr != nil {
                return nil, fmt.Errorf("getting secret after lost create race: %w", getErr)
            }
            return existing.Data[keyValue], nil
        }
        return nil, fmt.Errorf("creating secret: %w", err)
    }
    return v, nil
}
```

## 2. Adding a new Convex HTTP endpoint

All Convex calls live in `internal/convexclient/client.go` as methods on
`*Client` (holds `config Config` + `httpClient *http.Client`). Every method
follows the same shape:

1. **Request struct** named `<verb>Request` with `json:"..."` tags matching Convex's expected body (see `registerRequest`, `upsertWorkloadRequest`, `verifyGatewayTokenRequest`, `removeWorkloadRequest`).
2. **Response struct** named `<verb>Response` only if the endpoint returns a body to decode (`registerResponse`, `verifyGatewayTokenResponse`); skip it if the endpoint is fire-and-forget (`UpsertWorkload`, `RemoveWorkload` return only `error`).
3. `json.Marshal` the request, wrapping the error as `fmt.Errorf("marshaling <verb> request: %w", err)`.
4. `http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/<path>", bytes.NewReader(body))`, `Content-Type: application/json` always; add `Authorization: Bearer <heartbeatToken>` **unless** the call is `Register` itself (which authenticates via the enrollment secret in the body, not a bearer token — it's the only exception).
5. `c.httpClient.Do(req)`, `defer resp.Body.Close()`.
6. Status handling:
   - Plain endpoints: `if resp.StatusCode != http.StatusOK { return fmt.Errorf("<verb> returned status %d", resp.StatusCode) }`.
   - Endpoints where the caller needs to distinguish "credential rejected, re-register" (only `Heartbeat` today): a `switch` mapping `http.StatusUnauthorized, http.StatusGone` to the shared `ErrUnauthorized` sentinel, everything else to a generic status error, `StatusOK` to `nil`.
7. Decode the response body into the response struct only if one exists.

Every method that isn't `Register` takes `heartbeatToken string` as an
explicit parameter — the client itself is stateless about tokens; whoever
calls it (today, only `Runnable`) is responsible for holding the current
token.

### Wiring through Runnable (only if the reconciler/API server needs to call it)

`internal/convexclient/runnable.go`'s `Runnable` holds the current
`tokenstore.Tokens` behind a `sync.RWMutex` and exposes thin passthrough
methods that read the current heartbeat token under `RLock` and call the
`Client` method — see `UpsertWorkload`/`RemoveWorkload`/`VerifyGatewayToken`
for the exact pattern:

```go
func (r *Runnable) YourNewCall(ctx context.Context, ...) (T, error) {
    r.mu.RLock()
    token := r.tokens.HeartbeatToken
    r.mu.RUnlock()
    return r.client.YourNewCall(ctx, token, ...)
}
```

If callers outside `convexclient` need this (reconciler, API server), define
a narrow interface at the call site the way `internal/controller` has
`WorkloadNotifier` and `internal/api` has `GatewayVerifier`, and satisfy it
with this `Runnable` method — don't have callers reach into `*Client`
directly.

`Register`/`Heartbeat` themselves are only ever driven by `Runnable.Start`'s
`loadOrRegister`/`register`/`heartbeatOnce` loop; don't call them elsewhere.

### Tests

Add to `internal/convexclient/client_test.go` (or `runnable_test.go` if
testing the `Runnable` wiring), following the existing conventions:

- Reuse shared fixture consts already declared in `client_test.go` (`testHeartbeatToken`, `testOperatorName`, `testNamespace`, `testWorkloadName`, `testUserID`, `testBearerHeartbeatToken`, path consts like `pathOperatorsRegister`) — add a new path const there if your endpoint needs one, don't inline the string in multiple tests.
- Spin up `httptest.NewServer(http.HandlerFunc(...))`, assert on `r.URL.Path` and (if applicable) the `Authorization` header, decode the request body into your request struct and assert on its fields, `defer srv.Close()`.
- One test for the happy path (decode+assert the response, or just check `err == nil` for fire-and-forget calls), one test for the non-OK-status-is-error case (`TestUpsertWorkloadNonOKIsError` is the minimal template), and — only if the endpoint distinguishes 401/410 — a table-driven test over both statuses mapping to `ErrUnauthorized` (`TestHeartbeatUnauthorizedMapsToSentinel`).
- For `Runnable`-level behavior, use `newFakeTokenStore`/`newFakeEnrollmentWatcher` (fake controller-runtime client) plus `atomic.Int32` call counters to assert exactly how many times the server was hit.

### Skeleton (new fire-and-forget endpoint, needs heartbeat token)

```go
type yourCallRequest struct {
    Foo string `json:"foo"`
}

// YourCall tells Convex ... Called by <caller>.
func (c *Client) YourCall(ctx context.Context, heartbeatToken, foo string) error {
    body, err := json.Marshal(yourCallRequest{Foo: foo})
    if err != nil {
        return fmt.Errorf("marshaling your call request: %w", err)
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/your-call", bytes.NewReader(body))
    if err != nil {
        return fmt.Errorf("building your call request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer "+heartbeatToken)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("calling your call: %w", err)
    }
    defer func() { _ = resp.Body.Close() }()

    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("your call returned status %d", resp.StatusCode)
    }
    return nil
}
```
