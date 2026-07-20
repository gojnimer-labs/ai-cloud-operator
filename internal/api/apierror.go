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

package api

import (
	"encoding/json"
	"net/http"
)

// Stable, namespaced error codes for every response this server's handlers
// write via writeError — the same "stable, namespaced message key"
// convention ai-cloud-operator's catalog function results already use (see
// docs/catalog-parameters.md), extended to this HTTP API. `code` is what a
// consumer (ai-cloud-v2's Convex backend, then its frontend) keys a
// translated message off; `message` stays the original English text purely
// for logs/debugging and is not meant to be shown to an end user directly.
//
// This is purely additive: as of this change, ai-cloud-v2's Convex client
// only branches on the HTTP status code for these calls and never parses
// the response body on failure, so adding `code` alongside the existing
// plain-text body's replacement changes nothing about today's behavior.
const (
	codeAuthMissingToken       = "auth.missing_token"
	codeAuthInvalidToken       = "auth.invalid_token"
	codeRequestInvalidJSON     = "request.invalid_json"
	codeWorkloadNotFound       = "workload.not_found"
	codeWorkloadImageRequired  = "workload.image_required"
	codeWorkloadUserIDRequired = "workload.user_id_required"
	codeWorkloadNameRequired   = "workload.name_required"
	codeWorkloadInvalidName    = "workload.invalid_name"
	codeWorkloadInvalidConfig  = "workload.invalid_config"
	codeWorkloadNoRunningPod   = "workload.no_running_pod"
	codeCatalogTemplateUnknown = "catalog.template_unknown"
	codeCatalogFunctionUnknown = "catalog.function_unknown"
	codeCatalogInvalidParams   = "catalog.invalid_params"
	codeCatalogFunctionFailed  = "catalog.function_failed"
	// codeInternalError is the fallback for every failure mode not specific
	// enough to be worth its own code (Get/apply/delete against the k8s API
	// failing, signing a gateway cookie failing, etc.) — the frontend shows
	// a generic "something went wrong" for this one, same as Convex's own
	// generic-fallback codes.
	codeInternalError = "internal.error"
)

// errorResponse is the JSON body writeError sends on every non-2xx
// response from this server.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError replaces this package's former http.Error(w, "text", status)
// calls: same status code and same message text (still readable in a
// browser/curl or the operator's own logs), plus a stable `code` a
// programmatic caller can key a translated message off instead of
// pattern-matching `message`.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Code: code, Message: message})
}
