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

import corev1 "k8s.io/api/core/v1"

// ParameterType tells the frontend which form widget to render.
type ParameterType string

const (
	ParameterTypeString  ParameterType = "string"
	ParameterTypeNumber  ParameterType = "number"
	ParameterTypeBoolean ParameterType = "boolean"
	ParameterTypeSelect  ParameterType = "select"
)

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

// Template is one entry in the catalog.
type Template struct {
	ID          string                                        `json:"id"`
	Name        string                                        `json:"name"`
	Description string                                        `json:"description"`
	Icon        string                                        `json:"icon"`
	Parameters  []Parameter                                   `json:"parameters"`
	Build       func(params map[string]any) (Rendered, error) `json:"-"`
}
