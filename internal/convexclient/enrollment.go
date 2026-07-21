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
	"os"
	"strings"
)

// EnrollmentSecretPath is where the Deployment mounts the ENROLLMENT_SECRET
// Secret as a volume (see config/manager/manager.yaml and
// charts/ai-cloud-operator/templates/deployment.yaml) — deliberately a
// volume, not an env var: env vars only resolve once at container start, so
// an env var alone can't support checkEnrollmentSecret's out-of-band
// rotation detection, and reading the Kubernetes API by name (the previous
// approach) required this operator to know, and be granted RBAC on, the
// Secret's exact object name — which the chart's existingSecretName value
// could silently disagree with. A mounted volume sidesteps both: kubelet
// refreshes the file's content on its own sync cycle whenever the backing
// Secret changes (as long as no subPath is used, which would disable that),
// and this operator never needs to know — or have RBAC for — the Secret's
// Kubernetes object name at all, only this fixed path.
const EnrollmentSecretPath = "/etc/ai-cloud-operator/enrollment/ENROLLMENT_SECRET"

// EnrollmentSecretWatcher reads the current ENROLLMENT_SECRET value straight
// from the mounted volume, so callers can detect an out-of-band rotation (a
// human updating the backing Secret) without a pod restart.
type EnrollmentSecretWatcher struct {
	path string
}

// NewEnrollmentSecretWatcher returns a watcher reading path. An empty path
// defaults to EnrollmentSecretPath — tests pass an explicit temp-file path
// instead.
func NewEnrollmentSecretWatcher(path string) *EnrollmentSecretWatcher {
	if path == "" {
		path = EnrollmentSecretPath
	}
	return &EnrollmentSecretWatcher{path: path}
}

// Current returns the enrollment secret's present value, trimmed of the
// trailing newline Kubernetes Secret volumes commonly carry.
func (w *EnrollmentSecretWatcher) Current(_ context.Context) (string, error) {
	data, err := os.ReadFile(w.path)
	if err != nil {
		return "", fmt.Errorf("reading enrollment secret file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
