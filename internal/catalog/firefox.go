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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Firefox deploys a full browser accessible via the operator's gateway.
// Ported from ai-cloud v1's firefox.ts, minus the Traefik
// HTTPRoute/Middleware/ForwardAuth wiring — that's now handled by this
// operator's own /gw/* gateway instead of per-workload ingress resources.
var Firefox = Template{
	Build: func(params map[string]any) (Rendered, error) {
		profileDownloadURL := paramString(params, "profileDownloadUrl", "")

		return Rendered{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{Name: "PUID", Value: "1000"},
						{Name: "PGID", Value: "1000"},
						{Name: "TZ", Value: "Etc/UTC"},
					},
					Image:         "linuxserver/firefox:latest",
					LivenessProbe: browserProbe(3000, 30),
					Name:          "firefox",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 3000, Name: "http"},
						{ContainerPort: 3001, Name: "https"},
					},
					ReadinessProbe: browserProbe(3000, 15),
					Resources:      browserResources("1000m", "1500Mi", "3Gi"),
					VolumeMounts: []corev1.VolumeMount{
						{MountPath: "/config", Name: "config"},
					},
				},
			},
			InitContainers: []corev1.Container{
				restoreProfileInitContainer(".mozilla/firefox", profileDownloadURL),
			},
			ServicePorts: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(3000)},
			},
			Volumes: []corev1.Volume{
				{Name: "config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		}, nil
	},
	Description: "Full Firefox browser accessible via web interface",
	ID:          "firefox",
	Icon:        "🦊",
	Name:        "Firefox Browser",
	Parameters:  browserParameters,
}
