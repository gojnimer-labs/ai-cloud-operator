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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// browserParameters is the parameter set shared by firefox/chrome: a
// user-facing choice of whether/what profile to restore, and a
// system-computed presigned download URL Convex fills in when restore is
// requested — never an editable field, since an editable URL here would let
// the operator's init container curl an arbitrary user-supplied address.
var browserParameters = []Parameter{
	{
		Key:         "profileName",
		Label:       "Profile name",
		Description: "Identifies which saved profile to restore, if any.",
		Type:        ParameterTypeString,
		Source:      ParameterSourceUser,
		Required:    false,
	},
	{
		Key:      "restoreProfile",
		Label:    "Restore saved profile",
		Type:     ParameterTypeBoolean,
		Source:   ParameterSourceUser,
		Required: false,
		Default:  false,
	},
	{
		Key:      "profileDownloadUrl",
		Label:    "Profile download URL (system)",
		Type:     ParameterTypeString,
		Source:   ParameterSourceSystem,
		Required: false,
	},
}

// restoreProfileInitContainer builds the init container that restores a
// browser profile from profileDownloadURL (an R2 presigned GET URL Convex
// computed) before the main browser container starts. The URL travels as an
// env var, never string-interpolated into the shell script itself, so it
// can't break out of quoting.
//
// Unlike v1's version of this script, this checks the HTTP status before
// extracting — a 404 (no profile saved yet) is treated as "start fresh"
// instead of trying to tar-extract an error response body.
func restoreProfileInitContainer(profilePath string, profileDownloadURL string) corev1.Container {
	script := fmt.Sprintf(`set -e
apk add --no-cache curl tar gzip
PROFILE_DIR="/config/%s"
mkdir -p "$PROFILE_DIR"
if [ -n "$PROFILE_DOWNLOAD_URL" ]; then
  echo "Attempting profile restore from R2..."
  status=$(curl -sL -w "%%{http_code}" -o /tmp/profile.tar.gz "$PROFILE_DOWNLOAD_URL")
  if [ "$status" = "200" ]; then
    tar -xzf /tmp/profile.tar.gz -C /config
    rm -f /tmp/profile.tar.gz
    echo "Profile restored successfully"
  else
    echo "No existing profile found (HTTP $status), starting fresh"
    rm -f /tmp/profile.tar.gz
  fi
else
  echo "No profile restore requested, starting fresh"
fi
chown -R 1000:1000 /config
chmod -R 755 /config
`, profilePath)

	return corev1.Container{
		Command: []string{"/bin/sh", "-c", script},
		Env: []corev1.EnvVar{
			{Name: "PROFILE_DOWNLOAD_URL", Value: profileDownloadURL},
		},
		Image: "alpine:latest",
		Name:  "restore-profile",
		VolumeMounts: []corev1.VolumeMount{
			{MountPath: "/config", Name: "config"},
		},
	}
}

func browserResources(cpu, memRequest, memLimit string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memLimit),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memRequest),
		},
	}
}

func browserProbe(port int32, initialDelay int32) *corev1.Probe {
	return &corev1.Probe{
		FailureThreshold:    3,
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       10,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt32(port),
			},
		},
		TimeoutSeconds: 5,
	}
}

func resourceQuantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
