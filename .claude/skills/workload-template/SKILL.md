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
4. `catalog.ResolveParams(tmpl.Parameters, rawParams)` — applies defaults,
   errors on missing required values.
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
type Parameter struct {
	Key         string
	Label       string
	Description string          // optional
	Type        ParameterType   // ParameterTypeString | Number | Boolean | Select | DynamicSelectType("source")
	Source      ParameterSource // ParameterSourceUser | ParameterSourceSystem
	Required    bool
	Default     any             // omitted if nil; applied only when caller didn't supply the key
	Options     []SelectOption  // only for ParameterTypeSelect
}

type Rendered struct {
	Containers     []corev1.Container
	InitContainers []corev1.Container
	Volumes        []corev1.Volume
	ServicePorts   []corev1.ServicePort
}

type Template struct {
	ID              string
	Name            string
	Description     string
	Icon            string
	Parameters      []Parameter
	CustomFunctions []CustomFunction // optional, see "Advanced" below
	Build           func(params map[string]any) (Rendered, error)
}
```

`ResolveParams` (`internal/catalog/registry.go`) is what turns raw request
params into what `Build` receives:

```go
func ResolveParams(params []Parameter, raw map[string]any) (map[string]any, error) {
	resolved := make(map[string]any, len(params))
	maps.Copy(resolved, raw) // raw itself is never mutated
	for _, p := range params {
		if _, ok := resolved[p.Key]; !ok && p.Default != nil {
			resolved[p.Key] = p.Default
		}
		if p.Required {
			if v, ok := resolved[p.Key]; !ok || v == nil || v == "" {
				return nil, fmt.Errorf("missing required parameter %q", p.Key)
			}
		}
	}
	return resolved, nil
}
```

**Gotcha**: the required-check treats `""` as "missing" too (not just absent/nil).
Don't mark a parameter `Required: true` unless a real value is guaranteed to
exist by the time `Build` runs — that's why `profileDownloadUrl` (system,
optional — restore may not be requested) and `uploadUrl` (system, but always
required for that specific custom function call) differ in `Required` even
though both are `ParameterSourceSystem`.

Helpers already in `internal/catalog/registry.go`, available package-wide —
use them instead of re-deriving:
- `paramString(params, key, fallback string) string`
- `paramInt32(params, key string, fallback int32) int32` — handles `float64`
  (the JSON-decoded case), `int32`, and `int` (the case when a test constructs
  params directly).
- `int32ToString(v int32) string`

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
	ID:          templateIDRedis,
	Icon:        "🟥",
	Name:        "Redis",
	Parameters: []Parameter{
		{
			Default:     "256mb",
			Description: "Passed as --maxmemory.",
			Key:         "maxMemory",
			Label:       "Max memory",
			Required:    false,
			Source:      ParameterSourceUser,
			Type:        ParameterTypeString,
		},
		{
			Description: "Optional; passed as --requirepass. Leave blank for no auth (acceptable for a private in-cluster instance).",
			Key:         "password",
			Label:       "Password",
			Required:    false,
			Source:      ParameterSourceUser,
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
```

Also add `templateIDRedis` to the id list in `TestGetReturnsKnownTemplates`.

Then: `go build ./... && go test ./internal/catalog/...`

## Constraints and gotchas

- **Container naming convention**: every existing template names its primary
  container after the template ID (`nginx`, `firefox`, `chrome`). Keep doing
  this — a `CustomFunction.Run` (see Advanced below) targets a container by a
  hardcoded name closed over at registration time, so drifting the container
  name from the template ID makes wiring a future custom function for this
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
  Build time.
- **System- vs user-sourced parameters**: `ParameterSourceSystem` means Convex
  computes and injects the value server-side (e.g. a presigned URL) — it must
  never be rendered as an editable form field, and your `Build`/init-container
  script must treat it as untrusted-ish data (pass via env var / positional
  arg, never string-interpolate into a shell script — see
  `restoreProfileInitContainer` and `backupStateFunction` in
  `internal/catalog/browser.go` for the reasoning and the pattern to copy).
- **Reuse `internal/catalog/browser.go` helpers when they fit, don't force
  it**: `browserResources(cpu, memRequest, memLimit string)` is generic enough
  for any container despite the name (used above for redis). `browserProbe`
  and `restoreProfileInitContainer` are hard-coded to firefox/chrome's port
  and profile-restore shape — only reach for them if you're building another
  "restore a profile from an R2 tarball into a browser-like image" template;
  otherwise just write your own `corev1.Probe`/init container inline in your
  new file, the way `nginx.go` needs neither.
- **Struct-literal field order**: existing `Template`/`Parameter` literals are
  mostly alphabetical by field name (`Build, CustomFunctions, Description, ID,
  Icon, Name, Parameters`; `Default, Description, Key, Label, Required,
  Source, Type`) — not gofmt-enforced, just house style; match it for
  reviewability, but don't worry if a comment forces you to break it (see
  `profileName`'s `Parameter` in `browser.go`, which doesn't).

## Advanced (optional): CustomFunction

Beyond deploy-time `Parameters`, a `Template` can expose named operations
against an **already-running** workload via `CustomFunctions
[]CustomFunction`:

```go
type CustomFunction struct {
	Key         string
	Label       string
	Description string
	Parameters  []Parameter // same shape/rules as Template.Parameters
	Run         func(ctx context.Context, exec PodExecutor, pod PodRef, params map[string]any) (map[string]any, error)
}
```

The only current example is `backupStateFunction` in
`internal/catalog/browser.go`, shared by firefox and chrome as `"backup_state"`
— it execs a `tar` + `curl PUT` inside the running container via the injected
`PodExecutor` (decoupled from client-go's exec/SPDY machinery so the catalog
package stays test-friendly; see `fakePodExecutor` in `catalog_test.go`).
Reach for this only when a template needs an operation against a *live* pod
(backup/restore/reset-style actions), not for anything expressible as deploy
time config — that belongs in `Template.Parameters` + `Build` instead.
