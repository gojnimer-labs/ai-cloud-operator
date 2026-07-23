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

	paramKeyClaudeToken = "claudeCodeOauthToken"

	// claudeInstallHome is where the init container installs Claude Code —
	// the shared /config volume, so the binary is already present on
	// codeServerPort's container by the time code-server starts. Matches
	// linuxserver/code-server's own $HOME for the "abc" user, so a shell
	// opened in code-server's integrated terminal is the same $HOME the
	// installer ran against.
	claudeInstallHome = browserConfigMountPath

	// defaultWorkspacePath is fixed, not user-configurable — this template
	// exists to give Claude Code a consistent, known install/working
	// location, and a per-deploy-customizable path bought nothing for that
	// beyond a parameter to plumb through and re-validate in the init
	// container's directory-ownership fix (see
	// installClaudeCodeInitContainer's doc comment).
	defaultWorkspacePath = "/config/workspace"
)

// codeServerProbe targets codeServerPort — deliberately not browserProbe
// (see its own doc comment), which is hard-coded to browserHTTPPort for
// firefox/chrome specifically. Plain HTTP, confirmed live: linuxserver/
// code-server does not speak TLS on this port by default (curl against the
// pod IP got a clean HTTP/1.1 302; a TLS handshake attempt against the same
// port failed outright with "wrong version number") — don't set Scheme:
// HTTPS here without re-verifying that's changed.
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

// installClaudeCodeInitContainer prepares the shared /config volume before
// code-server starts: fixes a real, confirmed directory-ownership gap in
// linuxserver/code-server's own startup (see the dedicated comment block
// below), then installs the Claude Code CLI via the same official installer
// registry.coder.com/coder/claude-code's own install script wraps
// (https://claude.ai/install.sh) — mimicking that module's install step,
// minus the Coder-agent-specific parts (script bin dir symlink, tmux
// session) that don't apply outside a Coder workspace. Re-runs on every pod
// start since /config is a plain EmptyDir, same as every other
// browser-family template here (see restoreProfileInitContainer) — an
// acceptable cost since this template always needs network access to
// install Claude Code at all, unlike a conditional profile restore.
//
// Directory-ownership fix (found live, 2026-07-23, via `kubectl exec ...
// stat`): code-server actually runs as the "abc" user (uid 1000, confirmed
// via `ps aux` inside the container), but /config/workspace,
// /config/data (--user-data-dir), and /config/extensions
// (--extensions-dir) all came up owned by root:root 0755 — created fresh
// by linuxserver's own startup *after* this init container's later
// `chown -R 1000:1000 "$HOME"` had already run against an /config that
// didn't have them yet, and never chowned by linuxserver's own init
// afterward. Result: "EACCES: permission denied" the moment a user tries
// to save a file in the editor. Pre-creating these three directories here,
// before the main container ever starts, means linuxserver's own `mkdir
// -p`-shaped startup logic finds them already present (a no-op) instead of
// creating fresh root-owned ones — same defensive shape as
// restoreProfileInitContainer's own `mkdir -p` + chown.
//
// Deliberately NOT alpine, unlike this package's other init containers (see
// restoreProfileInitContainer) — debian-slim was tried after a real deploy
// first hung on Alpine specifically (musl libc). That turned out to be a
// red herring: live debugging on the actual cluster (root SSH + kubectl
// exec, 2026-07-23) found the installer hangs identically on debian-slim
// (glibc) — and in fact hangs on a bare `claude --version`, with no
// install/network/TTY involved at all. Zero syscalls, ~100% CPU across two
// threads, 100% reproducible on every attempt. Not a musl bug, not a TUI
// bug, not fixed by DISABLE_AUTOUPDATER — most likely the CLI's own
// bundled runtime (Bun, going by the epoll/eventfd fds and ~5GB reserved
// VmSize) hitting a container-security restriction this cluster enforces
// (kernel io_uring is enabled here, but the default container seccomp
// profile very plausibly still blocks the io_uring syscalls Bun wants —
// confirming that needs an unconfined-seccomp debug pod, which is a real
// security-posture call outside this template's authority to make
// unilaterally). Kept on debian-slim anyway since there's no evidence
// alpine is any better and no reason to reintroduce that variable.
//
// Given the CLI may simply not run in this cluster today, the install is
// bounded and non-fatal (`timeout ... || echo ...`, no bare `set -e`
// exposure on that line) rather than blocking the pod: code-server must
// come up either way, with or without Claude Code. See the Description on
// CodeServer's own doc comment update — 100% reproducible in testing, so
// 45s is enough slack for a slow download without leaving every pod start
// waiting on something that's never once succeeded here.
//
// PATH is exported into both .bashrc and .profile rather than just one —
// code-server's integrated terminal spawns bash as an interactive
// non-login shell (sources .bashrc), but a user attaching some other way
// (e.g. a login shell over `coder ssh`-style access) would only source
// .profile — cheap to cover both rather than assume one. Harmless to run
// even when the install above failed: just points PATH at a directory
// that happens not to exist yet.
//
// Carries an explicit CPU request (via browserResources, despite the name)
// — unlike every other init container in this package (see
// EstimatedResources' doc comment: "none of today's templates set
// Resources on one"). Doesn't fix the hang above, but there's no reason to
// leave it unset once identified.
func installClaudeCodeInitContainer() corev1.Container {
	const script = `set -e
apt-get update -qq
apt-get install -y -qq --no-install-recommends curl ca-certificates >/dev/null
export HOME=` + claudeInstallHome + `
mkdir -p "$HOME" "$HOME/data" "$HOME/extensions" "` + defaultWorkspacePath + `"
timeout 45 bash -c 'curl -fsSL https://claude.ai/install.sh | bash -s -- stable' || echo "Claude Code install failed or timed out — continuing without it, code-server will still start"
for rcfile in "$HOME/.bashrc" "$HOME/.profile"; do
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$rcfile"
done
chown -R 1000:1000 "$HOME"
`

	return corev1.Container{
		Command:   []string{shShellPath, "-c", script},
		Image:     "debian:bookworm-slim",
		Name:      "install-claude-code",
		Resources: browserResources("500m", "128Mi", "256Mi"),
		VolumeMounts: []corev1.VolumeMount{
			{MountPath: browserConfigMountPath, Name: configVolumeName},
		},
	}
}

// CodeServer deploys code-server (https://github.com/coder/code-server) — VS
// Code accessible via the browser — via linuxserver.io's image, for the same
// PUID/PGID/TZ/config-volume conventions as firefox/chrome/webtop, with a
// best-effort attempt to install and authenticate the Claude Code CLI the
// same way kubernetes-generic's Coder template wires up its claude-code
// module: an install step plus CLAUDE_CODE_OAUTH_TOKEN as a plain env var,
// which the CLI reads on its own — no separate `claude login` step needed.
// "Best-effort" because, as of 2026-07-23, the Claude Code CLI binary does
// not actually run in this cluster (hangs even on `--version`; see
// installClaudeCodeInitContainer's doc comment) — code-server itself still
// comes up either way, just without a working `claude` yet.
//
// Deliberately runs with no code-server password at all (PASSWORD/
// HASHED_PASSWORD/SUDO_PASSWORD are never set — not even offered as
// parameters) — this workload is only ever reachable through the
// operator's own /gw/ gateway, which already authenticates every request
// via a signed, workload-scoped session cookie (see
// internal/gateway/token.go) before it ever proxies to this Service. A
// second credential here would be redundant, not defense-in-depth: nothing
// else can reach this Service to present it against. This is the same
// "no per-workload auth" shape firefox/chrome/webtop already use. It's
// also a deliberate change from this template's first
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
		claudeToken := paramString(params, paramKeyClaudeToken, "")

		env := []corev1.EnvVar{
			{Name: envPUID, Value: linuxserverUID},
			{Name: envPGID, Value: linuxserverUID},
			{Name: envTZ, Value: linuxserverTimezone},
			{Name: "DEFAULT_WORKSPACE", Value: defaultWorkspacePath},
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
			// Named "http", plain HTTP — confirmed live (2026-07-23) by
			// curling the pod IP directly: port codeServerPort answers a
			// normal HTTP/1.1 302 redirect, and a TLS handshake against the
			// same port fails outright ("wrong version number"), so it does
			// NOT speak TLS. Naming this "https" would make
			// internal/gateway/proxy.go dial it over TLS and fail outright
			// — see serviceProxyScheme's own doc comment for when that
			// naming is actually correct to use.
			//
			// The real failure this entrypoint hit end-to-end wasn't a
			// scheme mismatch at all: the operator's gateway used to relay
			// every entrypoint through the Kubernetes API server's
			// services/proxy subresource, and the operator's own logs
			// showed `httputil: ReverseProxy read error during body copy:
			// unexpected EOF` live on this exact workload — something
			// nginx/firefox/chrome/webtop's much smaller payloads never
			// triggered. Addressed at the gateway level, not here — see
			// internal/gateway/proxy.go's own doc comment — by proxying
			// directly to the Service's ClusterIP instead of through that
			// subresource, which also closes an independent WebSocket-path
			// risk this template actually depends on (terminal, extension
			// host). That doc comment is explicit that the exact EOF
			// trigger wasn't reproduced under direct testing, though — this
			// still needs a real browser check once redeployed, not just
			// trust in the theory.
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
	// 1.1.0: dropped password/sudoPassword/defaultWorkspace — redundant
	// with the operator's own gateway auth, and a fixed workspace path
	// serves this template's actual purpose (a consistent Claude Code
	// install location) better than per-deploy configurability did. See
	// CodeServer's own doc comment for the reasoning.
	Version: "1.1.0",
	Parameters: []Parameter{
		{
			Description: "OAuth token for Claude Code (from `claude setup-token`). Leave blank to skip authentication — the CLI is still installed but requires a manual `claude login` from the integrated terminal.",
			Key:         paramKeyClaudeToken,
			Label:       "Claude Code OAuth token",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
	},
}
