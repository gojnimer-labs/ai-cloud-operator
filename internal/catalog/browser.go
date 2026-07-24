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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Shared string/numeric literals across the catalog package (source and
// tests) — extracted so the same value is never typed twice with a chance
// to drift out of sync.
const (
	templateIDNginx   = "nginx"
	templateIDFirefox = "firefox"
	templateIDChrome  = "chrome"
	templateIDWebtop  = "webtop"

	// profileSourceKeyFirefox/profileSourceKeyChrome/profileSourceKeyWebtop
	// are each template's files-table group — computed once here and reused
	// by both browserParameters (the restore picker's DataSource.Group) and
	// backupStateFunction (the uploadUrl param's DataSource.Group, and the
	// FileResult a successful backup produces), so the two never drift
	// apart the way separate ad hoc string concats at each call site could.
	profileSourceKeyFirefox = "profiles_" + templateIDFirefox
	profileSourceKeyChrome  = "profiles_" + templateIDChrome
	profileSourceKeyWebtop  = "profiles_" + templateIDWebtop

	portNameHTTP       = "http"
	portNameHTTPS      = "https"
	entrypointLabelWeb = "Web"
	browserHTTPPort    = int32(3000)
	browserHTTPSPort   = int32(3001)

	// dshmVolumeName names the memory-backed /dev/shm EmptyDir every
	// KasmVNC/Electron-based template (chrome, webtop) mounts — desktop
	// apps and Chromium both need real shared memory, more than the tiny
	// default Kubernetes gives a container's /dev/shm.
	dshmVolumeName = "dshm"
	dshmMountPath  = "/dev/shm"

	browserConfigMountPath = "/config"
	configVolumeName       = "config"
	envProfileDownloadURL  = "PROFILE_DOWNLOAD_URL"

	// envFileManagerPath is docker-baseimage-selkies's own env var
	// (https://github.com/linuxserver/docker-baseimage-selkies) for where its
	// Selkies web UI's file-transfer ("Files") tab reads/writes uploads and
	// downloads. Every template built on that base image (firefox, chrome,
	// webtop — code-server and nginx use unrelated, non-Selkies images) sets
	// it to browserConfigMountPath itself, one level above the unset
	// default. Confirmed live, 2026-07-23: with FILE_MANAGER_PATH unset, the
	// tab was rooted at "$browserConfigMountPath/Desktop" — a sibling of
	// "$browserConfigMountPath/Downloads" (where the desktop's own
	// XDG_DOWNLOAD_DIR and any in-browser "Save file" dialog actually land
	// files), so a user could reach one but not the other depending on which
	// folder they happened to save into. Rooting one level higher, at
	// browserConfigMountPath itself, makes both reachable in the same tab.
	envFileManagerPath = "FILE_MANAGER_PATH"

	// paramKeyStartURL is the shared key for the optional "open this URL on
	// launch" deploy-time parameter chrome/firefox each declare (see
	// startURLParameter) — not webtop, which runs a full desktop rather than
	// one specific browser, so "the URL to open" isn't a meaningful concept
	// for it.
	paramKeyStartURL = "startUrl"

	// envPUID/envPGID/envTZ are the standard linuxserver.io image env var
	// *names* (see
	// https://docs.linuxserver.io/general/understanding-puid-and-pgid/);
	// linuxserverUID/linuxserverTimezone are the values every template
	// built on one of their images (firefox, chrome, code-server) sets
	// them to. Never duplicated as inline string literals, both to avoid
	// drift and because 3+ occurrences of the same literal trips
	// golangci-lint's goconst check.
	envPUID             = "PUID"
	envPGID             = "PGID"
	envTZ               = "TZ"
	linuxserverUID      = "1000"
	linuxserverTimezone = "Etc/UTC"

	// shShellPath is the interpreter every init/exec script in this package
	// runs under — 3+ occurrences of the bare literal trips goconst.
	shShellPath = "/bin/sh"

	// alpineImage is this package's standard lightweight init-container
	// base — 3+ occurrences of the bare literal trips goconst.
	alpineImage = "alpine:latest"

	paramKeyLogLevel       = "logLevel"
	logLevelInfo           = "info"
	logLevelWarn           = "warn"
	logLevelError          = "error"
	paramKeyUploadURL      = "uploadUrl"
	paramKeyRestoreProfile = "restoreProfile"
	paramKeyLabel          = "label"
	paramKeyProfileName    = "profileName"
	paramKeyProfileURL     = "profileDownloadUrl"

	// initialTemplateVersion is every template's Version until its
	// Parameters change for the first time — see Template.Version.
	initialTemplateVersion = "1.0.0"
)

// browserParameters returns the parameter set shared by firefox/chrome: a
// user-facing choice of whether/what profile to restore, and a
// system-computed presigned download URL Convex fills in when restore is
// requested — never an editable field, since an editable URL here would let
// the operator's init container curl an arbitrary user-supplied address.
//
// profileSourceKey scopes the profile picker (and, on the Convex side,
// wherever backed-up profiles get recorded) to one specific browser —
// Firefox and Chrome profile tarballs aren't interchangeable, so they must
// never share one files-table group. Callers pass a distinct key per
// template (see firefox.go/chrome.go).
func browserParameters(profileSourceKey string) []Parameter {
	return []Parameter{
		{
			Key:         paramKeyProfileName,
			Label:       "Profile name",
			Description: "Identifies which saved profile to restore, if any.",
			// The value is a files-table row id, not a literal profile name —
			// Convex resolves it back to an actual R2 object when restoring (see
			// convex/workloads/actions.ts#deployWorkload).
			Type:       ParameterTypeSelect,
			DataSource: DataSource{Kind: DataSourceFileOptions, Group: profileSourceKey},
			// Only meaningful when a restore was actually requested — same
			// condition as profileDownloadUrl below, so the picker doesn't invite
			// a choice that deployWorkload will silently ignore because
			// restoreProfile never got toggled on. Required (not just visible):
			// toggling "restore saved profile" on with no profile actually picked
			// used to deploy silently anyway (ResolveParams/the frontend's zod
			// schema both only exempt Required for a *hidden* parameter — once
			// Visibility makes this one visible, an unset Required here was
			// simply never enforced at all). Visibility's existing "hidden means
			// exempt" rule still applies when restoreProfile is off.
			Validation: Validation{Required: true},
			Visibility: &Visibility{DependsOn: paramKeyRestoreProfile, Op: VisibilityEquals, Value: true},
		},
		{
			Key:        paramKeyRestoreProfile,
			Label:      "Restore saved profile",
			Type:       ParameterTypeBoolean,
			DataSource: DataSource{Kind: DataSourceStatic},
			Default:    false,
		},
		{
			Key:   paramKeyProfileURL,
			Label: "Profile download URL (system)",
			Type:  ParameterTypeString,
			DataSource: DataSource{
				Kind:        DataSourceFile,
				Direction:   DirectionDownload,
				SourceParam: paramKeyProfileName,
			},
			// Only meaningful when a restore was actually requested — machine
			// enforcement of what used to be just this doc comment's say-so.
			Visibility: &Visibility{DependsOn: paramKeyRestoreProfile, Op: VisibilityEquals, Value: true},
		},
	}
}

// restoreProfileInitContainer builds the init container that restores a
// browser profile from profileDownloadURL (an R2 presigned GET URL Convex
// computed) before the main browser container starts. The URL travels as an
// env var, never string-interpolated into the shell script itself, so it
// can't break out of quoting.
//
// Deliberately uses only tools already built into alpine:latest's BusyBox
// (tar, gzip, wget) rather than `apk add curl tar gzip` — that install step
// needs network access to Alpine's own package index just to start a
// container that, in the common no-restore-requested case, needs no network
// at all. Any wget failure (missing profile, network error) is treated as
// "start fresh" rather than distinguished by HTTP status.
func restoreProfileInitContainer(profilePath string, profileDownloadURL string) corev1.Container {
	script := fmt.Sprintf(`set -e
PROFILE_DIR="/config/%s"
mkdir -p "$PROFILE_DIR"
if [ -n "$PROFILE_DOWNLOAD_URL" ]; then
  echo "Attempting profile restore from R2..."
  if wget -q -O /tmp/profile.tar.gz "$PROFILE_DOWNLOAD_URL"; then
    tar -xzf /tmp/profile.tar.gz -C /config
    rm -f /tmp/profile.tar.gz
    echo "Profile restored successfully"
  else
    echo "No existing profile found (or download failed), starting fresh"
    rm -f /tmp/profile.tar.gz
  fi
else
  echo "No profile restore requested, starting fresh"
fi
chown -R 1000:1000 /config
chmod -R 755 /config
`, profilePath)

	return corev1.Container{
		Command: []string{shShellPath, "-c", script},
		Env: []corev1.EnvVar{
			{Name: envProfileDownloadURL, Value: profileDownloadURL},
		},
		Image: alpineImage,
		Name:  "restore-profile",
		VolumeMounts: []corev1.VolumeMount{
			{MountPath: browserConfigMountPath, Name: configVolumeName},
		},
	}
}

// backupStateFunction builds the "backup_state" Operation shared by
// firefox/chrome: the first instance of the reusable operation pattern (see
// catalog.Operation) — a named operation against an already-running
// workload, discovered by the frontend through the same catalog response as
// deploy-time parameters, with its own Parameters (including a
// file-sourced uploadUrl Convex computes, mirroring how profileDownloadUrl
// works for deploy-time restore).
//
// profilePath and uploadUrl are passed as sh positional parameters ($1, $2)
// rather than interpolated into the script string, so a URL containing
// shell-meaningful characters (S3 presigned URLs are full of & and % in
// their query string) can never be misparsed as script syntax.
//
// profileSourceKey is the same dynamic-select source browserParameters
// scopes the restore picker to (see its own doc comment) — passed
// explicitly here rather than derived from containerName, which only
// happens to equal the template ID for today's browser templates.
func backupStateFunction(profilePath, containerName, profileSourceKey string) Operation {
	return Operation{
		Key:         "backup_state",
		Label:       "Backup profile",
		Description: "Tars the current browser profile and uploads it to R2 so it can be restored into a future deploy.",
		// Real side effect (tar + upload) — a deliberate user action, not
		// something safe to silently re-invoke just to check a value.
		Refreshable: false,
		Parameters: []Parameter{
			{
				Key:         paramKeyLabel,
				Label:       "Backup name",
				Description: "A name to identify this saved profile later, when restoring it into a future deploy.",
				Type:        ParameterTypeString,
				DataSource:  DataSource{Kind: DataSourceStatic},
			},
			{
				Key:   paramKeyUploadURL,
				Label: "Upload URL (system)",
				Type:  ParameterTypeString,
				DataSource: DataSource{
					Kind:      DataSourceFile,
					Direction: DirectionUpload,
					Group:     profileSourceKey,
				},
				Validation: Validation{Required: true},
			},
		},
		// The success result carries a stable, namespaced message key
		// ("backup_state.success") in AdditionalInfo, not raw shell stdout
		// — tar/curl both run silently (no -v/-s output to surface), so
		// stdout was never meaningfully informative anyway, and a literal
		// English string can't be localized (the frontend looks this key
		// up in its own translation table) — plus a FileResult so Convex
		// records this backup as a future restore option, using "label"
		// (read here, with a timestamp fallback if the caller left it
		// blank). Failures instead surface as a real Go error below
		// (wrapping stderr), which the API layer returns as a plain error
		// string, not a translatable AdditionalInfo.
		Run: func(ctx context.Context, exec PodExecutor, pod PodRef, params map[string]any) (OperationResult, error) {
			uploadURL := paramString(params, paramKeyUploadURL, "")
			if uploadURL == "" {
				return OperationResult{}, fmt.Errorf("uploadUrl is required")
			}
			// tar exiting 1 means "some files changed while being read" (GNU
			// tar's own distinction from a fatal error, exit 2+) — an
			// unavoidable race with the browser actively writing to its
			// profile's SQLite/IDB files, not a failed backup. Only treat
			// exit codes above 1 as fatal; `|| { ... }` (not `set -e`
			// directly on the tar line) is what lets the script inspect
			// that exit code instead of aborting on any nonzero status.
			const script = `set -e
tar czf /tmp/backup.tar.gz -C /config "$1" || {
  rc=$?
  if [ "$rc" -gt 1 ]; then
    exit "$rc"
  fi
}
curl -sf -X PUT --upload-file /tmp/backup.tar.gz "$2"
rm -f /tmp/backup.tar.gz
`
			command := []string{shShellPath, "-c", script, "sh", profilePath, uploadURL}
			_, stderr, err := exec.Exec(ctx, pod.Namespace, pod.PodName, containerName, command)
			if err != nil {
				return OperationResult{}, fmt.Errorf("backup exec failed: %w (stderr: %s)", err, stderr)
			}
			label := paramString(params, paramKeyLabel, "")
			if label == "" {
				label = fmt.Sprintf("Backup %s", time.Now().UTC().Format(time.RFC3339))
			}
			return OperationResult{
				AdditionalInfo: []AdditionalInfo{
					{Name: "result", Type: AdditionalInfoPlain, Value: "backup_state.success"},
				},
				File: &FileResult{Type: "browser_profile_backup", Label: label},
			}, nil
		},
	}
}

// fileManagerPathEnv is the envFileManagerPath env var every Selkies-based
// template (firefox, chrome, webtop) sets identically — see
// envFileManagerPath's own doc comment for why browserConfigMountPath (not
// its default) is the right value.
func fileManagerPathEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: envFileManagerPath, Value: browserConfigMountPath}
}

// startURLParameter returns the optional deploy-time parameter chrome and
// firefox each declare for themselves (never folded into browserParameters,
// which webtop also uses via the same append pattern — an "open this URL on
// launch" field would be meaningless on a full desktop). appLabel names the
// application in the description text (e.g. "Chrome", "Firefox").
func startURLParameter(appLabel string) Parameter {
	return Parameter{
		Key:         paramKeyStartURL,
		Label:       "Default URL",
		Description: fmt.Sprintf("URL to open automatically when %s starts. Leave blank to use the browser's own default new-tab page.", appLabel),
		Type:        ParameterTypeString,
		DataSource:  DataSource{Kind: DataSourceStatic},
	}
}

func browserResources(cpu, memRequest, memLimit string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memLimit),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memRequest),
		},
	}
}

// browserProbe always targets browserHTTPPort — firefox/chrome are the only
// callers, and both images listen on the same port.
func browserProbe(initialDelay int32) *corev1.Probe {
	return &corev1.Probe{
		FailureThreshold:    3,
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       10,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt32(browserHTTPPort),
			},
		},
		TimeoutSeconds: 5,
	}
}

func resourceQuantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
