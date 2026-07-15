# Migration: the operator now owns workload identity (name + namespace)

A focused migration guide for one breaking change: Convex no longer
constructs, validates, or sends a workload's actual Kubernetes name, and
never picks its namespace at all. Every template deploy call creates a
brand-new Workload with a unique, operator-generated name (the template ID
plus a random suffix Kubernetes itself appends — `userId` doesn't need to
be part of it, since it's already recorded on the Workload's own spec),
always in a single namespace fixed for this operator instance — this is
also how multiple instances of the same template for the same user work:
there's no parameter for it, you just deploy again. Hand this to whoever/
whatever is doing the ai-cloud-v2 side of this change — it's
self-contained; for the full field-by-field API reference, see
`docs/catalog-parameters.md`.

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

### 1. Deploying: `name`/`namespace` are gone, and deploy is no longer an upsert

`POST /workloads` no longer accepts (or reads) `name`/`namespace` for a
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

// After — every call is identical in shape, whether it's the user's
// first firefox or their fifth
{
  "templateName": "firefox",
  "userId": "user-123",
  "config": { /* ... */ }
}
```

- The operator always generates a brand-new, unique Kubernetes name itself:
  the template ID plus a random suffix (Kubernetes' own `GenerateName`
  mechanism). There's no field for an instance label or anything else; you
  never construct, validate, or choose any part of the actual name.
  `userId` is still recorded on the created Workload — it's just not
  folded into the name, since it's already there on the object.
- **Every deploy call creates a new instance.** This is not an upsert
  anymore: two deploys with identical `templateName`+`userId` produce two
  separate, independently-addressable workloads. To run more than one
  instance of the same template for the same user, just call deploy again
  — there's nothing else to do.
- **This means deploy is no longer safe to retry blindly.** If you need to
  avoid creating a duplicate on a network retry or double-click, handle it
  on your side (debounce, disable the button while the call is in flight,
  track an in-flight state) — the operator has no way to distinguish "retry
  of the same intent" from "deliberately deploy a second instance."
- The operator deploys into a single namespace fixed for this operator
  instance at install time (an operator-level config value, not something
  that varies per request or per user).
- **`userId` is now required** for a template deploy — 400 if missing.
- The response shape is unchanged: `{ "name", "namespace", "status" }` —
  the response's `name` is the actual generated Kubernetes name and the
  *only* handle you have for this specific instance. **Store it** — you
  need it for every later `GET`/`DELETE`/`functions`/`/gw/` call against
  that instance.

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
      template deploys — both are ignored/rejected now.
- [ ] Start sending `userId` on every template deploy if you weren't
      already treating it as required (it now 400s without one).
- [ ] Treat every `POST /workloads` call as creating a new instance, not
      an upsert. If your current flow calls deploy repeatedly expecting it
      to update an existing workload's config in place, that no longer
      happens — you'll get a second workload instead.
- [ ] Add your own retry/duplicate-submission guard for the deploy action
      (debounce, disable-while-in-flight, or similar) if you don't already
      have one — the operator can no longer absorb an accidental double
      call for you.
- [ ] **Store the deploy response's `name`** (the actual generated
      Kubernetes name) for every instance you create — it's the only
      handle you have for that instance, needed for every later
      `GET`/`DELETE`/`functions`/`/gw/` call against it.
- [ ] Drop the namespace segment from every URL you build against
      `/workloads/...` and `/gw/...`.
- [ ] If you cached/stored a workload's namespace anywhere (e.g. to
      reconstruct URLs later), you can stop — it's always the same value
      for a given operator instance now, and the deploy response still
      tells you what it is if you want to display it.

## Ground truth, if you need to verify anything

`internal/api/server.go` (`deployRequest`, `handleDeploy`, route
registration), `internal/workloadns/ensure.go` (namespace creation),
`internal/gateway/proxy.go` (`ServiceProxy`) — all in
`github.com/gojnimer-labs/ai-cloud-operator`.
