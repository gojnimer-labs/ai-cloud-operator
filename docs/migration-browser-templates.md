# Migration: browser template fixes (file DataSource, per-browser profile catalogs, i18n backup response)

A focused migration guide for three related but independent changes to the
`firefox`/`chrome` templates. None of these touch deploy/gateway identity
(see `docs/migration-operator-owned-identity.md` for that, separate
change). Hand this to whoever/whatever is doing the ai-cloud-v2 side — for
the full field-by-field reference, see `docs/catalog-parameters.md`.

## 1. New `dataSource.kind: "file"`

Additive, not breaking on its own. `profileDownloadUrl` (firefox/chrome
deploy-time) and `uploadUrl` (`backup_state` operation) now report
`dataSource.kind: "file"` instead of `"system"`:

```jsonc
// Before
"dataSource": { "kind": "system" }

// After
"dataSource": { "kind": "file" }
```

`"file"` has the exact same rules as `"system"`: Convex computes and
injects the value server-side (a presigned S3/R2 URL), and it must never
be rendered as an editable form field. It's just a more specific label for
this common case, so treat it identically to `"system"` if you don't need
the extra specificity yet — a `case "system": case "file":` (or an
`in [...]` check) alongside your existing `"system"` handling is enough.

- [ ] Handle `dataSource.kind: "file"` wherever you currently handle
      `"system"`.

## 2. Firefox and Chrome no longer share a profile catalog

`profileName`'s `dataSource.sourceKey` was `"profiles_browser"` for **both**
templates — meaning a Firefox profile backup could show up as a restore
option when deploying Chrome (and vice versa), which would silently produce
a broken profile (the tarball layouts aren't compatible between browsers).
Each template now has its own key:

| Template | Old sourceKey | New sourceKey |
|---|---|---|
| `firefox` | `profiles_browser` | `profiles_firefox` |
| `chrome` | `profiles_browser` | `profiles_chrome` |

- [ ] Update wherever you call `resolveDynamicOptions` (or equivalent) for
      the `profileName` picker to use the new per-template key.
- [ ] If you store saved-profile records (selectOptions rows) keyed by the
      old shared `profiles_browser` key, split/re-key them by which browser
      each profile actually came from — there's no way to infer this after
      the fact from the key alone if you don't already track it, so do this
      before dropping the old key from your queries.
- [ ] Both templates' `version` bumped `1.0.0` → `1.1.0` to reflect this
      parameter shape change — update any stored preset comparisons.

## 3. `backup_state`'s response is now an i18n-able message key

```jsonc
// Before — raw shell stdout (in practice, usually empty: tar/curl both
// run silently, with no -v/-s output to surface)
{ "additionalInfo": [{ "name": "stdout", "type": "plain", "value": "" }] }

// After — a stable, namespaced key
{ "additionalInfo": [{ "name": "result", "type": "plain", "value": "backup_state.success" }] }
```

- [ ] Stop reading the `"stdout"` entry; read `"result"` instead.
- [ ] Treat `"backup_state.success"` as a lookup key into your own
      translation table, not display text — the response gives you no
      machine-readable signal that this particular `plain` value happens to
      be a key rather than literal text, so this is a convention specific
      to this operation (documented here and in the operation's own
      `description`), not something generically detectable.
- [ ] Failure is unchanged: a failed backup still surfaces as a real HTTP
      error (plain text body), not as an `additionalInfo` entry — nothing
      to update there.

## Ground truth, if you need to verify anything

`internal/catalog/types.go` (`DataSourceFile`), `internal/catalog/browser.go`
(`browserParameters`, `backupStateFunction`), `internal/catalog/firefox.go`,
`internal/catalog/chrome.go` — all in
`github.com/gojnimer-labs/ai-cloud-operator`.
