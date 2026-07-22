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

// Package workloadns ensures the single, fixed Kubernetes namespace every
// workload this operator manages gets deployed into (WORKLOAD_NAMESPACE)
// actually exists — an install-time config value, not something Convex
// chooses or sends per-request (see internal/api.Server).
package workloadns

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// podSecurityLabels pin the workload namespace to the "baseline" Pod
// Security Standard, regardless of whatever a cluster-wide default policy
// (common on hardened distros — this project targets an on-prem Talos
// cluster via core-d, see docs/argocd-helm-deploy.md) would otherwise
// impose on a freshly-created, unlabeled namespace. "restricted" is too
// strict for what actually runs here: the browser/desktop catalog
// templates (firefox/chrome/webtop, all linuxserver.io-based images — see
// internal/catalog) and the shared restoreProfileInitContainer all need to
// start as root (s6-overlay init, `chown` before dropping to PUID/PGID),
// which "restricted" flatly forbids — pods just silently never reach Ready,
// with no error the operator itself can see (PodSecurity's Pod-level
// enforcement is a separate admission check from the warning attached to
// this Namespace's own Deployment writes, which only reports "would
// violate" advisories, not the hard rejection those warnings imply is
// still happening downstream). "baseline" still blocks the things that
// actually matter for isolating untrusted, user-supplied workload
// config — host namespace/network access, privileged containers, hostPath
// volumes — while allowing root-in-container.
const podSecurityLevelBaseline = "baseline"

var podSecurityLabels = map[string]string{
	"pod-security.kubernetes.io/enforce": podSecurityLevelBaseline,
	"pod-security.kubernetes.io/audit":   podSecurityLevelBaseline,
	"pod-security.kubernetes.io/warn":    podSecurityLevelBaseline,
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=create;patch

// EnsureNamespace creates the Namespace named name (with podSecurityLabels
// already set) if it doesn't already exist, and merge-patches those same
// labels onto it otherwise — covering a namespace created by an earlier
// version of this operator (before podSecurityLabels existed) or out of
// band. Both the blind Create and the label Patch deliberately avoid any
// Get: this runs before mgr.Start(), when a read through the manager's
// cached client would hang waiting on an informer cache that hasn't
// started syncing yet, but writes (Create/Patch) go straight to the API
// server and are always safe. A JSON merge patch (client.Merge) needs no
// prior read to construct — unlike client.MergeFrom, which diffs against
// an already-fetched original — and only touches the label keys actually
// present in podSecurityLabels, leaving any other existing labels alone.
func EnsureNamespace(ctx context.Context, c client.Client, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: podSecurityLabels}}
	if err := c.Create(ctx, ns); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating workload namespace %q: %w", name, err)
		}
		patch := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: podSecurityLabels}}
		if err := c.Patch(ctx, patch, client.Merge); err != nil {
			return fmt.Errorf("labeling existing workload namespace %q: %w", name, err)
		}
	}
	return nil
}
