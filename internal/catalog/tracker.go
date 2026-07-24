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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	templateIDChromiumTracker = "chromium-tracker"

	// profileSourceKeyChromiumTracker scopes this template's own saved-
	// profile catalog — same reasoning as profileSourceKeyFirefox/Chrome/
	// Webtop (browser.go): never share a files-table group with another
	// template's profiles, since a Chrome/Firefox/Webtop profile tarball
	// isn't interchangeable with this template's (which additionally
	// carries the ai-cloud-tracker extension's own captured usage history —
	// see ChromiumTracker's own doc comment).
	profileSourceKeyChromiumTracker = "profiles_" + templateIDChromiumTracker

	// trackerInstallScriptURL is gojnimer-labs/ai-cloud-tracker's own
	// self-installer (github.com/gojnimer-labs/ai-cloud-tracker/blob/main/
	// scripts/install.sh) — downloads that repo's prebuilt, unpacked
	// extension/ directory into a target dir via wget+tar alone (no git, no
	// Node/pnpm needed at install time), the same curl-pipe-to-sh shape as
	// installClaudeCodeInitContainer's claude.ai/install.sh. Re-run on every
	// pod start (see trackerExtensionsVolumeName's own doc comment for why
	// that's deliberately not cached), so the deployed extension always
	// tracks that repo's main branch.
	trackerInstallScriptURL = "https://raw.githubusercontent.com/gojnimer-labs/ai-cloud-tracker/main/scripts/install.sh"

	// trackerExtensionInstallDir must match install.sh's own
	// TRACKER_INSTALL_DIR default — passed explicitly here anyway (rather
	// than relying on that default silently matching) so a future default
	// change on the ai-cloud-tracker side can't quietly break this
	// template.
	trackerExtensionInstallDir = "/extensions/poc"

	trackerExtensionsMountPath  = "/extensions"
	trackerExtensionsVolumeName = "extensions"

	// trackerStartURL is the startUrl parameter's Default (see
	// startURLParameter) — this template exists specifically to run
	// gojnimer-labs/ai-cloud-tracker against chatgpt.com (see that repo's
	// own README for what it captures), so unlike Chrome/Firefox's blank
	// default (their own new-tab page), a blank default here would defeat
	// the template's whole purpose. Still user-overridable, not hardcoded,
	// in case a future ai-cloud-tracker version targets a different site.
	trackerStartURL = "https://chatgpt.com"

	trackerConfigVolumeSize = "2Gi"
)

// installTrackerExtensionInitContainer builds the init container that fetches
// gojnimer-labs/ai-cloud-tracker's prebuilt extension into a shared, ephemeral
// EmptyDir (trackerExtensionsVolumeName) the main Chromium container then
// loads unpacked via CHROME_CLI. Deliberately alpine + wget/tar only (both
// built into BusyBox), matching this package's other init containers'
// preference for not installing anything (see restoreProfileInitContainer's
// own doc comment) — install.sh itself only needs those two tools.
//
// Bounded and non-fatal (`timeout ... || echo ...`, same shape as
// installClaudeCodeInitContainer): a network hiccup fetching the extension
// must never block the browser itself from coming up, even though the
// resulting workload won't actually be tracking anything without it.
func installTrackerExtensionInitContainer() corev1.Container {
	script := `set -e
timeout 60 sh -c 'wget -qO- "$1" | sh' _ "` + trackerInstallScriptURL + `" || echo "ai-cloud-tracker install failed or timed out — continuing without it, the browser will still start"
chown -R 1000:1000 ` + trackerExtensionsMountPath + `
`

	return corev1.Container{
		Command: []string{shShellPath, "-c", script},
		Env: []corev1.EnvVar{
			{Name: "TRACKER_INSTALL_DIR", Value: trackerExtensionInstallDir},
		},
		Image:     alpineImage,
		Name:      "install-tracker-extension",
		Resources: browserResources("250m", "128Mi", "256Mi"),
		VolumeMounts: []corev1.VolumeMount{
			{MountPath: trackerExtensionsMountPath, Name: trackerExtensionsVolumeName},
		},
	}
}

// ChromiumTracker deploys open-source Chromium (not branded Google Chrome —
// see the ai-cloud-tracker repo's own README for why that distinction is
// load-bearing here: stable-channel Chrome silently ignores
// --load-extension outside Chrome for Testing, Chromium doesn't) with
// gojnimer-labs/ai-cloud-tracker's usage-tracking extension force-loaded,
// plus the same restoreProfile/backup_state/startUrl support Chrome/Firefox
// have (see browserParameters/backupStateFunction/startURLParameter in
// browser.go) — this is a real Chromium profile like any other browser
// template's, so there's no reason it shouldn't be saveable/restorable too.
//
// Unlike Firefox/Chrome/Webtop, /config is a PersistentVolumeClaim, not an
// EmptyDir — this template's whole purpose is tracking a logged-in user's
// usage over time, so the chatgpt.com login (and therefore the extension's
// own captured usage history, which chrome.storage.local persists inside
// the profile directory under /config) must survive a pod restart, the
// same reasoning as CodeServer's /config (see its own doc comment). The
// extensions volume is the opposite: a plain EmptyDir, deliberately
// re-fetched fresh by the init container on every start (see
// trackerInstallScriptURL's doc comment) rather than persisted, so a
// restart always picks up the latest ai-cloud-tracker main.
var ChromiumTracker = Template{
	Build: func(params map[string]any) (Rendered, error) {
		profileDownloadURL := paramString(params, paramKeyProfileURL, "")
		startURL := paramString(params, paramKeyStartURL, trackerStartURL)

		return Rendered{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{Name: envPUID, Value: linuxserverUID},
						{Name: envPGID, Value: linuxserverUID},
						{Name: envTZ, Value: linuxserverTimezone},
						fileManagerPathEnv(),
						{
							Name: envChromeCLI,
							Value: "--load-extension=" + trackerExtensionInstallDir +
								" --disable-extensions-except=" + trackerExtensionInstallDir +
								" " + startURL,
						},
					},
					Image:         "lscr.io/linuxserver/chromium:latest",
					LivenessProbe: browserProbe(30),
					Name:          templateIDChromiumTracker,
					Ports: []corev1.ContainerPort{
						{ContainerPort: browserHTTPPort, Name: portNameHTTP},
						{ContainerPort: browserHTTPSPort, Name: portNameHTTPS},
					},
					ReadinessProbe: browserProbe(15),
					Resources:      browserResources("1000m", "1500Mi", "3Gi"),
					VolumeMounts: []corev1.VolumeMount{
						{MountPath: browserConfigMountPath, Name: configVolumeName},
						{MountPath: dshmMountPath, Name: dshmVolumeName},
						{MountPath: trackerExtensionsMountPath, Name: trackerExtensionsVolumeName, ReadOnly: true},
					},
				},
			},
			InitContainers: []corev1.Container{
				// Restores into all of /config — same reasoning as chrome.go/
				// firefox.go's own identical comment: this backs up the
				// whole Chromium profile, not just a browser-internal
				// subdirectory, which here also means the extension's own
				// captured usage history (stored via chrome.storage.local
				// inside the profile dir) survives a restore too, not just
				// login/cookies.
				restoreProfileInitContainer(profileDownloadURL),
				installTrackerExtensionInitContainer(),
			},
			ServicePorts: []corev1.ServicePort{
				{Name: portNameHTTP, Port: 80, TargetPort: intstr.FromInt32(browserHTTPPort)},
			},
			Volumes: []corev1.Volume{
				{
					Name: configVolumeName,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: configVolumeName},
					},
				},
				{
					Name: dshmVolumeName,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: resourceQuantityPtr("4Gi"),
						},
					},
				},
				{Name: trackerExtensionsVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			PersistentVolumeClaims: []PersistentVolumeClaimSpec{
				{
					Name:        configVolumeName,
					StorageSize: resource.MustParse(trackerConfigVolumeSize),
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			},
		}, nil
	},
	Operations:  []Operation{backupStateFunction(templateIDChromiumTracker, profileSourceKeyChromiumTracker)},
	Description: "Chromium pre-loaded with gojnimer-labs/ai-cloud-tracker, tracking ChatGPT usage limits",
	Entrypoints: []Entrypoint{{Name: portNameHTTP, Label: entrypointLabelWeb}},
	ID:          templateIDChromiumTracker,
	Icon:        "📊",
	Name:        "ChatGPT Usage Tracker",
	Version:     initialTemplateVersion,
	Parameters: append(
		browserParameters(profileSourceKeyChromiumTracker),
		Parameter{
			Key:         paramKeyStartURL,
			Label:       "Default URL",
			Description: "URL to open automatically when the browser starts.",
			Type:        ParameterTypeString,
			DataSource:  DataSource{Kind: DataSourceStatic},
			Default:     trackerStartURL,
		},
	),
}
