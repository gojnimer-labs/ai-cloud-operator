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

const (
	templateIDCodeServer = "code-server"
	codeServerPort       = int32(8443)
)

// codeServerProbe targets codeServerPort — deliberately not browserProbe
// (see its own doc comment), which is hard-coded to browserHTTPPort for
// firefox/chrome specifically.
func codeServerProbe(initialDelay int32) *corev1.Probe {
	return &corev1.Probe{
		FailureThreshold:    3,
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       10,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt32(codeServerPort),
			},
		},
		TimeoutSeconds: 5,
	}
}

// CodeServer deploys coder/code-server (https://github.com/coder/code-server)
// — VS Code accessible via the browser — using linuxserver.io's image for
// the same PUID/PGID/TZ conventions as firefox/chrome (see
// browserParameters's PUID/PGID/TZ block). Unlike firefox/chrome, its
// workspace volume is a plain EmptyDir with no backup_state-style Operation
// wired to R2 — add one later, following backupStateFunction in
// browser.go, if persisting/restoring the workspace across deploys becomes
// a real need.
var CodeServer = Template{
	Build: func(params map[string]any) (Rendered, error) {
		password := paramString(params, "password", "")
		sudoPassword := paramString(params, "sudoPassword", "")
		defaultWorkspace := paramString(params, "defaultWorkspace", "/config/workspace")

		env := []corev1.EnvVar{
			{Name: "PUID", Value: "1000"},
			{Name: "PGID", Value: "1000"},
			{Name: "TZ", Value: "Etc/UTC"},
			{Name: "DEFAULT_WORKSPACE", Value: defaultWorkspace},
		}
		// Omitted entirely rather than passed as "" — an explicit empty
		// PASSWORD/SUDO_PASSWORD env var is not the same thing to the
		// image's entrypoint script as the var being unset.
		if password != "" {
			env = append(env, corev1.EnvVar{Name: "PASSWORD", Value: password})
		}
		if sudoPassword != "" {
			env = append(env, corev1.EnvVar{Name: "SUDO_PASSWORD", Value: sudoPassword})
		}

		return Rendered{
			Containers: []corev1.Container{
				{
					Env:            env,
					Image:          "lscr.io/linuxserver/code-server:latest",
					LivenessProbe:  codeServerProbe(30),
					Name:           templateIDCodeServer,
					Ports:          []corev1.ContainerPort{{ContainerPort: codeServerPort, Name: portNameHTTP}},
					ReadinessProbe: codeServerProbe(15),
					Resources:      browserResources("1000m", "1024Mi", "2Gi"),
					VolumeMounts: []corev1.VolumeMount{
						{MountPath: browserConfigMountPath, Name: configVolumeName},
					},
				},
			},
			ServicePorts: []corev1.ServicePort{
				{Name: portNameHTTP, Port: 80, TargetPort: intstr.FromInt32(codeServerPort)},
			},
			Volumes: []corev1.Volume{
				{Name: configVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		}, nil
	},
	Description: "VS Code in the browser (coder/code-server)",
	Entrypoints: []Entrypoint{{Name: portNameHTTP, Label: entrypointLabelWeb}},
	ID:          templateIDCodeServer,
	Icon:        "💻",
	Name:        "code-server",
	Version:     initialTemplateVersion,
	Parameters: []Parameter{
		{
			Description: "Login password for the code-server web UI. Leave blank to use the image's own default password behavior (check the pod's startup logs).",
			Key:         "password",
			Label:       "Password",
			Required:    false,
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
		{
			Description: "Enables passwordless-if-blank sudo inside the container when set — passed through as SUDO_PASSWORD.",
			Key:         "sudoPassword",
			Label:       "Sudo password",
			Required:    false,
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
		{
			Default:     "/config/workspace",
			Description: "Folder code-server opens by default.",
			Key:         "defaultWorkspace",
			Label:       "Default workspace path",
			Required:    false,
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
	},
}
