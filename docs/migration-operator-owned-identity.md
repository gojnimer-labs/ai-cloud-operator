# Migration: the operator now owns workload identity (name + namespace)

A focused migration guide for one breaking change: Convex no longer picks
(or needs to track) a workload's Kubernetes name or namespace. Both are now
derived/owned entirely by the operator. Hand this to whoever/whatever is
doing the ai-cloud-v2 side of this change — it's self-contained; for the
full field-by-field API reference, see `docs/catalog-parameters.md`.

## Why

Previously, every `POST /workloads` had to include a `name` and `namespace`
Convex picked, and every other endpoint required the exact namespace a
workload lived in to be threaded through every URL. In practice this was
redundant: a workload's identity is fully determined by *who* deployed
*which* template, and namespace is really an install-time decision for a
given operator instance, not something that varies per request. Removing
both means Convex never has to mint, validate, or track a Kubernetes-safe
identifier at all.

## What changed

### 1. Deploying: `name`/`namespace` are gone from the request

`POST /workloads` no longer accepts (or needs) `name` or `namespace` for a
template deploy:

```jsonc
// Before
{
  "name": "my-firefox",
  "namespace": "default",
  "templateName": "firefox",
  "userId": "user-123",
  "config": { /* ... */ }
}

// After
{
  "templateName": "firefox",
  "userId": "user-123",
  "config": { /* ... */ }
}
```

- The operator derives the workload's name itself from `userId`+
  `templateName` (one workload per user per template) — lowercased,
  sanitized to a valid Kubernetes DNS-1123 label, truncated to 63 characters
  if needed.
- The operator deploys into a single namespace fixed for this operator
  instance at install time (an operator-level config value, not something
  that varies per request or per user).
- **`userId` is now required** for a template deploy — 400 if missing.
- If you still send a `name`, it's silently ignored for template deploys
  (there's no need to stop sending it immediately, but you should stop
  relying on it).
- The response shape is unchanged: `{ "name", "namespace", "status" }` —
  `name`/`namespace` are still there, just always reflecting the
  operator-derived/fixed values rather than whatever you sent.

### 2. Every namespace-taking URL lost its `{namespace}` segment

| Before | After |
|---|---|
| `GET /workloads/{namespace}/{name}` | `GET /workloads/{name}` |
| `DELETE /workloads/{namespace}/{name}` | `DELETE /workloads/{name}` |
| `POST /workloads/{namespace}/{name}/functions/{key}` | `POST /workloads/{name}/functions/{key}` |
| `GET /gw/{namespace}/{name}/{entrypoint}/{subpath...}` | `GET /gw/{name}/{entrypoint}/{subpath...}` |

Since you no longer receive or choose a namespace, build every one of these
URLs with just the workload's name (which you get back from the deploy
response, or can recompute yourself if you know the derivation rule above).

### 3. Nothing else about the gateway auth flow changed

The one-time-token exchange and session cookie behave exactly as before —
same query param, same cookie name, same single-use semantics. You don't
need to change anything about how you mint or hand off gateway tokens.

## Checklist

- [ ] Stop sending `name`/`namespace` in the `POST /workloads` body for
      template deploys.
- [ ] Start sending `userId` on every template deploy if you weren't
      already treating it as required (it now 400s without one).
- [ ] Drop the namespace segment from every URL you build against
      `/workloads/...` and `/gw/...`.
- [ ] If you cached/stored a workload's namespace anywhere (e.g. to
      reconstruct URLs later), you can stop — it's always the same value
      for a given operator instance now, and the deploy response still
      tells you what it is if you want to display it.

## Ground truth, if you need to verify anything

`internal/api/server.go` (`deployRequest`, `handleDeploy`, `workloadSlug`,
route registration), `internal/workloadns/ensure.go` (namespace creation),
`internal/gateway/proxy.go` (`ServiceProxy`) — all in
`github.com/gojnimer-labs/ai-cloud-operator`.
