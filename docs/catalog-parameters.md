# Catalog parameters: what Convex needs to know

This is the consumer-facing reference for `GET /catalog`'s response shape and
how a parameter's fields should drive Convex/frontend behavior — written for
updating the Convex side (ai-cloud-v2) to match the operator's
`internal/catalog` refactor. If you're adding a new *template* on the
operator side instead, see `.claude/skills/workload-template/SKILL.md` in
this repo — that one's for Go authors, this one's for API consumers.

**This is a breaking change** to the `/catalog` JSON shape (see "What changed
from the old shape" at the bottom) — nothing was removed conceptually, but
field names and a couple of value shapes are different.

## Migration checklist for ai-cloud-v2

Hand this section to whoever/whatever is doing the Convex-side work — it's
the concrete task list; every item links to the detailed section below it
for the exact shape/reasoning. Known reference points in ai-cloud-v2 (named
in this operator's own doc comments — confirm these still exist/are named
the same before assuming, this repo doesn't control that one):
`convex/operators/actions.ts#resolveDynamicOptions` (resolves dynamic select
options), `convex/workloads/actions.ts#deployWorkload` (builds deploy config,
resolves `profileName`/system params), `convex/workloads/actions.ts#runCustomFunction`
(invokes an operation — name and response-parsing both need updating, see #5).

**1. `/catalog` response parsing**
- [ ] Stop reading `parameters[].source` — read `parameters[].dataSource.kind`
      instead (`"static"` | `"dynamic"` | `"system"`). See **DataSource**.
- [ ] Stop parsing `type` for a `"select_<key>"` prefix — dynamic selects are
      now always `type: "select"` plus `dataSource: {kind: "dynamic", sourceKey}`.
- [ ] Read `templates[].version` and store it wherever presets end up saved
      (see #4). See **Versioning**.
- [ ] Rename any `customFunctions` reference to `operations`; each entry also
      now carries `refreshable: boolean`. See **Operations**.

**2. Form rendering**
- [ ] Only render `static`/`dynamic`-sourced parameters as form fields; never
      render `system`-sourced ones.
- [ ] For `dynamic` parameters, call `resolveDynamicOptions` (or whatever
      it's called now) keyed by `dataSource.sourceKey`, not a parsed-out
      `type` string.
- [ ] Implement `visibility` evaluation: hide a parameter unless its
      `dependsOn` field's current value satisfies `op`/`value`/`values`. See
      **Visibility**.
- [ ] Optional/nice-to-have: client-side `validation` checks (min/max/regex/
      maxLength) before submitting — the operator still enforces these
      authoritatively either way. See **Validation**.

**3. Deploy flow**
- [ ] No breaking change to the `/deploy` request shape itself — same
      `config` object as before.
- [ ] Confirm error handling already treats `400`/etc. response bodies as
      plain text, not JSON (this was already true, not new).

**4. Presets (new capability, not yet built)**
- [ ] Store the template's `version` alongside a preset when it's created.
- [ ] When a preset is loaded/used later, compare its stored version against
      the current `/catalog` response's `version` for that template ID —
      decide the product behavior on a mismatch (warn/block/auto-migrate).
      The operator has no opinion here. See **Versioning**.

**5. Operations (renamed from CustomFunctions)**
- [ ] Rename `customFunctions`-related code/types to `operations`.
- [ ] Update the "invoke an operation" response parser: was a bare ad hoc
      object (e.g. `{"stdout": "..."}`), now always
      `{"additionalInfo": [{"name", "type", "value"}, ...]}`. See
      **Operations** → "Invoking one."
- [ ] Handle `type: "secret"` vs `"plain"` in the UI — mask secrets by
      default, offer an explicit reveal/copy action rather than displaying
      inline.
- [ ] For an operation with `refreshable: true`, decide your own polling
      interval if you want a live-updating value (e.g. a bearer token a
      workload generated at runtime) — the operator does nothing special
      here, it's the same invoke endpoint called again on whatever schedule
      you choose.

**6. Entrypoints (new field, breaking gateway URL change)**
- [ ] Parse `templates[].entrypoints` (`Entrypoint[]`, always present, at
      least one entry). See **Entrypoints**.
- [ ] Build gateway URLs as
      `/gw/{name}/{entrypoint}/{subpath...}` — the entrypoint segment is now
      **mandatory** for every workload, including single-entrypoint templates
      like `nginx` (use its one declared `entrypoints[].name`, e.g. `"http"`).
- [ ] When a template declares more than one entrypoint, decide how to
      pick/display which one to open (e.g. a tab/dropdown keyed on
      `entrypoints[].label`) — the operator has no opinion on UI here.

**7. Identity (name and namespace are now operator-owned)**
- [ ] Stop sending `name`/`namespace` on `POST /workloads` for a template
      deploy — the operator deploys into a single fixed namespace
      configured for this operator instance, and always generates a
      unique, brand-new Kubernetes name itself (`templateName` plus a
      random suffix). `userId` is still recorded on the created Workload,
      just not part of its name.
- [ ] `userId` becomes **required** for a template deploy (400 if missing).
- [ ] **Every template deploy call now creates a new instance** — it's no
      longer an upsert. Calling deploy twice with the same
      `templateName`+`userId` produces two separate workloads. If you need
      retry-safety, handle it on your side (debounce, disable the button
      while in flight) — the operator has no way to tell "retry of the
      same intent" from "deliberately deploy a second instance" apart.
- [ ] **Store the deploy response's `name`** — it's the only handle you get
      for a specific instance; you'll need it for every later
      get/delete/functions/gateway call against that instance.
- [ ] Drop the namespace segment from every URL you build:
      `GET`/`DELETE /workloads/{name}`, `POST /workloads/{name}/functions/
      {key}`, and the gateway's `/gw/{name}/{entrypoint}/{subpath...}`.
- [ ] `workloadResponse.namespace` still exists in the deploy response if you
      want to log/display it — it just always reflects the same
      operator-configured value now.

## The shape, field by field

`GET /catalog` returns a JSON array of Templates. One Template:

```jsonc
{
  "id": "firefox",
  "name": "Firefox Browser",
  "description": "Full Firefox browser accessible via web interface",
  "icon": "🦊",
  "version": "1.0.0",
  "parameters": [ /* Parameter[], see below */ ],
  "entrypoints": [ /* Entrypoint[], see below — always at least one */ ],
  "operations": [ /* Operation[], see below — omitted if empty */ ]
}
```

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Stable identifier — what you send back as `templateName` on deploy. |
| `name`, `description`, `icon` | string | Display-only. |
| `version` | string | See **Versioning** below. |
| `parameters` | Parameter[] | Deploy-time config. |
| `entrypoints` | Entrypoint[] | Web entrypoints this workload's Service exposes — always at least one. See **Entrypoints**. |
| `operations` | Operation[] | Operations against an already-*running* workload (e.g. "back up now", "get current token") — separate from deploy-time parameters. Omitted entirely when a template has none. |

One `Parameter`:

```jsonc
{
  "key": "profileDownloadUrl",
  "label": "Profile download URL (system)",
  "description": "...",              // optional, omitted if empty
  "type": "string",
  "dataSource": { "kind": "system" },
  "required": false,
  "default": "...",                  // optional, any JSON type, omitted if unset
  "options": [ { "value": "...", "label": "..." } ], // only for type "select", omitted otherwise
  "visibility": { "dependsOn": "restoreProfile", "op": "equals", "value": true }, // optional
  "validation": { "min": 0, "max": 65536 }            // optional
}
```

| Field | Meaning | What Convex/frontend does with it |
|---|---|---|
| `key` | The config key — this is exactly the key you send in `deploy`'s `config` object. | Use verbatim. |
| `type` | `"string"` \| `"number"` \| `"boolean"` \| `"select"` | Picks the form widget. |
| `dataSource.kind` | `"static"` \| `"dynamic"` \| `"system"` | See **DataSource** below — this is the field that changed shape from the old API. |
| `required` | Whether a value must be present **when the field is visible** (see Visibility). | Client-side form validation; the operator also enforces this authoritatively at deploy time. |
| `default` | Value to send if the user hasn't touched the field. | Prefill the form / use as fallback if omitting the key entirely. |
| `options` | Static choices, only present for `type: "select"` with `dataSource.kind: "static"`. | Render as the dropdown/radio options. Absent (not just empty) for a dynamic select — see below. |
| `visibility` | Conditionally hides the field. | See **Visibility** below — evaluate this client-side to decide whether to render the field at all. |
| `validation` | Value constraints beyond presence. | See **Validation** below. |

## DataSource — the field that replaced `source`

`dataSource.kind` is one of:

- **`"static"`** — the operator's `options` (if any) are complete; render a
  normal form field, user fills it in directly. This is the default/common
  case (was previously just `source: "user"` with no distinction from
  dynamic).
- **`"dynamic"`** — this is a `select`, but the operator deliberately left
  `options` empty. `dataSource.sourceKey` names which Convex-side source
  resolves the actual choices per-request (e.g. `"profiles_browser"` →
  query the user's saved browser profiles and populate the dropdown
  yourselves). This is what `select_<key>`-prefixed `type` strings used to
  mean — the prefix is gone; the same information now lives in
  `dataSource.sourceKey` with `type` staying plain `"select"`.
- **`"system"`** — Convex computes this value itself (e.g. an R2 presigned
  URL) and injects it directly into the `config` object sent to `deploy`.
  **Never render this as an editable form field** — the operator's own doc
  comments call out why for `profileDownloadUrl` specifically: an editable
  URL there would let the operator's init container fetch an
  attacker-controlled address (SSRF). Treat any `system`-sourced key the same
  way regardless of which template it's on.

## Visibility — new, wasn't expressible before

```jsonc
"visibility": { "dependsOn": "restoreProfile", "op": "equals", "value": true }
```

- `dependsOn`: another parameter's `key` in the *same* `parameters` list.
- `op`: `"equals"` | `"notEquals"` | `"oneOf"`.
- `value` (equals/notEquals) or `values` (oneOf): what to compare the
  depended-on field's *current form value* against.

A parameter with a `visibility` block should be hidden in the UI unless the
condition holds against whatever the user's currently entered for
`dependsOn`. Concretely today: `profileName` and `profileDownloadUrl`
(firefox/chrome) both carry a `visibility` block dependent on
`restoreProfile`, so neither is shown/asked for until the user's toggled
"restore a saved profile" on — this used to be just a doc comment ("only
meaningful when restoreProfile is true"); it's now a machine-checked rule
the operator itself enforces (see next point).

**Important operator-side behavior**: when a parameter's visibility
condition doesn't hold, the operator's validation (`ResolveParams`) skips
*both* `required` and `validation` for it entirely — a hidden field is never
treated as "missing" even if `required: true`. You don't have to replicate
this specific leniency in Convex, but don't be stricter than the operator
either (i.e., don't block a deploy client-side for a hidden required field
being empty — the operator won't).

## Validation — new field

```jsonc
"validation": { "min": 0, "max": 65536 }
"validation": { "regex": "^[a-z]+$" }
"validation": { "maxLength": 100 }
```

`min`/`max` apply to numeric values, `regex`/`maxLength` to strings — any
subset may be present. **The operator is the authoritative enforcer**: it
re-validates on every deploy request (`POST /workloads`) and rejects
violations with `400` before creating anything. Convex/the frontend should
still validate client-side for a decent UX (don't make the user wait for a
round trip to learn their input's out of range), but doesn't need to worry
about being the last line of defense.

Error responses today are **plain text**, not JSON (`http.Error` with just a
string body), so don't try to `JSON.parse` a 400 response. Current message
shapes, if you want to pattern-match on them for nicer error display:
- Missing required: `missing required parameter "<key>"`
- Validation failure: `parameter "<key>" invalid: must be <= 65536` (or
  `must be >= N`, `must match pattern "<regex>"`, `must be at most N
  characters`)

## Versioning — for your presets

```jsonc
"version": "1.0.0"
```

Every `Template` carries a manually-bumped version string. **The operator
never reads or enforces this itself** — it's purely informational, entirely
for Convex's benefit. Recommended usage for presets:

1. When a preset is saved, store the `version` of the template it was built
   against alongside it.
2. When a preset is loaded/used later, compare the stored version against
   the current `/catalog` response's `version` for that template ID.
3. On mismatch, it's entirely up to you what to do — warn the user the
   preset may be stale, block using it until reviewed, attempt an automatic
   migration, whatever fits the product. The operator has no opinion here
   and will accept a deploy either way as long as the actual `config` keys
   sent still resolve against the current template (unknown keys are
   ignored; missing now-required keys will 400 same as any other deploy).

A template's version bumps whenever *that template's* `parameters` change —
unrelated templates changing doesn't touch it.

## Operations — same Parameter shape, different endpoint

```jsonc
"operations": [
  {
    "key": "backup_state",
    "label": "Backup profile",
    "description": "...",
    "parameters": [ /* same Parameter shape as above, or [] for no input */ ],
    "refreshable": false
  }
]
```

These are operations against an already-*running* workload (invoked via
`POST /workloads/{name}/functions/{key}`, not the deploy
endpoint), discovered through this same `/catalog` response. Their
`parameters` follow every rule above identically (DataSource/Visibility/
Validation/Required all apply the same way, including "none needed" — an
empty/absent `parameters` array is entirely valid) — resolved and validated
by the same `ResolveParams` function on the operator side.

### `refreshable`

A catalog-level hint, not a per-invocation thing: `true` means this
operation is side-effect-free and safe for you to re-invoke on your own
interval just to get a current reading (e.g. an operation that reads a
token file the container generated at boot — re-reading it has no cost or
side effect). `false` (like `backup_state`, which tars and uploads to R2)
means invoking it does real work — only call it when a user deliberately
triggers it, never on a background timer. The operator does nothing special
with this value itself; it's purely informational for you to build a
polling/revalidation policy on top of. No rate-limiting or caching happens
operator-side regardless of how often you call a refreshable operation —
pick a sane interval (seconds-to-minutes) yourselves, since each call is a
real `exec` into the pod via the Kubernetes API server, not a cheap read.

### Invoking one: `POST /workloads/{name}/functions/{key}`

Request body: `{ "params": { ... } }` (or `{}`/omit `params` entirely for an
operation with no parameters). Response body:

```jsonc
{
  "additionalInfo": [
    { "name": "stdout", "type": "plain", "value": "Backup completed successfully" }
  ]
}
```

Each entry's `type` is `"secret"` or `"plain"`:
- **`secret`** — sensitive (e.g. a bearer token). Mask by default in any UI,
  avoid logging or persisting it in plaintext, offer an explicit
  reveal/copy action rather than displaying it inline.
- **`plain`** — informational, no special handling (e.g. `backup_state`'s
  `stdout`). (Not called `opaque` — this operator's Go code already uses
  that word for real Kubernetes Secrets with a different meaning:
  "unstructured secret data.")

There's no separate static schema declaring what an operation's outputs
*will* be ahead of time — `type`/`value` only exist once you actually invoke
it. Errors here are plain text same as `/deploy` (see **Validation** above)
— `404` for an unknown operation key, `400` for a resolution failure (e.g. a
required parameter missing), `409` if the workload has no running pod right
now, `500` if the operation itself failed (e.g. the exec'd command errored).

## Entrypoints — new, drives the gateway URL

```jsonc
"entrypoints": [
  { "name": "http", "label": "Web" }
]
```

Every template declares at least one entrypoint — a named web port its
Service exposes. Most templates today have exactly one, but a template whose
container serves more than one meaningful web UI (e.g. separate
"backoffice"/"frontoffice" ports) can declare several — this is the "see"
half: `entrypoints[].label` is what you'd show the user to pick one, when
there's more than one to pick from.

The "reach" half is the gateway URL shape, which now **requires** the
entrypoint name as a path segment for every workload, single-entrypoint or
not:

```
/gw/{name}/{entrypoint}/{subpath...}
```

`{entrypoint}` must be one of that workload's template's `entrypoints[].name`
values (e.g. `"http"` for every template today). An unknown entrypoint name
gets a `404`. This segment is purely a routing detail — it does **not**
affect the gateway auth cookie/token scope, which stays scoped to
`(namespace, name)` only: once you've authenticated against any one
entrypoint of a workload, the resulting cookie authorizes every other
entrypoint of that same workload too, with no separate token exchange per
entrypoint.

## Full real examples

**Nginx** (the simplest template — static select + validated number, no
system/dynamic parameters, no operations):

```json
{
  "id": "nginx",
  "name": "Nginx",
  "description": "Simple nginx web server with hello world demo",
  "icon": "🌐",
  "version": "1.0.0",
  "parameters": [
    {
      "key": "logLevel",
      "label": "Log level",
      "type": "select",
      "dataSource": { "kind": "static" },
      "required": false,
      "default": "info",
      "options": [
        { "value": "info", "label": "Info" },
        { "value": "warn", "label": "Warn" },
        { "value": "error", "label": "Error" }
      ]
    },
    {
      "key": "workerConnections",
      "label": "Worker connections",
      "description": "Passed through as an env var for illustration.",
      "type": "number",
      "dataSource": { "kind": "static" },
      "required": false,
      "default": 1024,
      "validation": { "min": 0, "max": 65536 }
    }
  ],
  "entrypoints": [
    { "name": "http", "label": "Web" }
  ]
}
```

**Firefox** (dynamic select + boolean + system param with visibility, plus an
operation):

```json
{
  "id": "firefox",
  "name": "Firefox Browser",
  "description": "Full Firefox browser accessible via web interface",
  "icon": "🦊",
  "version": "1.0.0",
  "parameters": [
    {
      "key": "profileName",
      "label": "Profile name",
      "description": "Identifies which saved profile to restore, if any.",
      "type": "select",
      "dataSource": { "kind": "dynamic", "sourceKey": "profiles_browser" },
      "required": false,
      "visibility": { "dependsOn": "restoreProfile", "op": "equals", "value": true }
    },
    {
      "key": "restoreProfile",
      "label": "Restore saved profile",
      "type": "boolean",
      "dataSource": { "kind": "static" },
      "required": false,
      "default": false
    },
    {
      "key": "profileDownloadUrl",
      "label": "Profile download URL (system)",
      "type": "string",
      "dataSource": { "kind": "system" },
      "required": false,
      "visibility": { "dependsOn": "restoreProfile", "op": "equals", "value": true }
    }
  ],
  "entrypoints": [
    { "name": "http", "label": "Web" }
  ],
  "operations": [
    {
      "key": "backup_state",
      "label": "Backup profile",
      "description": "Tars the current browser profile and uploads it to R2 so it can be restored into a future deploy.",
      "parameters": [
        {
          "key": "label",
          "label": "Backup name",
          "description": "A name to identify this saved profile later, when restoring it into a future deploy.",
          "type": "string",
          "dataSource": { "kind": "static" },
          "required": false
        },
        {
          "key": "uploadUrl",
          "label": "Upload URL (system)",
          "type": "string",
          "dataSource": { "kind": "system" },
          "required": true
        }
      ],
      "refreshable": false
    }
  ]
}
```

`chrome` is identical in shape to `firefox` (same `browserParameters`,
different image/id/name/icon).

## Deploying (name/namespace are now operator-owned, not sent by you)

`POST /workloads`:

```jsonc
{
  "templateName": "firefox",
  "userId": "user-123",
  "config": {
    "restoreProfile": true,
    "profileName": "<selectOptions row id>",
    "profileDownloadUrl": "<presigned R2 GET url Convex computed>"
  }
}
```

No `name` or `namespace` field for a template deploy. The operator deploys
into a single namespace fixed for this operator instance at install time,
and always creates a **brand-new** Workload with a unique, auto-generated
name (`templateName` plus a random suffix Kubernetes itself appends) —
there's nothing to pick, track, or pass to run more than one instance of
the same template for the same user; just call deploy again. `userId` is
**required** — 400 if missing — and is still recorded on the created
Workload; it just isn't part of the generated name (no need to duplicate
it there when it's already on the object).

**Every template deploy call creates a new instance** — this is not an
upsert. Calling deploy twice with identical `templateName`+`userId`+
`config` produces two separate, independently-addressable workloads, not
one workload updated twice. If you need retry-safety (e.g. a network retry
shouldn't create a duplicate), that's on your side — debounce the user
action, disable the button while a deploy is in flight, or otherwise avoid
firing the same logical deploy twice.

(`name` still exists on the request purely for the legacy non-template/
raw-`image` deploy path, where it's used verbatim as the literal Kubernetes
name — you shouldn't need that path.)

`config` only needs `static`/`dynamic` (user-facing) keys the user actually
set plus whatever `system` keys apply — the operator resolves defaults for
anything else. Response is `202 Accepted` with
`{ "name", "namespace", "status" }` (the Workload's current `.status`, likely
still `"Deploying"` at this point — poll `GET /workloads/{name}`
or watch for the Convex-side upsert notification for the real end state).
**Store the response's `name`** — it's the actual generated Kubernetes name,
the only handle you have for this specific instance, needed for every later
`GET`/`DELETE /workloads/{name}`, `/workloads/{name}/functions/{key}`, and
`/gw/{name}/...` call against it.

## What changed from the old shape

If you're updating existing Convex code rather than starting fresh:

- `parameters[].source` (`"user"` | `"system"`) → **removed**. Replaced by
  `parameters[].dataSource.kind` (`"static"` | `"dynamic"` | `"system"`) —
  old `source: "user"` splits into `dataSource.kind: "static"` (the common
  case) or `"dynamic"` (if it was also a `select_<key>`-prefixed type). Old
  `source: "system"` → `dataSource.kind: "system"`, unchanged meaning.
- `parameters[].type` values like `"select_profiles_browser"` → **gone**.
  `type` is now always one of the four plain values; the source-key moved to
  `dataSource.sourceKey`.
- `parameters[].visibility`, `parameters[].validation` → **new**, both
  optional/absent on any parameter that doesn't need them.
- `templates[].version` → **new**, always present (never empty string in
  practice — every template sets it).
- `templates[].customFunctions` → renamed to `templates[].operations` (same
  shape at the Template level, still omitted when empty). Each entry also
  gains `refreshable` (see **Operations** above).
- Invoking one (`POST /workloads/{name}/functions/{key}`)'s
  response body shape changed: previously a bare, ad hoc JSON object (e.g.
  `{"stdout": "..."}`); now always
  `{"additionalInfo": [{"name", "type", "value"}, ...]}` — every value is
  now explicitly typed `secret`/`plain` instead of you having to know by
  convention which fields might be sensitive.
- `templates[].entrypoints` → **new**, always present, at least one entry.
  The gateway URL shape changed to require an entrypoint segment:
  `/gw/{namespace}/{name}/{subpath...}` →
  `/gw/{namespace}/{name}/{entrypoint}/{subpath...}` — **breaking for every
  workload**, not just multi-entrypoint ones. See **Entrypoints**.
- `deployRequest.namespace` → **removed** for a template deploy. The
  operator deploys into a single namespace fixed for this operator
  instance — `userId` is now **required** (400 if missing) for a template
  deploy. Every namespace-taking URL lost its `{namespace}` segment as a
  result: `/workloads/{namespace}/{name}` → `/workloads/{name}`,
  `/workloads/{namespace}/{name}/functions/{key}` →
  `/workloads/{name}/functions/{key}`, and
  `/gw/{namespace}/{name}/{entrypoint}/{subpath...}` →
  `/gw/{name}/{entrypoint}/{subpath...}`. `workloadResponse.namespace` is
  unchanged shape-wise, just always the same operator-configured value now.
- `deployRequest.name` → **ignored entirely** for a template deploy. The
  operator always generates a brand-new, unique Kubernetes name from
  `templateName` plus a random suffix — **every template deploy call now
  creates a new instance rather than updating an existing one**, which is
  what lets one user run more than one instance of the same template
  without the caller picking or tracking anything. Store the
  response's `name` to address a specific instance later. Unchanged for
  the legacy non-template/raw-`image` path.

## Ground truth, if you need to verify anything

`internal/catalog/types.go` (struct definitions + doc comments),
`internal/catalog/registry.go` (`ResolveParams`'s exact two-pass
visibility/required/validation logic), `internal/catalog/{nginx,browser,
firefox,chrome}.go` (the real template definitions), `internal/api/server.go`
(`handleCatalog`/`handleDeploy`/`handleRunFunction`) — all in
`github.com/gojnimer-labs/ai-cloud-operator`.
