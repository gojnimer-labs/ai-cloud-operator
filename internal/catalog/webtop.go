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

// webtopFlavors enumerates linuxserver/webtop's published OS+desktop-
// environment image tags (https://docs.linuxserver.io/images/docker-webtop/).
// "latest" is Alpine+XFCE, upstream's own default, so it doubles as this
// parameter's Default rather than picking an opinionated non-default flavor.
var webtopFlavors = []SelectOption{
	{Value: "latest", Label: "Alpine + XFCE (latest)"},
	{Value: "alpine-i3", Label: "Alpine + i3"},
	{Value: "alpine-kde", Label: "Alpine + KDE"},
	{Value: "alpine-mate", Label: "Alpine + MATE"},
	{Value: "arch-i3", Label: "Arch + i3"},
	{Value: "arch-kde", Label: "Arch + KDE"},
	{Value: "arch-mate", Label: "Arch + MATE"},
	{Value: "arch-xfce", Label: "Arch + XFCE"},
	{Value: "debian-i3", Label: "Debian + i3"},
	{Value: "debian-kde", Label: "Debian + KDE"},
	{Value: "debian-mate", Label: "Debian + MATE"},
	{Value: "debian-xfce", Label: "Debian + XFCE"},
	{Value: "fedora-i3", Label: "Fedora + i3"},
	{Value: "fedora-kde", Label: "Fedora + KDE"},
	{Value: "fedora-mate", Label: "Fedora + MATE"},
	{Value: "fedora-xfce", Label: "Fedora + XFCE"},
	{Value: "ubuntu-i3", Label: "Ubuntu + i3"},
	{Value: "ubuntu-kde", Label: "Ubuntu + KDE"},
	{Value: "ubuntu-mate", Label: "Ubuntu + MATE"},
	{Value: "ubuntu-xfce", Label: "Ubuntu + XFCE"},
}

// Webtop deploys a full Linux desktop (Selkies —
// github.com/linuxserver/docker-baseimage-selkies) accessible via the
// operator's gateway — same linuxserver.io PUID/PGID/TZ/config-volume/
// profile-restore/FILE_MANAGER_PATH shape as Firefox/Chrome (which are
// themselves built on this same Selkies base image), except there's no
// single-application "default URL" concept for a full desktop, so unlike
// Firefox/Chrome it declares no startURLParameter. The "profile" being
// restored/backed up is, and always was, the entire /config home directory
// (passed as "." — Firefox/Chrome now do the same, see their own doc
// comments on why) rather than one browser's profile subdirectory, since a
// desktop has no single well-known profile path to narrow it to.
var Webtop = Template{
	Build: func(params map[string]any) (Rendered, error) {
		flavor := paramString(params, "flavor", "latest")
		profileDownloadURL := paramString(params, paramKeyProfileURL, "")

		return Rendered{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{Name: envPUID, Value: linuxserverUID},
						{Name: envPGID, Value: linuxserverUID},
						{Name: envTZ, Value: linuxserverTimezone},
						fileManagerPathEnv(),
					},
					Image:         "lscr.io/linuxserver/webtop:" + flavor,
					LivenessProbe: browserProbe(30),
					Name:          templateIDWebtop,
					Ports: []corev1.ContainerPort{
						{ContainerPort: browserHTTPPort, Name: portNameHTTP},
						{ContainerPort: browserHTTPSPort, Name: portNameHTTPS},
					},
					ReadinessProbe: browserProbe(15),
					Resources:      browserResources("1500m", "2Gi", "4Gi"),
					VolumeMounts: []corev1.VolumeMount{
						{MountPath: browserConfigMountPath, Name: configVolumeName},
						{MountPath: dshmMountPath, Name: dshmVolumeName},
					},
				},
			},
			InitContainers: []corev1.Container{
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
							SizeLimit: resourceQuantityPtr("1Gi"),
						},
					},
				},
			},
		}, nil
	},
	Operations:  []Operation{backupStateFunction(".", templateIDWebtop, profileSourceKeyWebtop)},
	Description: "Full Linux desktop environment accessible via web browser",
	Entrypoints: []Entrypoint{{Name: portNameHTTP, Label: entrypointLabelWeb}},
	ID:          templateIDWebtop,
	Icon:        "🖥️",
	Name:        "Webtop Desktop",
	Version:     initialTemplateVersion,
	Parameters: append([]Parameter{
		{
			Default:     "latest",
			Description: "Base OS and desktop environment combination.",
			Key:         "flavor",
			Label:       "Flavor",
			Options:     webtopFlavors,
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeSelect,
		},
	}, browserParameters(profileSourceKeyWebtop)...),
}
