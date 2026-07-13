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

// dynamicSelectPrefix marks a select parameter whose Options the operator
// deliberately leaves empty — Convex populates them per-request from its own
// database (see convex/operators/actions.ts#resolveDynamicOptions), keyed by
// the source name after the prefix. The operator has no R2/database
// credentials and doesn't know what belongs to which user, so it can only
// declare *that* a parameter needs a dynamic select and *which* source backs
// it, never resolve the options itself.
const dynamicSelectPrefix = "select_"

// DynamicSelectType builds a reusable "select_<sourceKey>" parameter type —
// the pattern any future dynamic select (e.g. "ssh_keys", "snapshots")
// reuses without needing a new ParameterType constant or any change on the
// Convex side beyond adding rows with that sourceKey.
func DynamicSelectType(sourceKey string) ParameterType {
	return ParameterType(dynamicSelectPrefix + sourceKey)
}

// ParameterSource distinguishes form fields a user fills in from values
// Convex computes itself and injects server-side. System-sourced parameters
// must never be rendered as an editable form field — see the profileDownloadUrl
// example in the firefox/chrome templates for why (an editable URL field
// there would let the operator's init container curl an arbitrary
// user-supplied URL, an SSRF risk).
type ParameterSource string

const (
	ParameterSourceUser   ParameterSource = "user"
	ParameterSourceSystem ParameterSource = "system"
)

// SelectOption is one choice for a ParameterTypeSelect parameter.
type SelectOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Parameter describes one configurable value a template accepts.
type Parameter struct {
	Key         string          `json:"key"`
	Label       string          `json:"label"`
	Description string          `json:"description,omitempty"`
	Type        ParameterType   `json:"type"`
	Source      ParameterSource `json:"source"`
	Required    bool            `json:"required"`
	Default     any             `json:"default,omitempty"`
	Options     []SelectOption  `json:"options,omitempty"`
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
	ID              string                                        `json:"id"`
	Name            string                                        `json:"name"`
	Description     string                                        `json:"description"`
	Icon            string                                        `json:"icon"`
	Parameters      []Parameter                                   `json:"parameters"`
	CustomFunctions []CustomFunction                              `json:"customFunctions,omitempty"`
	Build           func(params map[string]any) (Rendered, error) `json:"-"`
}
