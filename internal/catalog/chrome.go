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

// envChromeCLI is docker-chrome's own env var
// (https://github.com/linuxserver/docker-chrome) for passing flags straight
// to the Chrome binary's argv — a bare URL is a valid Chrome CLI argument,
// so this doubles as the mechanism startURLParameter's value rides on.
const envChromeCLI = "CHROME_CLI"

// Chrome deploys a full browser accessible via the operator's gateway.
// Ported from ai-cloud v1's chrome.ts — same shape as Firefox, different
// image/profile path.
var Chrome = Template{
	Build: func(params map[string]any) (Rendered, error) {
		profileDownloadURL := paramString(params, paramKeyProfileURL, "")

		env := []corev1.EnvVar{
			{Name: envPUID, Value: linuxserverUID},
			{Name: envPGID, Value: linuxserverUID},
			{Name: envTZ, Value: linuxserverTimezone},
			fileManagerPathEnv(),
		}
		// Blank is a valid, supported state: CHROME_CLI unset just means
		// Chrome opens its own default new-tab page, same as today.
		if startURL := paramString(params, paramKeyStartURL, ""); startURL != "" {
			// Confirmed by linuxserver's own docker-compose example
			// (CHROME_CLI=https://www.linuxserver.io/) — opens it as the
			// initial tab.
			env = append(env, corev1.EnvVar{Name: envChromeCLI, Value: startURL})
		}

		return Rendered{
			Containers: []corev1.Container{
				{
					Env:           env,
					Image:         "lscr.io/linuxserver/chrome:latest",
					LivenessProbe: browserProbe(30),
					Name:          templateIDChrome,
					Ports: []corev1.ContainerPort{
						{ContainerPort: browserHTTPPort, Name: portNameHTTP},
						{ContainerPort: browserHTTPSPort, Name: portNameHTTPS},
					},
					ReadinessProbe: browserProbe(15),
					Resources:      browserResources("1000m", "1500Mi", "3Gi"),
					VolumeMounts: []corev1.VolumeMount{
						{MountPath: browserConfigMountPath, Name: configVolumeName},
						{MountPath: dshmMountPath, Name: dshmVolumeName},
					},
				},
			},
			InitContainers: []corev1.Container{
				// "." (all of /config), not just ".config/google-chrome" —
				// see Webtop's own doc comment for why the whole home
				// directory, not one browser-internal subdirectory, is the
				// right scope: since fileManagerPathEnv now makes
				// $browserConfigMountPath/Downloads and .../Desktop directly
				// reachable through Selkies' own Files tab, a backup/restore
				// cycle needs to carry those too, not just Chrome's own
				// profile database.
				restoreProfileInitContainer(".", profileDownloadURL),
			},
			ServicePorts: []corev1.ServicePort{
				{Name: portNameHTTP, Port: 80, TargetPort: intstr.FromInt32(browserHTTPPort)},
			},
			Volumes: []corev1.Volume{
				{Name: configVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{
					Name: dshmVolumeName,
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
	Operations:  []Operation{backupStateFunction(".", templateIDChrome, profileSourceKeyChrome)},
	Description: "Full Chrome browser accessible via web interface",
	Entrypoints: []Entrypoint{{Name: portNameHTTP, Label: entrypointLabelWeb}},
	ID:          templateIDChrome,
	Icon:        "🌐",
	Name:        "Chrome Browser",
	// 1.2.0: added startUrl (see startURLParameter). 1.1.1 -> this version
	// also widened backup/restore scope from ".config/google-chrome" to "."
	// and set FILE_MANAGER_PATH — Build-only changes, not Parameters, so
	// they wouldn't need their own bump under this field's own convention,
	// but are called out here since they landed in the same commit.
	Version:    "1.2.0",
	Parameters: append(browserParameters(profileSourceKeyChrome), startURLParameter("Chrome")),
}
