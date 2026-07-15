---
name: workload-template
description: Use when asked to add a new catalog workload template to this operator (e.g. "add a new workload template", "new catalog template", "support running redis/postgres/<X> as a workload") — walks through internal/catalog Template/Parameter/Build/registry and testing conventions.
---

# Adding a catalog workload template

A "template" is a pre-built container spec a user can pick via
`Workload.spec.templateName` instead of hand-writing `spec.image` /
`spec.containerPort` / `spec.env`. All templates live in `internal/catalog/`.

## How it plugs into the reconciler (read this first)

`internal/controller/workload_controller.go`'s `render` function:

1. If `Spec.TemplateName == ""` → legacy raw-image path (`Spec.Image` required),
   builds one container literally named `"workload"`. Not your concern here.
2. If set → `catalog.Get(name)` (linear scan over the `templates` slice in
   `internal/catalog/registry.go`); unknown name → immediate
   `fmt.Errorf("unknown template %q", ...)`, surfaced as a reconcile error.
3. `configToParams(workload.Spec.Config)` — `Spec.Config` is an
   `*apiextensionsv1.JSON` (`+kubebuilder:pruning:PreserveUnknownFields`, plain
   passthrough, see `api/v1alpha1/workload_types.go`). Nil/empty → `map[string]any{}`
   (not an error — every param falls back to its default). Otherwise it's a
   plain `json.Unmarshal` into `map[string]any`, so **numbers arrive as
   `float64`**, not `int`.
4. `catalog.ResolveParams(tmpl.Parameters, rawParams)` — applies defaults, then
   evaluates `Visibility`, then checks `Required`/`Validation` on whatever's
   still visible (see below).
5. `tmpl.Build(resolvedParams)` → `catalog.Rendered{Containers, InitContainers,
   Volumes, ServicePorts}`.
6. The reconciler does a **direct, unconditional assignment** — no merging, no
   name normalization: `deployment.Spec.Template.Spec.Containers =
   rendered.Containers` (same for `InitContainers`/`Volumes`), and
   `service.Spec.Ports = rendered.ServicePorts`. Whatever container/port names
   your `Build` sets are exactly what lands in the cluster.

**Confirmed: no CRD/RBAC/manifest change is needed to add a template.**
`templateName` in the CRD (`config/crd/bases/apps.aicloud.dev_workloads.yaml`,
mirrored in `charts/ai-cloud-operator/templates/workload-crd.yaml`) is a plain
`type: string` with no enum restricting values. Adding a template is a pure Go
change: a new file in `internal/catalog/` plus one line in
`internal/catalog/registry.go`.

`catalog.ResolveParams` is also called from `internal/api/server.go`'s
`handleDeploy` (validates before creating the Workload CR, 400s on error, but
discards the resolved map — only raw `Config` gets stored) and
`handleRunFunction` (same validation, for `Operation.Parameters`) — three
call sites total, all sharing this one function.

## Walkthrough

1. **Create `internal/catalog/<name>.go`.** Package `catalog`, same license
   header as the other files in that dir. Define a package-level `var <Name> =
   Template{...}` (see struct shapes below).
2. **Register it** — add the value to the `templates` slice in
   `internal/catalog/registry.go` (currently `[]Template{Nginx, Firefox,
   Chrome}`). That's the entire "registration" mechanism — `Get`/`List` just
   read this slice.
3. **Add tests** to `internal/catalog/catalog_test.go` following the existing
   pattern (see Testing section below).
4. **Verify**: `go build ./... && go test ./internal/catalog/...`

## The struct shapes (from `internal/catalog/types.go`)

```go
type DataSourceKind string
const (
	DataSourceStatic  DataSourceKind = "static"  // inline Options (or none); user fills the form — default
	DataSourceDynamic DataSourceKind = "dynamic" // Convex resolves Options by SourceKey at request time
	DataSourceSystem  DataSourceKind = "system"  // Convex computes the value and injects it directly; never a form field
)
type DataSource struct {
	Kind      DataSourceKind
	SourceKey string // only meaningful when Kind == DataSourceDynamic
}

type VisibilityOp string // "equals" | "notEquals" | "oneOf"
type Visibility struct {
	DependsOn string       // another Parameter.Key in the same list
	Op        VisibilityOp
	Value     any   // equals/notEquals
	Values    []any // oneOf
}

type Validation struct {
	Min, Max  *float64 // numeric range
	Regex     string   // string pattern
	MaxLength *int     // string length
}

type Parameter struct {
	Key, Label, Description string
	Type        ParameterType // ParameterTypeString | Number | Boolean | Select
	DataSource  DataSource
	Required    bool
	Default     any            // omitted if nil; applied only when caller didn't supply the key
	Options     []SelectOption // only for ParameterTypeSelect
	Visibility  *Visibility    // nil = always visible/enforced
	Validation  *Validation    // nil = no constraint beyond Required
}

type Rendered struct {
	Containers     []corev1.Container
	InitContainers []corev1.Container
	Volumes        []corev1.Volume
	ServicePorts   []corev1.ServicePort
}

type Entrypoint struct {
	Name  string // must match a corev1.ServicePort.Name your Build() actually returns
	Label string
}

type Template struct {
	ID, Name, Description, Icon string
	Version     string // manually bumped, e.g. "1.0.0" — see Versioning below
	Parameters  []Parameter
	Entrypoints []Entrypoint // required, at least one — see below
	Operations  []Operation  // optional, see "Advanced" below
	Build       func(params map[string]any) (Rendered, error)
}
```

**`Entrypoints` is required, not optional** — every template needs at least
one, even if it only has a single port. Each `Entrypoint.Name` must equal the
`Name` of a `corev1.ServicePort` your `Build()` actually returns; this is
checked by `TestEntrypointsMatchRenderedServicePorts` in
`internal/catalog/catalog_test.go`, which runs against every template in the
registry — forgetting to add an `Entrypoint`, or misspelling its `Name`
relative to your `ServicePorts`, fails that test immediately. This is what
lets the gateway route `/gw/{namespace}/{name}/{entrypoint}/{subpath...}`
requests to the right port by name instead of always using the first one —
see `internal/gateway/proxy.go`. Only declare an `Entrypoint` for a port that
actually speaks HTTP; the gateway proxies over HTTP, so a non-HTTP port (a
raw TCP protocol like Redis's) can still exist as a `ServicePort` without a
corresponding `Entrypoint` meant for browser use — but note the current rule
requires at least one `Entrypoint` on every template regardless, so a
template with no genuinely browsable port still needs to declare one
pointing at whichever port makes the most sense as its primary identifier.

`ResolveParams` (`internal/catalog/registry.go`) turns raw request params into
what `Build` receives, in two passes:

```
pass 1 (defaults): resolved = copy(raw); for each p where resolved[p.Key] is
  absent and p.Default != nil: resolved[p.Key] = p.Default

pass 2 (visibility, then required, then validation) — needs pass 1's full map
  since Visibility can depend on another parameter's resolved/defaulted value:
  for each p:
    if p.Visibility != nil and it evaluates to "not visible": skip Required
      and Validation entirely for this p (its value, if any, is left in the
      map untouched — not stripped)
    if p.Required and resolved[p.Key] is missing/nil/"": error
    if p.Validation != nil and a value is present: check Min/Max (numeric) or
      Regex/MaxLength (string), error on violation
```

**Gotcha**: a hidden parameter (its `Visibility` condition doesn't hold) is
never enforced as required or validated — nothing rendered a form field for
it, so demanding a value would be unsatisfiable. A *visible* required
parameter still treats `""`/`nil` as "missing" (not just absent) — don't mark
a parameter `Required: true` unless a real value is guaranteed by the time
`Build` runs (see `profileDownloadUrl` below: not required, because a restore
may not even be requested).

Helpers already in `internal/catalog/registry.go`, available package-wide —
use them instead of re-deriving:
- `paramString(params, key, fallback string) string`
- `paramInt32(params, key string, fallback int32) int32` — handles `float64`
  (the JSON-decoded case), `int32`, and `int` (the case when a test constructs
  params directly).
- `int32ToString(v int32) string`
- `ptrFloat64(v float64) *float64` — for populating `Validation.Min`/`Max`
  struct literals from a bare number.

## Versioning

`Template.Version` is a plain string (semver-shaped by convention, e.g.
`"1.0.0"`), bumped by hand whenever *that template's* `Parameters` change.
It's purely informational — the operator never reads or enforces it, and
there's no version field on the deploy request. It exists so Convex-side
presets (a saved parameter set built against a template at some point) can
detect that the schema has since moved, entirely on the Convex side. When you
change an existing template's `Parameters` (add/remove/rename a key, change a
`Required`/`Type`/`Visibility` in a way that could break a saved preset),
bump `Version`. Adding a brand-new template starts at `"1.0.0"`.

## Worked example: a "redis" template

`internal/catalog/redis.go`:

```go
/*
Copyright 2026 gojnimer-labs.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package catalog

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	templateIDRedis = "redis"
	redisPort       = int32(6379)
)

// Redis deploys a single-node Redis server: no init container, no custom
// functions — the simplest non-trivial shape (one container, one port, a
// couple of user-tunable startup flags).
var Redis = Template{
	Build: func(params map[string]any) (Rendered, error) {
		maxMemory := paramString(params, "maxMemory", "256mb")
		password := paramString(params, "password", "")

		args := []string{"--maxmemory", maxMemory, "--maxmemory-policy", "allkeys-lru"}
		if password != "" {
			args = append(args, "--requirepass", password)
		}

		return Rendered{
			Containers: []corev1.Container{
				{
					Args:      args,
					Image:     "redis:7-alpine",
					Name:      templateIDRedis,
					Ports:     []corev1.ContainerPort{{ContainerPort: redisPort, Name: "redis"}},
					Resources: browserResources("500m", "256Mi", "512Mi"),
				},
			},
			ServicePorts: []corev1.ServicePort{
				{Name: "redis", Port: redisPort, TargetPort: intstr.FromInt32(redisPort)},
			},
		}, nil
	},
	Description: "Single-node Redis in-memory data store",
	Entrypoints: []Entrypoint{{Name: "redis", Label: "Redis"}},
	ID:          templateIDRedis,
	Icon:        "🟥",
	Name:        "Redis",
	Version:     "1.0.0",
	Parameters: []Parameter{
		{
			Default:     "256mb",
			Description: "Passed as --maxmemory.",
			Key:         "maxMemory",
			Label:       "Max memory",
			Required:    false,
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
			Validation:  &Validation{Regex: `^\d+(kb|mb|gb)$`},
		},
		{
			Description: "Optional; passed as --requirepass. Leave blank for no auth (acceptable for a private in-cluster instance).",
			Key:         "password",
			Label:       "Password",
			Required:    false,
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
	},
}
```

Then in `internal/catalog/registry.go`:

```go
var templates = []Template{
	Nginx,
	Firefox,
	Chrome,
	Redis,
}
```

No separate test is needed to check your new template's `Entrypoints` are
consistent with its `Build()`'s `ServicePorts` —
`TestEntrypointsMatchRenderedServicePorts` already iterates every template in
`templates` and checks this for you; it'll fail immediately if you forget an
`Entrypoint` or misspell its `Name`.

Test additions to `internal/catalog/catalog_test.go` (follow the existing
per-behavior-function style, not table tests):

```go
func TestRedisBuildAppliesMaxMemoryAndPassword(t *testing.T) {
	tmpl, _ := Get(templateIDRedis)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{"password": "hunter2"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rendered, err := tmpl.Build(resolved)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	args := rendered.Containers[0].Args
	if len(args) < 2 || args[len(args)-2] != "--requirepass" || args[len(args)-1] != "hunter2" {
		t.Fatalf("expected --requirepass hunter2 in args, got %+v", args)
	}
}

func TestRedisBuildDefaultsMaxMemoryWithoutPassword(t *testing.T) {
	tmpl, _ := Get(templateIDRedis)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved["maxMemory"] != "256mb" {
		t.Fatalf("expected default maxMemory=256mb, got %v", resolved["maxMemory"])
	}
	rendered, err := tmpl.Build(resolved)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, a := range rendered.Containers[0].Args {
		if a == "--requirepass" {
			t.Fatalf("did not expect --requirepass when password is blank, got %+v", rendered.Containers[0].Args)
		}
	}
}

func TestRedisRejectsInvalidMaxMemory(t *testing.T) {
	tmpl, _ := Get(templateIDRedis)
	if _, err := ResolveParams(tmpl.Parameters, map[string]any{"maxMemory": "lots"}); err == nil {
		t.Fatalf("expected an error for a maxMemory value that doesn't match the regex")
	}
}
```

Also add `templateIDRedis` to the id list in `TestGetReturnsKnownTemplates`.

Then: `go build ./... && go test ./internal/catalog/...`

## Constraints and gotchas

- **Container naming convention**: every existing template names its primary
  container after the template ID (`nginx`, `firefox`, `chrome`). Keep doing
  this — an `Operation.Run` (see Advanced below) targets a container by a
  hardcoded name closed over at registration time, so drifting the container
  name from the template ID makes wiring a future operation for this
  template error-prone. There is no automatic normalization; the reconciler
  copies `rendered.Containers` verbatim.
- **Port/service wiring is entirely manual**: nothing auto-derives
  `ServicePorts` from `Containers[].Ports`. If you add a `ContainerPort`, you
  must add a matching `corev1.ServicePort` yourself (see `redisPort` used in
  both places above) or the Service won't route to it.
- **Defaults and JSON typing**: `Default` values you write as Go literals in
  the `Parameter` (e.g. `Default: float64(1024)` for a number) must match the
  type `paramString`/`paramInt32` (or your own reader) expects, because
  real requests decode JSON numbers as `float64`. Follow `nginx.go`'s
  `workerConnections` example.
- **`Required: true` + `""`/`nil` are both treated as "missing"** by
  `ResolveParams` — don't require a field that's legitimately optional at
  Build time. Combine with `Visibility` when a field is only ever relevant
  conditionally (see `profileDownloadUrl` below) rather than leaving it
  optional-in-name-only.
- **System-sourced parameters** (`DataSource{Kind: DataSourceSystem}`): Convex
  computes and injects the value server-side (e.g. a presigned URL) — it must
  never be rendered as an editable form field, and your `Build`/init-container
  script must treat it as untrusted-ish data (pass via env var / positional
  arg, never string-interpolate into a shell script — see
  `restoreProfileInitContainer` and `backupStateFunction` in
  `internal/catalog/browser.go` for the reasoning and the pattern to copy).
  `profileDownloadUrl` also demonstrates pairing a system parameter with
  `Visibility` — it's only meaningful (and only enforced/validated) when
  `restoreProfile == true`.
- **Dynamic-select parameters** (`DataSource{Kind: DataSourceDynamic,
  SourceKey: "..."}`): leave `Options` empty — Convex resolves the actual
  choices per-request from its own database keyed by `SourceKey` (see
  `profileName` in `browser.go`). The operator declares *that* a dynamic
  select is needed and *which* source backs it, never the options themselves.
- **Reuse `internal/catalog/browser.go` helpers when they fit, don't force
  it**: `browserResources(cpu, memRequest, memLimit string)` is generic enough
  for any container despite the name (used above for redis). `browserProbe`
  and `restoreProfileInitContainer` are hard-coded to firefox/chrome's port
  and profile-restore shape — only reach for them if you're building another
  "restore a profile from an R2 tarball into a browser-like image" template;
  otherwise just write your own `corev1.Probe`/init container inline in your
  new file, the way `nginx.go` needs neither.
- **Struct-literal field order**: existing `Template`/`Parameter` literals are
  mostly alphabetical by field name (`Build, Description, Entrypoints, ID,
  Icon, Name, Operations, Parameters, Version`; `DataSource, Default,
  Description, Key, Label, Required, Type, Validation, Visibility`) — not
  gofmt-enforced, just house style; match it for reviewability, but don't
  worry if a comment forces you to break it.

## Advanced (optional): Operation

Beyond deploy-time `Parameters`, a `Template` can expose named operations
against an **already-running** workload via `Operations []Operation`:

```go
type Operation struct {
	Key         string
	Label       string
	Description string
	Parameters  []Parameter // same shape/rules as Template.Parameters
	// Refreshable marks this operation as side-effect-free — safe to
	// re-invoke on an interval for a fresh reading (e.g. reading a token
	// file), as opposed to one that does real work and should only run
	// when a user deliberately triggers it.
	Refreshable bool
	Run         func(ctx context.Context, exec PodExecutor, pod PodRef, params map[string]any) ([]AdditionalInfo, error)
}

type AdditionalInfoType string // "secret" | "plain"
type AdditionalInfo struct {
	Name  string
	Type  AdditionalInfoType
	Value any
}
```

`Run` returns `[]AdditionalInfo` rather than a bare map — each named value
says whether it's `secret` (sensitive, e.g. a bearer token — should be
masked/handled carefully downstream) or `plain` (display as-is, no special
handling; not called `opaque` — this codebase already uses
`corev1.SecretTypeOpaque` for real Kubernetes Secrets, a different meaning).
There's deliberately no separate static schema declaring what an operation's
outputs *will* be — the type travels with the value at the moment `Run`
actually returns it, so there's nothing to drift out of sync.

The only current example is `backupStateFunction` in
`internal/catalog/browser.go`, shared by firefox and chrome as `"backup_state"`
— it execs a `tar` + `curl PUT` inside the running container via the injected
`PodExecutor` (decoupled from client-go's exec/SPDY machinery so the catalog
package stays test-friendly; see `fakePodExecutor` in `catalog_test.go`),
returning `[]AdditionalInfo{{Name: "stdout", Type: AdditionalInfoPlain, Value: stdout}}`.
It sets `Refreshable: false` since it has a real side effect (uploads to R2)
— re-invoking it isn't something a caller should do just to check a value.
An operation that only *reads* something already computed (e.g. `cat`-ing a
token file the container generated at boot) should set `Refreshable: true`
and return that value with `Type: AdditionalInfoSecret` — the caller (Convex)
decides its own polling interval for refreshable operations; nothing on the
operator side enforces or rate-limits this.

Reach for an Operation only when a template needs to do something against a
*live* pod (backup/restore/reset-style actions, or reading a runtime-generated
value like a bearer token), not for anything expressible as deploy time
config — that belongs in `Template.Parameters` + `Build` instead.
