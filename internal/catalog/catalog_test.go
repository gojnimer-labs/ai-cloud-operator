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
	"context"
	"errors"
	"testing"
)

const (
	testNamespace  = "default"
	testFirefoxPod = "firefox-abc"

	// Parameter keys reused across the synthetic Templates below.
	paramKeyName  = "name"
	paramKeyMode  = "mode"
	paramKeyCount = "count"
)

// fakePodExecutor is a minimal in-package stand-in for a real client-go SPDY
// exec, recording the single call made so tests can assert on it.
type fakePodExecutor struct {
	namespace, podName, container string
	command                       []string
	stdout, stderr                string
	err                           error
}

func (f *fakePodExecutor) Exec(_ context.Context, namespace, podName, container string, command []string) (string, string, error) {
	f.namespace, f.podName, f.container, f.command = namespace, podName, container, command
	return f.stdout, f.stderr, f.err
}

func TestGetReturnsKnownTemplates(t *testing.T) {
	for _, id := range []string{templateIDNginx, templateIDFirefox, templateIDChrome} {
		if _, ok := Get(id); !ok {
			t.Fatalf("expected template %q to be registered", id)
		}
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Fatalf("expected unknown template id to be absent")
	}
}

func TestResolveParamsAppliesDefaults(t *testing.T) {
	tmpl, _ := Get(templateIDNginx)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved[paramKeyLogLevel] != logLevelInfo {
		t.Fatalf("expected default logLevel=info, got %v", resolved[paramKeyLogLevel])
	}
	if resolved["workerConnections"] != float64(1024) {
		t.Fatalf("expected default workerConnections=1024, got %v", resolved["workerConnections"])
	}
}

func TestResolveParamsUserValueOverridesDefault(t *testing.T) {
	tmpl, _ := Get(templateIDNginx)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{paramKeyLogLevel: logLevelError})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved[paramKeyLogLevel] != logLevelError {
		t.Fatalf("expected logLevel=error, got %v", resolved[paramKeyLogLevel])
	}
}

func TestResolveParamsRejectsMissingRequired(t *testing.T) {
	tmpl := Template{
		ID: "test",
		Parameters: []Parameter{
			{Key: paramKeyName, Required: true, Type: ParameterTypeString},
		},
	}
	if _, err := ResolveParams(tmpl.Parameters, map[string]any{}); err == nil {
		t.Fatalf("expected an error for missing required parameter")
	}
}

// TestResolveParamsSkipsRequiredAndValidationWhenHidden covers the core new
// rule: a parameter whose Visibility condition doesn't hold is exempt from
// both Required and Validation — nothing rendered a form field for it, so
// enforcing either would be unsatisfiable by construction.
func TestResolveParamsSkipsRequiredAndValidationWhenHidden(t *testing.T) {
	maxLen := 3
	params := []Parameter{
		{Key: paramKeyMode, Type: ParameterTypeString, DataSource: DataSource{Kind: DataSourceStatic}},
		{
			Key:        "advancedOption",
			Type:       ParameterTypeString,
			Required:   true,
			Validation: &Validation{MaxLength: &maxLen},
			Visibility: &Visibility{DependsOn: paramKeyMode, Op: VisibilityEquals, Value: "advanced"},
		},
	}

	// mode != "advanced", so advancedOption is hidden: missing + required +
	// would-be-too-long-if-checked must still resolve cleanly.
	if _, err := ResolveParams(params, map[string]any{paramKeyMode: "simple"}); err != nil {
		t.Fatalf("expected hidden required parameter to be exempt, got error: %v", err)
	}

	// mode == "advanced" now makes it visible, so the same missing value
	// must be rejected.
	if _, err := ResolveParams(params, map[string]any{paramKeyMode: "advanced"}); err == nil {
		t.Fatalf("expected an error once the dependency makes the parameter visible")
	}
}

func TestResolveParamsRejectsValidationViolations(t *testing.T) {
	minV, maxV := 0.0, 10.0
	numeric := []Parameter{
		{Key: paramKeyCount, Type: ParameterTypeNumber, DataSource: DataSource{Kind: DataSourceStatic}, Validation: &Validation{Min: &minV, Max: &maxV}},
	}
	if _, err := ResolveParams(numeric, map[string]any{paramKeyCount: float64(50)}); err == nil {
		t.Fatalf("expected an error for a value above Max")
	}
	if _, err := ResolveParams(numeric, map[string]any{paramKeyCount: float64(5)}); err != nil {
		t.Fatalf("expected an in-range value to pass, got %v", err)
	}

	pattern := []Parameter{
		{Key: paramKeyName, Type: ParameterTypeString, DataSource: DataSource{Kind: DataSourceStatic}, Validation: &Validation{Regex: "^[a-z]+$"}},
	}
	if _, err := ResolveParams(pattern, map[string]any{paramKeyName: "Not Valid!"}); err == nil {
		t.Fatalf("expected an error for a regex mismatch")
	}
	if _, err := ResolveParams(pattern, map[string]any{paramKeyName: "validname"}); err != nil {
		t.Fatalf("expected a matching value to pass, got %v", err)
	}
}

// TestNginxRejectsOutOfRangeWorkerConnections exercises Validation on a real
// template's real field, not just a synthetic one.
func TestNginxRejectsOutOfRangeWorkerConnections(t *testing.T) {
	tmpl, _ := Get(templateIDNginx)
	if _, err := ResolveParams(tmpl.Parameters, map[string]any{"workerConnections": float64(999999)}); err == nil {
		t.Fatalf("expected an error for workerConnections exceeding its declared Max")
	}
}

func TestNginxBuildUsesResolvedParams(t *testing.T) {
	tmpl, _ := Get(templateIDNginx)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{paramKeyLogLevel: logLevelWarn})
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
		if env.Name == "LOG_LEVEL" && env.Value == logLevelWarn {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected LOG_LEVEL=warn env var, got %+v", rendered.Containers[0].Env)
	}
}

func TestFirefoxBuildPassesProfileDownloadURL(t *testing.T) {
	tmpl, _ := Get(templateIDFirefox)
	rendered, err := tmpl.Build(map[string]any{"profileDownloadUrl": "https://example.com/profile.tar.gz"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rendered.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(rendered.InitContainers))
	}
	found := false
	for _, env := range rendered.InitContainers[0].Env {
		if env.Name == envProfileDownloadURL && env.Value == "https://example.com/profile.tar.gz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PROFILE_DOWNLOAD_URL env var on init container, got %+v", rendered.InitContainers[0].Env)
	}
}

func TestFirefoxAndChromeExposeBackupStateFunction(t *testing.T) {
	for _, id := range []string{templateIDFirefox, templateIDChrome} {
		tmpl, _ := Get(id)
		fn, ok := GetCustomFunction(tmpl, "backup_state")
		if !ok {
			t.Fatalf("%s: expected a backup_state custom function", id)
		}
		if len(fn.Parameters) != 2 || fn.Parameters[0].Key != "label" || fn.Parameters[1].Key != paramKeyUploadURL {
			t.Fatalf("%s: expected label and uploadUrl parameters, got %+v", id, fn.Parameters)
		}
		if fn.Parameters[0].DataSource.Kind != DataSourceStatic {
			t.Fatalf("%s: expected label to be a static data source", id)
		}
		if fn.Parameters[1].DataSource.Kind != DataSourceSystem {
			t.Fatalf("%s: expected uploadUrl to be a system data source", id)
		}
	}
	if _, ok := GetCustomFunction(Template{}, "backup_state"); ok {
		t.Fatalf("expected no functions on an empty template")
	}
}

func TestBackupStateFunctionRequiresUploadURL(t *testing.T) {
	tmpl, _ := Get(templateIDFirefox)
	fn, _ := GetCustomFunction(tmpl, "backup_state")

	exec := &fakePodExecutor{}
	if _, err := fn.Run(context.Background(), exec, PodRef{Namespace: testNamespace, PodName: testFirefoxPod}, map[string]any{}); err == nil {
		t.Fatalf("expected an error when uploadUrl is missing")
	}
	if exec.command != nil {
		t.Fatalf("expected no exec call when validation fails, got %+v", exec.command)
	}
}

func TestBackupStateFunctionExecutesTarAndCurl(t *testing.T) {
	tmpl, _ := Get(templateIDFirefox)
	fn, _ := GetCustomFunction(tmpl, "backup_state")

	exec := &fakePodExecutor{stdout: "Backup completed successfully"}
	result, err := fn.Run(context.Background(), exec, PodRef{Namespace: testNamespace, PodName: testFirefoxPod}, map[string]any{
		paramKeyUploadURL: "https://r2.example.com/profiles/firefox/user-1/123.tar.gz?X-Amz-Signature=abc&X-Amz-Expires=900",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["stdout"] != "Backup completed successfully" {
		t.Fatalf("expected stdout to be returned, got %+v", result)
	}

	if exec.namespace != testNamespace || exec.podName != testFirefoxPod || exec.container != templateIDFirefox {
		t.Fatalf("unexpected exec target: namespace=%q podName=%q container=%q", exec.namespace, exec.podName, exec.container)
	}
	// The profile path and upload URL must travel as trailing positional
	// args (not interpolated into the script string) — see
	// backupStateFunction's doc comment for why.
	if len(exec.command) < 2 {
		t.Fatalf("expected at least 2 trailing args, got %+v", exec.command)
	}
	lastTwo := exec.command[len(exec.command)-2:]
	if lastTwo[0] != ".mozilla/firefox" {
		t.Fatalf("expected profile path as second-to-last arg, got %q", lastTwo[0])
	}
	if lastTwo[1] != "https://r2.example.com/profiles/firefox/user-1/123.tar.gz?X-Amz-Signature=abc&X-Amz-Expires=900" {
		t.Fatalf("expected upload URL as last arg, got %q", lastTwo[1])
	}
}

func TestBackupStateFunctionWrapsExecutorError(t *testing.T) {
	tmpl, _ := Get(templateIDChrome)
	fn, _ := GetCustomFunction(tmpl, "backup_state")

	exec := &fakePodExecutor{stderr: "tar: /config/.config/google-chrome: No such file", err: errors.New("command terminated with exit code 1")}
	if _, err := fn.Run(context.Background(), exec, PodRef{Namespace: testNamespace, PodName: "chrome-abc"}, map[string]any{
		paramKeyUploadURL: "https://r2.example.com/upload",
	}); err == nil {
		t.Fatalf("expected the executor's error to surface")
	}
}

func TestFirefoxBuildWithoutProfileURLStartsFresh(t *testing.T) {
	tmpl, _ := Get(templateIDFirefox)
	rendered, err := tmpl.Build(map[string]any{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, env := range rendered.InitContainers[0].Env {
		if env.Name == envProfileDownloadURL && env.Value != "" {
			t.Fatalf("expected empty PROFILE_DOWNLOAD_URL when not provided, got %q", env.Value)
		}
	}
}
