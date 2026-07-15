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

// Nginx is the simplest catalog template — no init container, no system
// parameters — deliberately exercising the select and number widget types.
var Nginx = Template{
	Build: func(params map[string]any) (Rendered, error) {
		logLevel := paramString(params, paramKeyLogLevel, logLevelInfo)
		workerConnections := paramInt32(params, "workerConnections", 1024)

		return Rendered{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{Name: "LOG_LEVEL", Value: logLevel},
						{Name: "WORKER_CONNECTIONS", Value: int32ToString(workerConnections)},
					},
					Image: "nginxdemos/hello:latest",
					Name:  templateIDNginx,
					Ports: []corev1.ContainerPort{{ContainerPort: 80, Name: portNameHTTP}},
				},
			},
			ServicePorts: []corev1.ServicePort{
				{Name: portNameHTTP, Port: 80, TargetPort: intstr.FromInt32(80)},
			},
		}, nil
	},
	Description: "Simple nginx web server with hello world demo",
	Entrypoints: []Entrypoint{{Name: portNameHTTP, Label: entrypointLabelWeb}},
	ID:          templateIDNginx,
	Icon:        "🌐",
	Name:        "Nginx",
	Version:     initialTemplateVersion,
	Parameters: []Parameter{
		{
			Default: logLevelInfo,
			Key:     paramKeyLogLevel,
			Label:   "Log level",
			Options: []SelectOption{
				{Label: "Info", Value: logLevelInfo},
				{Label: "Warn", Value: logLevelWarn},
				{Label: "Error", Value: logLevelError},
			},
			Required:   false,
			DataSource: DataSource{Kind: DataSourceStatic},
			Type:       ParameterTypeSelect,
		},
		{
			Default:     float64(1024),
			Description: "Passed through as an env var for illustration.",
			Key:         "workerConnections",
			Label:       "Worker connections",
			Required:    false,
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeNumber,
			// Demonstrates the new Validation field — an out-of-range worker
			// connection count is rejected rather than silently accepted.
			Validation: &Validation{Min: ptrFloat64(0), Max: ptrFloat64(65536)},
		},
	},
}
