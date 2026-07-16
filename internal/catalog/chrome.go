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

// Chrome deploys a full browser accessible via the operator's gateway.
// Ported from ai-cloud v1's chrome.ts — same shape as Firefox, different
// image/profile path.
var Chrome = Template{
	Build: func(params map[string]any) (Rendered, error) {
		profileDownloadURL := paramString(params, "profileDownloadUrl", "")

		return Rendered{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{Name: envPUID, Value: linuxserverUID},
						{Name: envPGID, Value: linuxserverUID},
						{Name: envTZ, Value: linuxserverTimezone},
					},
					Image:         "lscr.io/linuxserver/chrome:latest",
					LivenessProbe: browserProbe(30),
					Name:          templateIDChrome,
					Ports: []corev1.ContainerPort{
						{ContainerPort: browserHTTPPort, Name: portNameHTTP},
						{ContainerPort: 3001, Name: "https"},
					},
					ReadinessProbe: browserProbe(15),
					Resources:      browserResources("1000m", "1500Mi", "3Gi"),
					VolumeMounts: []corev1.VolumeMount{
						{MountPath: browserConfigMountPath, Name: configVolumeName},
						{MountPath: "/dev/shm", Name: "dshm"},
					},
				},
			},
			InitContainers: []corev1.Container{
				restoreProfileInitContainer(".config/google-chrome", profileDownloadURL),
			},
			ServicePorts: []corev1.ServicePort{
				{Name: portNameHTTP, Port: 80, TargetPort: intstr.FromInt32(browserHTTPPort)},
			},
			Volumes: []corev1.Volume{
				{Name: configVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{
					Name: "dshm",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: resourceQuantityPtr("4Gi"),
						},
					},
				},
			},
		}, nil
	},
	Operations:  []Operation{backupStateFunction(".config/google-chrome", templateIDChrome, profileSourceKeyChrome)},
	Description: "Full Chrome browser accessible via web interface",
	Entrypoints: []Entrypoint{{Name: portNameHTTP, Label: entrypointLabelWeb}},
	ID:          templateIDChrome,
	Icon:        "🌐",
	Name:        "Chrome Browser",
	Version:     "1.1.0",
	Parameters:  browserParameters(profileSourceKeyChrome),
}
