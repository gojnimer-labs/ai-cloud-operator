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

// Package labels holds the app.kubernetes.io/managed-by label this operator
// stamps on every object it creates (Deployments/Services via the
// controller, Secrets via tokenstore/gateway.KeyStore) — shared so the key
// and value can't drift out of sync between the packages that set it.
package labels

const (
	ManagedBy      = "app.kubernetes.io/managed-by"
	ManagedByValue = "ai-cloud-operator"

	// WorkloadID correlates a claim-flow-created Workload CR back to the
	// Convex row that requested it, from the moment of Create() (when the
	// CR's real name doesn't exist yet, since it's still minted via
	// GenerateName) through its first successful upsert. Set by
	// internal/provisioning.WorkloadCreator.Create from the claim response's
	// workloadId, read back by internal/controller when reporting
	// ownership/lifecycle to Convex. Empty/absent for a manually
	// kubectl-created or legacy CR with no Convex row to correlate with.
	WorkloadID = "apps.aicloud.dev/workload-id"
)
