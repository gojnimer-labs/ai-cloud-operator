# Migrating ai-cloud-v2 against the operator

A standalone, chronological log of breaking changes to the operator's HTTP
API since the last known Convex-side sync. Hand this file to whoever/
whatever is doing the ai-cloud-v2 work as the top-level task list — each
entry links into `docs/catalog-parameters.md`'s field-by-field reference for
the exact shape/reasoning behind it. That file also has its own "Migration
checklist" section covering the same ground in more detail; this one is the
short version meant to be read start to finish.

Known ai-cloud-v2 reference points (named in the operator's own doc comments
— confirm these still exist/are named the same before assuming, this repo
doesn't control that one): `convex/operators/actions.ts#resolveDynamicOptions`,
`convex/workloads/actions.ts#deployWorkload`, `convex/workloads/actions.ts#runCustomFunction`.

## 3. Entrypoints + breaking gateway URL change (current)

- [ ] Parse the new `templates[].entrypoints` field (`Entrypoint[]`, always
      present, at least one entry: `{ name, label }`).
- [ ] Update every gateway URL you build from
      `/gw/{namespace}/{name}/{subpath...}` to
      `/gw/{namespace}/{name}/{entrypoint}/{subpath...}` — **this breaks
      every existing workload**, not just multi-entrypoint ones. Use the
      template's one declared `entrypoints[].name` (e.g. `"http"`) for
      today's single-entrypoint templates (nginx/firefox/chrome).
- [ ] When a template declares more than one entrypoint, decide your own UI
      for picking which one to open (tabs, a dropdown, etc., keyed on
      `entrypoints[].label`) — the operator has no opinion here.
- [ ] No change to the one-time-token exchange or gateway auth cookie: a
      token still authorizes "this workload" generically, and one cookie
      authorizes every entrypoint of that workload — you don't need a
      separate token per entrypoint.

See `docs/catalog-parameters.md` → **Entrypoints**.

## 2. CustomFunction → Operation, typed AdditionalInfo output, Refreshable

- [ ] Rename any `customFunctions`-related code/types to `operations`.
- [ ] Update the "invoke an operation" response parser: was a bare ad hoc
      object (e.g. `{"stdout": "..."}`), now always
      `{"additionalInfo": [{"name", "type", "value"}, ...]}`.
- [ ] Handle `type: "secret"` vs `"plain"` in the UI — mask secrets by
      default, offer an explicit reveal/copy action rather than displaying
      inline.
- [ ] For an operation with `refreshable: true`, decide your own polling
      interval if you want a live-updating value — the operator does
      nothing special here, it's the same invoke endpoint called again on
      whatever schedule you choose.

See `docs/catalog-parameters.md` → **Operations**.

## 1. DataSource / Visibility / Validation / per-template Version

- [ ] Stop reading `parameters[].source` — read `parameters[].dataSource.kind`
      instead (`"static"` | `"dynamic"` | `"system"`).
- [ ] Stop parsing `type` for a `"select_<key>"` prefix — dynamic selects are
      now always `type: "select"` plus `dataSource: {kind: "dynamic", sourceKey}`.
- [ ] Only render `static`/`dynamic`-sourced parameters as form fields; never
      render `system`-sourced ones.
- [ ] For `dynamic` parameters, call `resolveDynamicOptions` keyed by
      `dataSource.sourceKey`.
- [ ] Implement `visibility` evaluation: hide a parameter unless its
      `dependsOn` field's current value satisfies `op`/`value`/`values`.
- [ ] Optional: client-side `validation` checks (min/max/regex/maxLength) —
      the operator still enforces these authoritatively either way.
- [ ] Read `templates[].version` and store it alongside any preset you save;
      compare on load and decide mismatch behavior yourself (warn/block/
      auto-migrate) — the operator has no opinion here.
- [ ] No breaking change to the `/deploy` request shape itself. Error
      responses are plain text, not JSON — don't `JSON.parse` a 400 body.

See `docs/catalog-parameters.md` → **DataSource**, **Visibility**,
**Validation**, **Versioning**.
