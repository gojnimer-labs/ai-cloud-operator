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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

// TestEstimatedResourcesNonZeroForEveryTemplate guards against the exact gap
// nginx used to have (see nginx.go): a template that declares no Resources
// at all would silently count as free toward any capacity accounting.
func TestEstimatedResourcesNonZeroForEveryTemplate(t *testing.T) {
	for _, tmpl := range List() {
		milliCPU, memoryBytes := tmpl.EstimatedResources()
		if milliCPU <= 0 || memoryBytes <= 0 {
			t.Errorf("template %q: expected nonzero estimated resources, got milliCPU=%d memoryBytes=%d", tmpl.ID, milliCPU, memoryBytes)
		}
	}
}

// TestHashIsDeterministic guards against Hash accidentally depending on
// nondeterministic encoding — internal/convexclient.Runnable relies on exact
// equality against a value persisted on a prior process's run.
func TestHashIsDeterministic(t *testing.T) {
	first := Hash()
	second := Hash()
	if first != second {
		t.Fatalf("expected Hash to be deterministic across calls, got %q then %q", first, second)
	}
}

// TestHashChangesWhenCatalogContentChanges is the core guarantee Runnable
// depends on: any change to the registered templates' wire-relevant content
// (e.g. a template's Version bump) must change Hash's output, or a stale
// persisted token would never trigger a re-register.
func TestHashChangesWhenCatalogContentChanges(t *testing.T) {
	original := templates
	before := Hash()
	defer func() { templates = original }()

	templates = append(append([]Template{}, original...), Template{ID: "synthetic", Version: "9.9.9"})

	if Hash() == before {
		t.Fatalf("expected Hash to change when catalog content changes")
	}
}

// TestEstimatedResourcesMatchesHardcodedBrowserValues pins the exact figures
// browserResources("1000m", "1500Mi", "3Gi") should sum to, so a future
// change to those hardcoded values is a deliberate, visible diff here rather
// than a silent capacity-accounting drift.
func TestEstimatedResourcesMatchesHardcodedBrowserValues(t *testing.T) {
	const wantMilliCPU = 1000
	const wantMemoryBytes = 1500 * 1024 * 1024

	for _, id := range []string{templateIDFirefox, templateIDChrome} {
		tmpl, ok := Get(id)
		if !ok {
			t.Fatalf("expected template %q to be registered", id)
		}
		milliCPU, memoryBytes := tmpl.EstimatedResources()
		if milliCPU != wantMilliCPU || memoryBytes != wantMemoryBytes {
			t.Errorf("template %q: expected milliCPU=%d memoryBytes=%d, got milliCPU=%d memoryBytes=%d", id, wantMilliCPU, wantMemoryBytes, milliCPU, memoryBytes)
		}
	}
}

func TestGetReturnsKnownTemplates(t *testing.T) {
	for _, id := range []string{templateIDNginx, templateIDFirefox, templateIDChrome, templateIDWebtop, templateIDCodeServer} {
		if _, ok := Get(id); !ok {
			t.Fatalf("expected template %q to be registered", id)
		}
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Fatalf("expected unknown template id to be absent")
	}
}

// TestEntrypointsMatchRenderedServicePorts guards against catalog/runtime
// drift: every declared Entrypoint.Name must correspond to a real
// ServicePort.Name the template's own Build() produces, since that's the
// name internal/gateway/proxy.go looks up at request time.
func TestEntrypointsMatchRenderedServicePorts(t *testing.T) {
	for _, tmpl := range List() {
		if len(tmpl.Entrypoints) == 0 {
			t.Errorf("template %q declares no Entrypoints", tmpl.ID)
			continue
		}

		rendered, err := tmpl.Build(map[string]any{})
		if err != nil {
			t.Errorf("template %q: Build failed: %v", tmpl.ID, err)
			continue
		}

		portNames := make(map[string]bool, len(rendered.ServicePorts))
		for _, sp := range rendered.ServicePorts {
			if portNames[sp.Name] {
				t.Errorf("template %q: duplicate ServicePort name %q", tmpl.ID, sp.Name)
			}
			portNames[sp.Name] = true
		}

		for _, ep := range tmpl.Entrypoints {
			if !portNames[ep.Name] {
				t.Errorf("template %q: Entrypoint %q has no matching ServicePort", tmpl.ID, ep.Name)
			}
		}
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
			{Key: paramKeyName, Validation: Validation{Required: true}, Type: ParameterTypeString},
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
			Validation: Validation{Required: true, MaxLength: &maxLen},
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
		{Key: paramKeyCount, Type: ParameterTypeNumber, DataSource: DataSource{Kind: DataSourceStatic}, Validation: Validation{Min: &minV, Max: &maxV}},
	}
	if _, err := ResolveParams(numeric, map[string]any{paramKeyCount: float64(50)}); err == nil {
		t.Fatalf("expected an error for a value above Max")
	}
	if _, err := ResolveParams(numeric, map[string]any{paramKeyCount: float64(5)}); err != nil {
		t.Fatalf("expected an in-range value to pass, got %v", err)
	}

	pattern := []Parameter{
		{Key: paramKeyName, Type: ParameterTypeString, DataSource: DataSource{Kind: DataSourceStatic}, Validation: Validation{Regex: "^[a-z]+$"}},
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
	rendered, err := tmpl.Build(map[string]any{paramKeyProfileURL: "https://example.com/profile.tar.gz"})
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

// TestBrowserRequiresProfileNameWhenRestoreProfileIsOn is a regression test
// for a real bug: profileName had no Validation set at all (Go zero value,
// Required: false), so toggling restoreProfile on without ever picking a
// profile deployed anyway — Visibility's "hidden means exempt" rule was
// working correctly, there was just nothing to be exempt *from*.
func TestBrowserRequiresProfileNameWhenRestoreProfileIsOn(t *testing.T) {
	for _, id := range []string{templateIDFirefox, templateIDChrome} {
		tmpl, _ := Get(id)

		if _, err := ResolveParams(tmpl.Parameters, map[string]any{paramKeyRestoreProfile: true}); err == nil {
			t.Fatalf("%s: expected an error for restoreProfile=true with no profileName selected", id)
		}

		if _, err := ResolveParams(tmpl.Parameters, map[string]any{
			paramKeyRestoreProfile: true,
			paramKeyProfileName:    "some-row-id",
		}); err != nil {
			t.Fatalf("%s: expected a selected profileName to resolve cleanly, got: %v", id, err)
		}

		// restoreProfile off (or simply absent, its own default): profileName
		// stays hidden and exempt, same as before this fix.
		if _, err := ResolveParams(tmpl.Parameters, map[string]any{}); err != nil {
			t.Fatalf("%s: expected no error when restoreProfile is left at its default, got: %v", id, err)
		}
	}
}

func TestFirefoxAndChromeExposeBackupStateFunction(t *testing.T) {
	for _, id := range []string{templateIDFirefox, templateIDChrome, templateIDWebtop} {
		tmpl, _ := Get(id)
		fn, ok := GetOperation(tmpl, "backup_state")
		if !ok {
			t.Fatalf("%s: expected a backup_state operation", id)
		}
		if len(fn.Parameters) != 2 || fn.Parameters[0].Key != "label" || fn.Parameters[1].Key != paramKeyUploadURL {
			t.Fatalf("%s: expected label and uploadUrl parameters, got %+v", id, fn.Parameters)
		}
		if fn.Parameters[0].DataSource.Kind != DataSourceStatic {
			t.Fatalf("%s: expected label to be a static data source", id)
		}
		uploadSource := fn.Parameters[1].DataSource
		wantGroup := "profiles_" + id
		if uploadSource.Kind != DataSourceFile || uploadSource.Direction != DirectionUpload || uploadSource.Group != wantGroup {
			t.Fatalf("%s: expected uploadUrl to be an upload-direction file data source with group %q, got %+v", id, wantGroup, uploadSource)
		}
		if fn.Refreshable {
			t.Fatalf("%s: expected backup_state to not be Refreshable — it has a real side effect", id)
		}
	}
	if _, ok := GetOperation(Template{}, "backup_state"); ok {
		t.Fatalf("expected no operations on an empty template")
	}
}

// TestBrowserProfileDownloadURLDeclaresDownloadDirection guards the other
// half of the file-param contract: profileDownloadUrl must declare it
// resolves from profileName's selected row, not just that it's
// file-sourced — deployWorkload dispatches on these fields generically (see
// workloads/actions.ts#deployWorkload), so a missing/wrong value here would
// silently break restore.
func TestBrowserProfileDownloadURLDeclaresDownloadDirection(t *testing.T) {
	for _, id := range []string{templateIDFirefox, templateIDChrome, templateIDWebtop} {
		tmpl, _ := Get(id)
		source := findParameter(t, tmpl.Parameters, paramKeyProfileURL).DataSource
		if source.Kind != DataSourceFile || source.Direction != DirectionDownload ||
			source.SourceParam != paramKeyProfileName {
			t.Fatalf("%s: unexpected profileDownloadUrl data source: %+v", id, source)
		}
	}
}

// TestFirefoxAndChromeUseDistinctProfileSourceKeys guards against the two
// templates ever sharing one files-table group for saved profiles —
// Firefox and Chrome profile tarballs aren't interchangeable, so restoring
// one into the other would silently produce a broken profile.
func TestFirefoxAndChromeUseDistinctProfileSourceKeys(t *testing.T) {
	firefox, _ := Get(templateIDFirefox)
	chrome, _ := Get(templateIDChrome)

	firefoxGroup := findParameter(t, firefox.Parameters, paramKeyProfileName).DataSource.Group
	chromeGroup := findParameter(t, chrome.Parameters, paramKeyProfileName).DataSource.Group

	if firefoxGroup == "" || chromeGroup == "" {
		t.Fatalf("expected both templates to declare a profileName group, got firefox=%q chrome=%q", firefoxGroup, chromeGroup)
	}
	if firefoxGroup == chromeGroup {
		t.Fatalf("expected distinct profileName groups, both were %q", firefoxGroup)
	}
}

func findParameter(t *testing.T, params []Parameter, key string) Parameter {
	t.Helper()
	for _, p := range params {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("parameter %q not found", key)
	return Parameter{}
}

func TestBackupStateFunctionRequiresUploadURL(t *testing.T) {
	tmpl, _ := Get(templateIDFirefox)
	fn, _ := GetOperation(tmpl, "backup_state")

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
	fn, _ := GetOperation(tmpl, "backup_state")

	exec := &fakePodExecutor{stdout: "irrelevant — the response no longer surfaces raw stdout"}
	result, err := fn.Run(context.Background(), exec, PodRef{Namespace: testNamespace, PodName: testFirefoxPod}, map[string]any{
		paramKeyLabel:     "test backup",
		paramKeyUploadURL: "https://r2.example.com/profiles/firefox/user-1/123.tar.gz?X-Amz-Signature=abc&X-Amz-Expires=900",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.AdditionalInfo) != 1 || result.AdditionalInfo[0].Name != "result" ||
		result.AdditionalInfo[0].Type != AdditionalInfoPlain || result.AdditionalInfo[0].Value != "backup_state.success" {
		t.Fatalf("expected a single plain result AdditionalInfo with a stable message key, got %+v", result.AdditionalInfo)
	}
	if result.File == nil || result.File.Type != "browser_profile_backup" || result.File.Label != "test backup" {
		t.Fatalf("expected a browser_profile_backup FileResult, got %+v", result.File)
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
	fn, _ := GetOperation(tmpl, "backup_state")

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

func TestWebtopBuildDefaultsFlavorToLatest(t *testing.T) {
	tmpl, _ := Get(templateIDWebtop)
	rendered, err := tmpl.Build(map[string]any{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := rendered.Containers[0].Image; got != "lscr.io/linuxserver/webtop:latest" {
		t.Fatalf("expected default flavor image lscr.io/linuxserver/webtop:latest, got %q", got)
	}
}

func TestWebtopBuildUsesResolvedFlavor(t *testing.T) {
	tmpl, _ := Get(templateIDWebtop)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{"flavor": "ubuntu-kde"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rendered, err := tmpl.Build(resolved)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := rendered.Containers[0].Image; got != "lscr.io/linuxserver/webtop:ubuntu-kde" {
		t.Fatalf("expected flavor-specific image, got %q", got)
	}
}

func TestWebtopBuildIncludesSharedMemoryVolume(t *testing.T) {
	tmpl, _ := Get(templateIDWebtop)
	rendered, err := tmpl.Build(map[string]any{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	found := false
	for _, v := range rendered.Volumes {
		if v.Name == dshmVolumeName && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a memory-backed dshm volume, got %+v", rendered.Volumes)
	}
}

func TestWebtopBuildPassesProfileDownloadURL(t *testing.T) {
	tmpl, _ := Get(templateIDWebtop)
	rendered, err := tmpl.Build(map[string]any{paramKeyProfileURL: "https://example.com/desktop.tar.gz"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	found := false
	for _, env := range rendered.InitContainers[0].Env {
		if env.Name == envProfileDownloadURL && env.Value == "https://example.com/desktop.tar.gz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PROFILE_DOWNLOAD_URL env var on init container, got %+v", rendered.InitContainers[0].Env)
	}
}

func TestWebtopBuildWithoutProfileURLStartsFresh(t *testing.T) {
	tmpl, _ := Get(templateIDWebtop)
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

// TestWebtopUsesDistinctProfileSourceKeyFromBrowsers guards the same
// no-shared-group invariant as TestFirefoxAndChromeUseDistinctProfileSourceKeys,
// extended to webtop: a whole-desktop backup and a single browser's profile
// backup must never land in the same files-table group.
func TestWebtopUsesDistinctProfileSourceKeyFromBrowsers(t *testing.T) {
	webtop, _ := Get(templateIDWebtop)
	firefox, _ := Get(templateIDFirefox)
	chrome, _ := Get(templateIDChrome)

	webtopGroup := findParameter(t, webtop.Parameters, paramKeyProfileName).DataSource.Group
	firefoxGroup := findParameter(t, firefox.Parameters, paramKeyProfileName).DataSource.Group
	chromeGroup := findParameter(t, chrome.Parameters, paramKeyProfileName).DataSource.Group

	if webtopGroup == "" || webtopGroup == firefoxGroup || webtopGroup == chromeGroup {
		t.Fatalf("expected webtop's profileName group %q to be distinct and non-empty (firefox=%q chrome=%q)", webtopGroup, firefoxGroup, chromeGroup)
	}
}

func TestCodeServerBuildDefaultsWithoutPasswordsOrToken(t *testing.T) {
	tmpl, _ := Get(templateIDCodeServer)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved[paramKeyWorkspace] != "/config/workspace" {
		t.Fatalf("expected default defaultWorkspace, got %v", resolved[paramKeyWorkspace])
	}
	rendered, err := tmpl.Build(resolved)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	container := rendered.Containers[0]
	if container.Name != templateIDCodeServer || container.Image != "lscr.io/linuxserver/code-server:latest" {
		t.Fatalf("unexpected container name/image: %+v", container)
	}
	for _, env := range container.Env {
		if env.Name == envPassword || env.Name == "SUDO_PASSWORD" || env.Name == "CLAUDE_CODE_OAUTH_TOKEN" {
			t.Fatalf("did not expect %s to be set when left blank, got %+v", env.Name, container.Env)
		}
	}
	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != codeServerPort || container.Ports[0].Name != portNameHTTP {
		t.Fatalf("unexpected container ports: %+v", container.Ports)
	}
	if len(rendered.ServicePorts) != 1 || rendered.ServicePorts[0].TargetPort.IntVal != codeServerPort {
		t.Fatalf("unexpected service ports: %+v", rendered.ServicePorts)
	}
}

func TestCodeServerBuildAppliesPasswordsWorkspaceAndClaudeToken(t *testing.T) {
	tmpl, _ := Get(templateIDCodeServer)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{
		paramKeyWorkspace:    "/config/project",
		paramKeyPassword:     "hunter2",
		paramKeySudoPassword: "sudopw",
		paramKeyClaudeToken:  "sk-ant-oat-test",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rendered, err := tmpl.Build(resolved)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	env := map[string]string{}
	for _, e := range rendered.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env[envPassword] != "hunter2" || env["SUDO_PASSWORD"] != "sudopw" || env["DEFAULT_WORKSPACE"] != "/config/project" {
		t.Fatalf("unexpected env: %+v", env)
	}
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-test" {
		t.Fatalf("expected CLAUDE_CODE_OAUTH_TOKEN to be passed through, got %+v", env)
	}
}

// TestCodeServerInstallsClaudeCodeViaInitContainer guards the mechanism this
// template exists for: the Claude Code CLI must be installed into the
// shared /config volume (via the official claude.ai installer, the same one
// registry.coder.com/coder/claude-code's own install script wraps) before
// code-server starts, regardless of whether a token was supplied — an empty
// token only skips authentication, never the install itself.
func TestCodeServerInstallsClaudeCodeViaInitContainer(t *testing.T) {
	tmpl, _ := Get(templateIDCodeServer)
	rendered, err := tmpl.Build(map[string]any{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rendered.InitContainers) != 1 {
		t.Fatalf("expected exactly one init container, got %d", len(rendered.InitContainers))
	}
	init := rendered.InitContainers[0]
	if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].Name != configVolumeName {
		t.Fatalf("expected init container to mount the shared config volume, got %+v", init.VolumeMounts)
	}
	if len(init.Command) == 0 || !strings.Contains(init.Command[len(init.Command)-1], "claude.ai/install.sh") {
		t.Fatalf("expected init container command to install Claude Code from claude.ai/install.sh, got %+v", init.Command)
	}
	// Regression guard for a real incident: with no CPU request, the
	// installer got starved on an oversubscribed node and Init:0/1 sat for
	// minutes looking stuck (see codeserver.go's doc comment).
	if init.Resources.Requests.Cpu().IsZero() {
		t.Fatalf("expected init container to declare a CPU request, got %+v", init.Resources)
	}
}

// TestCodeServerRunsWithNoBuiltInAuthByDefault guards the fix for why this
// template was reverted once before (see CodeServer's doc comment): its
// login page must not exist unless the caller explicitly opts in with a
// password, since this workload's only intended access path is through the
// operator's own authenticated gateway.
func TestCodeServerRunsWithNoBuiltInAuthByDefault(t *testing.T) {
	tmpl, _ := Get(templateIDCodeServer)
	resolved, err := ResolveParams(tmpl.Parameters, map[string]any{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rendered, err := tmpl.Build(resolved)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, env := range rendered.Containers[0].Env {
		if env.Name == envPassword || env.Name == "HASHED_PASSWORD" {
			t.Fatalf("expected no password env vars by default, got %+v", rendered.Containers[0].Env)
		}
	}
}
