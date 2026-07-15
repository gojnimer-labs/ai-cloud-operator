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

package convexclient

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnrollmentSecretName is the Secret (created out-of-band per cluster, never
// checked into git — see docs/production-deploy.md) that holds
// ENROLLMENT_SECRET. The operator reads its initial value from the env var
// the Deployment populates from this same Secret, then polls the Secret
// itself thereafter so a rotation doesn't require restarting the pod.
const EnrollmentSecretName = "ai-cloud-operator-env"

const enrollmentSecretKey = "ENROLLMENT_SECRET"

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get,resourceNames=ai-cloud-operator-env

// EnrollmentSecretWatcher reads the current ENROLLMENT_SECRET value straight
// from its Secret, so callers can detect an out-of-band rotation (a human
// re-running the kubectl create secret step) without a pod restart.
type EnrollmentSecretWatcher struct {
	client    client.Client
	namespace string
}

// NewEnrollmentSecretWatcher returns a watcher reading EnrollmentSecretName
// in namespace.
func NewEnrollmentSecretWatcher(c client.Client, namespace string) *EnrollmentSecretWatcher {
	return &EnrollmentSecretWatcher{client: c, namespace: namespace}
}

// Current returns the enrollment secret's present value. An empty string
// means the key (or the Secret itself) doesn't exist.
func (w *EnrollmentSecretWatcher) Current(ctx context.Context) (string, error) {
	var secret corev1.Secret
	err := w.client.Get(ctx, client.ObjectKey{Name: EnrollmentSecretName, Namespace: w.namespace}, &secret)
	if err != nil {
		return "", fmt.Errorf("getting enrollment secret: %w", err)
	}
	return string(secret.Data[enrollmentSecretKey]), nil
}
