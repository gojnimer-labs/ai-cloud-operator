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
	// DataSourceFile is DataSourceSystem's value shape and every rule above
	// applies identically — same server-injected, never-editable string —
	// but specifically for a file to fetch or upload (a presigned S3/R2
	// URL), so template authors and the frontend have a clearer, more
	// specific label than the generic "system" for this common case.
	// Prefer this over DataSourceSystem whenever the value is a file URL.
	DataSourceFile DataSourceKind = "file"
	// DataSourceFileOptions marks a ParameterTypeSelect parameter whose
	// Options are files Convex resolves from its own files table, scoped by
	// DataSource.Group — the files-table counterpart to DataSourceDynamic/
	// selectOptions. Like DataSourceDynamic, the operator only declares
	// *that* a select needs this and *which* group backs it; Convex holds
	// the actual file records.
	DataSourceFileOptions DataSourceKind = "fileOptions"
)

// FileDirection distinguishes the two things a DataSourceFile parameter can
// mean for Convex: a target Convex mints fresh right before calling the
// operator (DirectionUpload, e.g. backup_state's uploadUrl) or a value
// Convex resolves from another parameter's already-selected row
// (DirectionDownload, e.g. profileDownloadUrl, resolved from profileName).
type FileDirection string

const (
	DirectionUpload   FileDirection = "upload"
	DirectionDownload FileDirection = "download"
)

// DataSource describes where a Parameter's value comes from. The zero value
// (Kind == "") is treated as DataSourceStatic by callers that care — but
// Template/Operation literals should set Kind explicitly for clarity.
type DataSource struct {
	Kind DataSourceKind `json:"kind"`
	// SourceKey names which Convex-side source resolves this parameter's
	// options. Only meaningful when Kind == DataSourceDynamic.
	SourceKey string `json:"sourceKey,omitempty"`

	// Direction — see FileDirection. Only meaningful when Kind == DataSourceFile.
	Direction FileDirection `json:"direction,omitempty"`
	// SourceParam names another Parameter.Key in the same Parameters list
	// whose resolved value is a files-table row id to resolve into a URL.
	// Only meaningful when Kind == DataSourceFile and Direction ==
	// DirectionDownload.
	SourceParam string `json:"sourceParam,omitempty"`
	// Group names which group of files this belongs to — for
	// DataSourceFileOptions, which group to list as this select's options;
	// for DataSourceFile with Direction == DirectionUpload, which group a
	// newly-uploaded file should be filed under (matching the
	// DataSourceFileOptions parameter that will later list it).
	Group string `json:"group,omitempty"`
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
// and captures its combined stdout/stderr. Operation.Run depends on this
// interface rather than a concrete Kubernetes client so the catalog package
// stays decoupled from client-go's exec/SPDY machinery — see
// internal/podexec for the real implementation and internal/api/server.go
// for how it's wired in.
type PodExecutor interface {
	Exec(ctx context.Context, namespace, podName, container string, command []string) (stdout, stderr string, err error)
}

// PodRef identifies which pod an Operation should run against — resolved by
// the caller (see internal/podexec.FindPod) before Run is invoked, since
// finding "the" pod for a workload is a live cluster lookup, not something
// the catalog package itself should own. Container isn't part of this: each
// Operation already knows which of its template's containers it targets
// (see backupStateFunction's containerName closure).
type PodRef struct {
	Namespace string
	PodName   string
}

// AdditionalInfoType says how a piece of an Operation's result should be
// handled/displayed downstream — currently just the secret/not-secret axis.
// Not named "opaque" for the non-secret case: this codebase already uses
// corev1.SecretTypeOpaque for real Kubernetes Secrets, where "Opaque" means
// "unstructured secret data" — the opposite of what we'd mean by it here.
type AdditionalInfoType string

const (
	// AdditionalInfoSecret is sensitive — mask by default, avoid logging or
	// persisting it in plaintext, etc.
	AdditionalInfoSecret AdditionalInfoType = "secret"
	// AdditionalInfoPlain is informational — display as-is, no special
	// handling.
	AdditionalInfoPlain AdditionalInfoType = "plain"
)

// AdditionalInfo is one named value an Operation's Run produces — always
// display data for the caller (secret/plain), never a processing
// directive. Unlike Parameter (an input schema), there's no separate static
// declaration of what an Operation's Outputs will be — the type travels
// with the value at the moment Run actually returns it, so there's no
// schema/instance drift to keep in sync.
type AdditionalInfo struct {
	Name  string             `json:"name"`
	Type  AdditionalInfoType `json:"type"`
	Value any                `json:"value"`
}

// FileResult is set on OperationResult when a Run call produced a file
// worth recording (e.g. a completed backup) — see backupStateFunction in
// browser.go for the only current producer. Convex creates the actual
// files-table row itself using data only it holds (the R2 key/bucket it
// minted before calling the operator); this only carries what the
// operator itself decided.
type FileResult struct {
	// Type is a free-form kind tag, e.g. "browser_profile_backup".
	Type string `json:"type"`
	// Label is the display name for the resulting file (e.g. a
	// user-supplied backup name).
	Label string `json:"label"`
}

// OperationResult is what Operation.Run returns.
type OperationResult struct {
	AdditionalInfo []AdditionalInfo `json:"additionalInfo"`
	// File is nil unless this call produced a file worth recording.
	File *FileResult `json:"file,omitempty"`
}

// Operation is a named operation a template exposes against an
// already-running workload — distinct from Template.Parameters, which only
// apply at deploy time. This is the reusable pattern any future operation
// (not just backup_state) follows: declare Parameters the same way a
// template does (including system-sourced ones Convex computes, e.g. an R2
// upload URL — see the firefox/chrome backup_state operation), and
// implement Run to do the actual work via the injected PodExecutor. The
// frontend discovers these the same way it discovers deploy-time
// parameters: they're part of the catalog response (see
// internal/api/server.go#handleCatalog).
type Operation struct {
	Key         string      `json:"key"`
	Label       string      `json:"label"`
	Description string      `json:"description,omitempty"`
	Parameters  []Parameter `json:"parameters"`
	// Refreshable marks this operation as side-effect-free — safe for a
	// caller to re-invoke on its own interval just to get a current
	// reading (e.g. reading a token file), as opposed to one like
	// backup_state that does real work and should only run when a user
	// deliberately triggers it.
	Refreshable bool                                                                                                    `json:"refreshable"`
	Run         func(ctx context.Context, exec PodExecutor, pod PodRef, params map[string]any) (OperationResult, error) `json:"-"`
}

// Entrypoint is one web entrypoint a template's Service exposes — catalog
// metadata only, independent of any specific Build() call's params. Name
// must match a corev1.ServicePort.Name a template's Build() actually
// produces for the gateway to route to it (enforced by
// TestEntrypointsMatchRenderedServicePorts) — see internal/gateway/proxy.go,
// which selects a Service port by this name.
type Entrypoint struct {
	Name  string `json:"name"`
	Label string `json:"label"`
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
	Version    string      `json:"version"`
	Parameters []Parameter `json:"parameters"`
	// Entrypoints lists every web entrypoint this template's Service
	// exposes. Every template must declare at least one — the gateway's
	// /gw/{name}/{entrypoint}/{subpath...} route always requires this
	// segment.
	Entrypoints []Entrypoint                                  `json:"entrypoints"`
	Operations  []Operation                                   `json:"operations,omitempty"`
	Build       func(params map[string]any) (Rendered, error) `json:"-"`
}
