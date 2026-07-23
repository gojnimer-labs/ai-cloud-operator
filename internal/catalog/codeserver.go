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

	paramKeyPassword     = "password"
	paramKeySudoPassword = "sudoPassword"
	paramKeyWorkspace    = "defaultWorkspace"
	paramKeyClaudeToken  = "claudeCodeOauthToken"

	// claudeInstallHome is where the init container installs Claude Code —
	// the shared /config volume, so the binary is already present on
	// codeServerPort's container by the time code-server starts. Matches
	// linuxserver/code-server's own $HOME for the "abc" user, so a shell
	// opened in code-server's integrated terminal is the same $HOME the
	// installer ran against.
	claudeInstallHome = browserConfigMountPath
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

// installClaudeCodeInitContainer installs the Claude Code CLI onto the
// shared /config volume before code-server starts, via the same official
// installer registry.coder.com/coder/claude-code's own install script wraps
// (https://claude.ai/install.sh) — mimicking that module's install step,
// minus the Coder-agent-specific parts (script bin dir symlink, tmux
// session) that don't apply outside a Coder workspace. Re-runs on every pod
// start since /config is a plain EmptyDir, same as every other
// browser-family template here (see restoreProfileInitContainer) — an
// acceptable cost since this template always needs network access to
// install Claude Code at all, unlike a conditional profile restore.
//
// Runs on alpine (already this package's standard init-container base, see
// restoreProfileInitContainer) with bash/curl/ca-certificates added: the
// installer script itself requires bash, which alpine doesn't ship by
// default.
//
// PATH is exported into both .bashrc and .profile rather than just one —
// code-server's integrated terminal spawns bash as an interactive
// non-login shell (sources .bashrc), but a user attaching some other way
// (e.g. a login shell over `coder ssh`-style access) would only source
// .profile — cheap to cover both rather than assume one.
//
// Carries an explicit CPU request (via browserResources, despite the name)
// — unlike every other init container in this package (see
// EstimatedResources' doc comment: "none of today's templates set
// Resources on one"). Found the hard way on a real deploy: with no
// request, the installer binary (briefly ~90%+ CPU on its own) gets
// starved rather than scheduled fairly on an oversubscribed node, so
// Init:0/1 can sit for minutes looking stuck when it's actually just
// waiting for CPU time. 500m is enough for the kernel/scheduler to give it
// a fair share without reserving a full core for what's normally a
// few-seconds download-and-extract.
func installClaudeCodeInitContainer() corev1.Container {
	const script = `set -e
apk add --no-cache bash curl ca-certificates >/dev/null
export HOME=` + claudeInstallHome + `
mkdir -p "$HOME"
curl -fsSL https://claude.ai/install.sh | bash -s -- stable
for rcfile in "$HOME/.bashrc" "$HOME/.profile"; do
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$rcfile"
done
chown -R 1000:1000 "$HOME"
`

	return corev1.Container{
		Command:   []string{shShellPath, "-c", script},
		Image:     "alpine:latest",
		Name:      "install-claude-code",
		Resources: browserResources("500m", "128Mi", "256Mi"),
		VolumeMounts: []corev1.VolumeMount{
			{MountPath: browserConfigMountPath, Name: configVolumeName},
		},
	}
}

// CodeServer deploys code-server (https://github.com/coder/code-server) — VS
// Code accessible via the browser — via linuxserver.io's image, for the same
// PUID/PGID/TZ/config-volume conventions as firefox/chrome/webtop, with the
// Claude Code CLI installed and authenticated the same way
// kubernetes-generic's Coder template wires up its claude-code module: an
// install step plus CLAUDE_CODE_OAUTH_TOKEN as a plain env var, which the
// CLI reads on its own — no separate `claude login` step needed.
//
// Deliberately runs with no code-server password (PASSWORD/HASHED_PASSWORD
// both left unset unless the caller opts in) — this workload is only ever
// reachable through the operator's own /gw/ gateway, which already
// authenticates every request via a signed, workload-scoped session cookie
// (see internal/gateway/token.go) before it ever proxies to this Service.
// This is the same "no per-workload auth" shape firefox/chrome/webtop
// already use. It's also a deliberate change from this template's first
// attempt (see git history: "Add/Remove the code-server catalog template"),
// which defaulted to code-server's own password-protected login page and
// was reverted after that page 502'd through the cluster's Traefik
// front-door — a Traefik-specific routing issue never resolved, not a bug in
// the operator's own gateway (which proxied it fine directly). Removing the
// page entirely removes that failure surface rather than re-risking it —
// but the Traefik front-door path itself is still unverified end-to-end
// from this change alone.
var CodeServer = Template{
	Build: func(params map[string]any) (Rendered, error) {
		password := paramString(params, paramKeyPassword, "")
		sudoPassword := paramString(params, paramKeySudoPassword, "")
		defaultWorkspace := paramString(params, paramKeyWorkspace, "/config/workspace")
		claudeToken := paramString(params, paramKeyClaudeToken, "")

		env := []corev1.EnvVar{
			{Name: envPUID, Value: linuxserverUID},
			{Name: envPGID, Value: linuxserverUID},
			{Name: envTZ, Value: linuxserverTimezone},
			{Name: "DEFAULT_WORKSPACE", Value: defaultWorkspace},
		}
		// Omitted entirely rather than passed as "" — an explicit empty
		// PASSWORD/SUDO_PASSWORD env var is not the same thing to the
		// image's entrypoint script as the var being unset (see
		// linuxserver/code-server's docs: no PASSWORD/HASHED_PASSWORD means
		// no auth at all, which is what this template wants by default).
		if password != "" {
			env = append(env, corev1.EnvVar{Name: envPassword, Value: password})
		}
		if sudoPassword != "" {
			env = append(env, corev1.EnvVar{Name: "SUDO_PASSWORD", Value: sudoPassword})
		}
		// Blank is a valid, supported state: the CLI is still installed by
		// the init container below, just unauthenticated until a user runs
		// `claude login` themselves from the integrated terminal.
		if claudeToken != "" {
			env = append(env, corev1.EnvVar{Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: claudeToken})
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
			InitContainers: []corev1.Container{
				installClaudeCodeInitContainer(),
			},
			ServicePorts: []corev1.ServicePort{
				{Name: portNameHTTP, Port: 80, TargetPort: intstr.FromInt32(codeServerPort)},
			},
			Volumes: []corev1.Volume{
				{Name: configVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		}, nil
	},
	Description: "VS Code in the browser (code-server), with the Claude Code CLI pre-installed",
	Entrypoints: []Entrypoint{{Name: portNameHTTP, Label: entrypointLabelWeb}},
	ID:          templateIDCodeServer,
	Icon:        "💻",
	Name:        "VS Code (Browser)",
	Version:     initialTemplateVersion,
	Parameters: []Parameter{
		{
			Description: "Login password for the code-server web UI. Leave blank: this workload is only reachable through the operator's own authenticated gateway, so no separate password is needed.",
			Key:         paramKeyPassword,
			Label:       "Password",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
		{
			Description: "Enables sudo inside the container's terminal when set — passed through as SUDO_PASSWORD.",
			Key:         paramKeySudoPassword,
			Label:       "Sudo password",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
		{
			Default:     "/config/workspace",
			Description: "Folder code-server opens by default.",
			Key:         paramKeyWorkspace,
			Label:       "Default workspace path",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
		{
			Description: "OAuth token for Claude Code (from `claude setup-token`). Leave blank to skip authentication — the CLI is still installed but requires a manual `claude login` from the integrated terminal.",
			Key:         paramKeyClaudeToken,
			Label:       "Claude Code OAuth token",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
	},
}
