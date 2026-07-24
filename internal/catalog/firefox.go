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

// envFirefoxCLI is docker-firefox's own equivalent of docker-chrome's
// envChromeCLI (see chrome.go's own comment) — same
// pass-straight-to-argv-in-full mechanism, different app.
const envFirefoxCLI = "FIREFOX_CLI"

// Firefox deploys a full browser accessible via the operator's gateway.
// Ported from ai-cloud v1's firefox.ts, minus the Traefik
// HTTPRoute/Middleware/ForwardAuth wiring — that's now handled by this
// operator's own /gw/* gateway instead of per-workload ingress resources.
var Firefox = Template{
	Build: func(params map[string]any) (Rendered, error) {
		profileDownloadURL := paramString(params, paramKeyProfileURL, "")

		env := []corev1.EnvVar{
			{Name: envPUID, Value: linuxserverUID},
			{Name: envPGID, Value: linuxserverUID},
			{Name: envTZ, Value: linuxserverTimezone},
			fileManagerPathEnv(),
		}
		// Blank is a valid, supported state: FIREFOX_CLI unset just means
		// Firefox opens its own default new-tab/home page, same as today.
		if startURL := paramString(params, paramKeyStartURL, ""); startURL != "" {
			env = append(env, corev1.EnvVar{Name: envFirefoxCLI, Value: startURL})
		}

		return Rendered{
			Containers: []corev1.Container{
				{
					Env:           env,
					Image:         "lscr.io/linuxserver/firefox:latest",
					LivenessProbe: browserProbe(30),
					Name:          templateIDFirefox,
					Ports: []corev1.ContainerPort{
						{ContainerPort: browserHTTPPort, Name: portNameHTTP},
						{ContainerPort: browserHTTPSPort, Name: portNameHTTPS},
					},
					ReadinessProbe: browserProbe(15),
					Resources:      browserResources("1000m", "1500Mi", "3Gi"),
					VolumeMounts: []corev1.VolumeMount{
						{MountPath: browserConfigMountPath, Name: configVolumeName},
					},
				},
			},
			InitContainers: []corev1.Container{
				// Restores into all of /config — see chrome.go's identical
				// comment on the same change; the reasoning is byte-for-byte
				// the same here, only the profile subdirectory name differs.
				restoreProfileInitContainer(profileDownloadURL),
			},
			ServicePorts: []corev1.ServicePort{
				{Name: portNameHTTP, Port: 80, TargetPort: intstr.FromInt32(browserHTTPPort)},
			},
			Volumes: []corev1.Volume{
				{Name: configVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		}, nil
	},
	Operations:  []Operation{backupStateFunction(templateIDFirefox, profileSourceKeyFirefox)},
	Description: "Full Firefox browser accessible via web interface",
	Entrypoints: []Entrypoint{{Name: portNameHTTP, Label: entrypointLabelWeb}},
	ID:          templateIDFirefox,
	Icon:        "🦊",
	Name:        "Firefox Browser",
	// 1.2.0: see chrome.go's identical version-comment — same three changes
	// (startUrl parameter, "." backup/restore scope, FILE_MANAGER_PATH),
	// same reasoning, applied to Firefox instead of Chrome.
	Version:    "1.2.0",
	Parameters: append(browserParameters(profileSourceKeyFirefox), startURLParameter("Firefox")),
}
