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

// Package catalog is the registry of deployable workload templates (nginx,
// firefox, chrome, ...). Each template declares its parameters — including
// whether a parameter is user-supplied (rendered as a form field by the
// frontend) or system-computed (filled in by Convex using credentials/business
// logic the operator never holds, e.g. an R2 presigned URL) — and a Build
// function that turns resolved parameter values into the PodSpec fragments
// and Service ports the reconciler assembles into a Deployment + Service.
//
// The operator is the single source of truth for "what does a firefox
// workload actually need" (image, ports, init containers, probes); Convex
// only ever supplies opaque parameter values via Workload.Spec.Config.
package catalog

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// ParameterType tells the frontend which form widget to render.
type ParameterType string

const (
	ParameterTypeString  ParameterType = "string"
	ParameterTypeNumber  ParameterType = "number"
	ParameterTypeBoolean ParameterType = "boolean"
	ParameterTypeSelect  ParameterType = "select"
)

// DataSourceKind says where a parameter's value (or, for a select, its
// Options) actually comes from — a different axis than ParameterType, which
// only says what widget renders.
type DataSourceKind string

const (
	// DataSourceStatic is the default: the operator's own Options (if any)
	// are complete, and the user fills in the value via a rendered form
	// field.
	DataSourceStatic DataSourceKind = "static"
	// DataSourceDynamic marks a parameter (almost always a
	// ParameterTypeSelect) whose Options the operator deliberately leaves
	// empty — Convex populates them per-request from its own database (see
	// convex/operators/actions.ts#resolveDynamicOptions), keyed by
	// SourceKey. The operator has no R2/database credentials and doesn't
	// know what belongs to which user, so it can only declare *that* a
	// parameter needs a dynamic select and *which* source backs it, never
	// resolve the options itself.
	DataSourceDynamic DataSourceKind = "dynamic"
	// DataSourceSystem marks a value Convex computes itself and injects
	// server-side. Must never be rendered as an editable form field — see
	// the profileDownloadUrl example in the firefox/chrome templates for
	// why (an editable URL field there would let the operator's init
	// container curl an arbitrary user-supplied URL, an SSRF risk).
	DataSourceSystem DataSourceKind = "system"
)

// DataSource describes where a Parameter's value comes from. The zero value
// (Kind == "") is treated as DataSourceStatic by callers that care — but
// Template/CustomFunction literals should set Kind explicitly for clarity.
type DataSource struct {
	Kind DataSourceKind `json:"kind"`
	// SourceKey names which Convex-side source resolves this parameter's
	// options. Only meaningful when Kind == DataSourceDynamic.
	SourceKey string `json:"sourceKey,omitempty"`
}

// VisibilityOp is the comparison a Visibility condition applies to the
// parameter it depends on.
type VisibilityOp string

const (
	VisibilityEquals    VisibilityOp = "equals"
	VisibilityNotEquals VisibilityOp = "notEquals"
	VisibilityOneOf     VisibilityOp = "oneOf"
)

// Visibility makes a parameter's visibility conditional on another
// parameter's resolved value — deliberately a single condition, not a
// boolean expression tree: every case in this catalog today (and every case
// anticipated) is "show this field only if that other field has/doesn't
// have/is one of these values," and a real expression language would be a
// lot of machinery (and surface area) for a need that hasn't shown up yet.
type Visibility struct {
	// DependsOn is another Parameter.Key in the same list.
	DependsOn string       `json:"dependsOn"`
	Op        VisibilityOp `json:"op"`
	// Value is compared against for VisibilityEquals/VisibilityNotEquals.
	Value any `json:"value,omitempty"`
	// Values is the candidate set for VisibilityOneOf.
	Values []any `json:"values,omitempty"`
}

// Validation constrains a parameter's resolved value beyond presence
// (Required). Every field is optional; nil/zero means "no constraint of
// this kind." Only checked for parameters that are both visible and have a
// value present — see ResolveParams.
type Validation struct {
	Min       *float64 `json:"min,omitempty"`
	Max       *float64 `json:"max,omitempty"`
	Regex     string   `json:"regex,omitempty"`
	MaxLength *int     `json:"maxLength,omitempty"`
}

// SelectOption is one choice for a ParameterTypeSelect parameter.
type SelectOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Parameter describes one configurable value a template accepts.
type Parameter struct {
	Key         string         `json:"key"`
	Label       string         `json:"label"`
	Description string         `json:"description,omitempty"`
	Type        ParameterType  `json:"type"`
	DataSource  DataSource     `json:"dataSource"`
	Required    bool           `json:"required"`
	Default     any            `json:"default,omitempty"`
	Options     []SelectOption `json:"options,omitempty"`
	// Visibility, when set, hides this parameter (and exempts it from
	// Required/Validation checks) unless the condition holds — see
	// ResolveParams.
	Visibility *Visibility `json:"visibility,omitempty"`
	Validation *Validation `json:"validation,omitempty"`
}

// Rendered is what a template's Build function produces: the pieces the
// reconciler plugs into a Deployment's PodSpec and a Service's ports.
type Rendered struct {
	Containers     []corev1.Container
	InitContainers []corev1.Container
	Volumes        []corev1.Volume
	ServicePorts   []corev1.ServicePort
}

// PodExecutor runs a command inside a specific container of a running pod
// and captures its combined stdout/stderr. CustomFunction.Run depends on
// this interface rather than a concrete Kubernetes client so the catalog
// package stays decoupled from client-go's exec/SPDY machinery — see
// internal/podexec for the real implementation and internal/api/server.go
// for how it's wired in.
type PodExecutor interface {
	Exec(ctx context.Context, namespace, podName, container string, command []string) (stdout, stderr string, err error)
}

// PodRef identifies which pod a CustomFunction should run against —
// resolved by the caller (see internal/podexec.FindPod) before Run is
// invoked, since finding "the" pod for a workload is a live cluster lookup,
// not something the catalog package itself should own. Container isn't part
// of this: each CustomFunction already knows which of its template's
// containers it targets (see backupStateFunction's containerName closure).
type PodRef struct {
	Namespace string
	PodName   string
}

// CustomFunction is a named operation a template exposes against an
// already-running workload — distinct from Template.Parameters, which only
// apply at deploy time. This is the reusable pattern any future custom
// function (not just backup_state) follows: declare Parameters the same way
// a template does (including system-sourced ones Convex computes, e.g. an R2
// upload URL — see the firefox/chrome backup_state function), and implement
// Run to do the actual work via the injected PodExecutor. The frontend
// discovers these the same way it discovers deploy-time parameters: they're
// part of the catalog response (see internal/api/server.go#handleCatalog).
type CustomFunction struct {
	Key         string                                                                                                 `json:"key"`
	Label       string                                                                                                 `json:"label"`
	Description string                                                                                                 `json:"description,omitempty"`
	Parameters  []Parameter                                                                                            `json:"parameters"`
	Run         func(ctx context.Context, exec PodExecutor, pod PodRef, params map[string]any) (map[string]any, error) `json:"-"`
}

// Template is one entry in the catalog.
type Template struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	// Version is a plain, manually-bumped string (semver-shaped by
	// convention, e.g. "1.0.0") — bump it whenever this template's
	// Parameters change. Purely informational: the operator never reads or
	// enforces it itself. It exists so Convex-side presets (a saved
	// parameter set built against this template at some point in time) can
	// detect that the schema they were built against has since moved,
	// entirely on the Convex side — the operator has no opinion on what a
	// mismatch should mean.
	Version         string                                        `json:"version"`
	Parameters      []Parameter                                   `json:"parameters"`
	CustomFunctions []CustomFunction                              `json:"customFunctions,omitempty"`
	Build           func(params map[string]any) (Rendered, error) `json:"-"`
}
