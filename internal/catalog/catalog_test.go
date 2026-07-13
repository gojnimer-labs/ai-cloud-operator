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

import "testing"

func TestGetReturnsKnownTemplates(t *testing.T) {
	for _, id := range []string{"nginx", "firefox", "chrome"} {
		if _, ok := Get(id); !ok {
			t.Fatalf("expected template %q to be registered", id)
		}
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Fatalf("expected unknown template id to be absent")
	}
}

func TestResolveParamsAppliesDefaults(t *testing.T) {
	tmpl, _ := Get("nginx")
	resolved, err := ResolveParams(tmpl, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved["logLevel"] != "info" {
		t.Fatalf("expected default logLevel=info, got %v", resolved["logLevel"])
	}
	if resolved["workerConnections"] != float64(1024) {
		t.Fatalf("expected default workerConnections=1024, got %v", resolved["workerConnections"])
	}
}

func TestResolveParamsUserValueOverridesDefault(t *testing.T) {
	tmpl, _ := Get("nginx")
	resolved, err := ResolveParams(tmpl, map[string]any{"logLevel": "error"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved["logLevel"] != "error" {
		t.Fatalf("expected logLevel=error, got %v", resolved["logLevel"])
	}
}

func TestResolveParamsRejectsMissingRequired(t *testing.T) {
	tmpl := Template{
		ID: "test",
		Parameters: []Parameter{
			{Key: "name", Required: true, Type: ParameterTypeString},
		},
	}
	if _, err := ResolveParams(tmpl, map[string]any{}); err == nil {
		t.Fatalf("expected an error for missing required parameter")
	}
}

func TestNginxBuildUsesResolvedParams(t *testing.T) {
	tmpl, _ := Get("nginx")
	resolved, err := ResolveParams(tmpl, map[string]any{"logLevel": "warn"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rendered, err := tmpl.Build(resolved)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rendered.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(rendered.Containers))
	}
	found := false
	for _, env := range rendered.Containers[0].Env {
		if env.Name == "LOG_LEVEL" && env.Value == "warn" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected LOG_LEVEL=warn env var, got %+v", rendered.Containers[0].Env)
	}
}

func TestFirefoxBuildPassesProfileDownloadURL(t *testing.T) {
	tmpl, _ := Get("firefox")
	rendered, err := tmpl.Build(map[string]any{"profileDownloadUrl": "https://example.com/profile.tar.gz"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rendered.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(rendered.InitContainers))
	}
	found := false
	for _, env := range rendered.InitContainers[0].Env {
		if env.Name == "PROFILE_DOWNLOAD_URL" && env.Value == "https://example.com/profile.tar.gz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PROFILE_DOWNLOAD_URL env var on init container, got %+v", rendered.InitContainers[0].Env)
	}
}

func TestFirefoxBuildWithoutProfileURLStartsFresh(t *testing.T) {
	tmpl, _ := Get("firefox")
	rendered, err := tmpl.Build(map[string]any{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, env := range rendered.InitContainers[0].Env {
		if env.Name == "PROFILE_DOWNLOAD_URL" && env.Value != "" {
			t.Fatalf("expected empty PROFILE_DOWNLOAD_URL when not provided, got %q", env.Value)
		}
	}
}
