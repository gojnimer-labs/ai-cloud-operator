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

	paramKeyClaudeToken     = "claudeCodeOauthToken"
	paramKeyAnthropicAPIKey = "anthropicApiKey"

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
// linuxserver/code-server's own startup, then installs the Claude Code CLI
// via the official installer (https://claude.ai/install.sh — the same one
// registry.coder.com/coder/claude-code's own install step wraps).
//
// Directory-ownership fix: code-server runs as the "abc" user (uid 1000,
// confirmed via `ps aux` inside the container), but /config/workspace,
// /config/data (--user-data-dir), and /config/extensions
// (--extensions-dir) all come up owned by root:root 0755 — created fresh
// by linuxserver's own startup, after which nothing chowns them to match
// PUID/PGID. Result: "EACCES: permission denied" the moment a user tries
// to save a file. Pre-creating these three directories here, before the
// main container ever starts, means linuxserver's own `mkdir -p`-shaped
// startup logic finds them already present (a no-op) instead of creating
// fresh root-owned ones — same defensive shape as
// restoreProfileInitContainer's own `mkdir -p` + chown.
//
// The install itself has a real history worth knowing before touching this
// again. It used to hang forever (100% CPU, ~zero syscalls) on this
// cluster's node hardware — confirmed live via root SSH + `strace -p
// <host-pid>` run directly from the node (bypassing the container's own
// ptrace restrictions): the hung process burned CPU while making almost no
// syscalls over an 8-second trace window, a genuine userspace compute
// spin, not blocked I/O. Musl vs glibc, TTY presence, DISABLE_AUTOUPDATER,
// CPU limits, and seccomp (`/proc/<pid>/status` showed `Seccomp: 0` — not
// even active) were all ruled out first. The actual cause: the node's VM
// had a QEMU generic "Common KVM processor" CPU type, missing AVX, AVX2,
// SSE4, SSSE3, even POPCNT — Node.js/V8 ran fine on that same hardware,
// only the Bun-compiled `claude` binary hung, consistent with a JIT-engine
// bug triggered by assumed-present CPU features being absent. **Fixed at
// the infrastructure level, not here**: the Proxmox VM's CPU type was
// changed from the generic model to host passthrough (confirmed live,
// 2026-07-23 — real CPU flags now visible, e.g. avx2/bmi2/popcnt, and the
// official installer completes normally, `claude --version` returns
// instantly). If Claude Code ever seems to hang on install again, suspect
// the VM's CPU type before this script — check `lscpu`/`/proc/cpuinfo` on
// the node first.
//
// Bounded and non-fatal regardless (`timeout ... || echo ...`, no bare
// `set -e` exposure on that line): code-server must come up either way,
// with or without Claude Code, even though the install is expected to
// succeed now.
//
// PATH is exported into both .bashrc and .profile rather than just one —
// code-server's integrated terminal spawns bash as an interactive
// non-login shell (sources .bashrc), but a user attaching some other way
// (e.g. a login shell over `coder ssh`-style access) would only source
// .profile.
//
// Carries an explicit CPU request (via browserResources, despite the
// name) — unlike every other init container in this package (see
// EstimatedResources' doc comment: "none of today's templates set
// Resources on one"). Found the hard way on a real deploy: with no
// request, the installer got starved on an oversubscribed node and
// Init:0/1 sat for minutes looking stuck.
func installClaudeCodeInitContainer() corev1.Container {
	const script = `set -e
apk add --no-cache bash curl ca-certificates >/dev/null
export HOME=` + claudeInstallHome + `
mkdir -p "$HOME" "$HOME/data" "$HOME/extensions" "` + defaultWorkspacePath + `"
timeout 90 bash -c 'curl -fsSL https://claude.ai/install.sh | bash -s -- stable' || echo "Claude Code install failed or timed out — continuing without it, code-server will still start"
for rcfile in "$HOME/.bashrc" "$HOME/.profile"; do
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$rcfile"
done
chown -R 1000:1000 "$HOME"
`

	return corev1.Container{
		Command:   []string{shShellPath, "-c", script},
		Image:     alpineImage,
		Name:      "install-claude-code",
		Resources: browserResources("500m", "128Mi", "256Mi"),
		VolumeMounts: []corev1.VolumeMount{
			{MountPath: browserConfigMountPath, Name: configVolumeName},
		},
	}
}

// disableCasualEnvDumpHook returns a postStart lifecycle hook that wraps
// /usr/bin/env and /usr/bin/printenv so a bare, no-argument invocation
// (the "just show me everything" form) prints a short message instead of
// dumping the container's environment — including CLAUDE_CODE_OAUTH_TOKEN/
// ANTHROPIC_API_KEY, which necessarily land in this container as plain env
// vars (that's the only interface the Claude Code CLI reads either from;
// see CodeServer's own doc comment for why that's unavoidable here). This
// is explicitly a deterrent against a non-technical end-user casually
// running a familiar command out of curiosity, not a security boundary:
// anyone who goes looking has plenty of equivalent ways to read the same
// environment (a bash builtin like `export`/`set`/`declare -x`,
// /proc/self/environ, a one-line python3/node snippet — all left
// untouched, deliberately, since this container is a coding IDE and
// disabling them isn't practical without breaking normal use). Both
// wrapped commands still work exactly as before when called *with*
// arguments — `env some-command args...` and `printenv SPECIFIC_VAR` pass
// straight through to the real binary — only the bare form is intercepted.
// env in particular can't just be deleted: it's also the standard shebang
// interpreter-resolution mechanism (`#!/usr/bin/env python3`), which real
// dev tooling depends on constantly.
//
// Can't be an init container: those run in a separate filesystem from the
// main container and can only touch what's explicitly shared (this
// package's other init containers only ever write to the shared /config
// volume) — never the main image's own /usr/bin. A postStart hook runs
// inside the main container's own filesystem right after it starts, which
// is what direct binary replacement needs. Ends with an explicit `exit 0`
// — a postStart hook that exits non-zero gets the whole container killed
// and restarted per Kubernetes' own semantics, which would turn a cosmetic
// nice-to-have into an outage, and without it the script's exit status
// would be whatever the last executed command happened to return (e.g.
// falsy on a re-run where both binaries are already wrapped, since the
// last evaluated `[ ! -f "$real" ]` check would itself be false). Verified
// directly in a throwaway container (not just read): the wrapper blocks
// bare `env`/`printenv`, passes through `env <cmd> [args]` and `printenv
// VAR` unchanged, and is idempotent on a second run. Timing is best-effort
// too (no ordering guarantee against the container's own entrypoint), but
// linuxserver's s6 init and the operator's own gateway-auth round trip
// both take meaningfully longer than this hook's few filesystem
// operations, so in practice it's in place long before a human could
// reach a terminal.
func disableCasualEnvDumpHook() *corev1.Lifecycle {
	const script = `
for cmd in env printenv; do
  bin="/usr/bin/$cmd"
  real="$bin.real"
  if [ -f "$bin" ] && [ ! -f "$real" ]; then
    mv "$bin" "$real"
    cat > "$bin" <<'WRAP'
#!/bin/sh
if [ $# -eq 0 ]; then
  echo "$0: environment listing is disabled on this workspace" >&2
  exit 1
fi
WRAP
    echo "exec \"$real\" \"\$@\"" >> "$bin"
    chmod +x "$bin"
  fi
done
exit 0
`

	return &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{shShellPath, "-c", script},
			},
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
// ANTHROPIC_API_KEY is also supported as an alternative credential (direct
// API billing instead of an OAuth/subscription-backed token) — set
// whichever fits; both were exercised and confirmed working during the
// investigation that got the underlying install hang fixed (see
// installClaudeCodeInitContainer's doc comment for that story).
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
		anthropicAPIKey := paramString(params, paramKeyAnthropicAPIKey, "")

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
		if anthropicAPIKey != "" {
			env = append(env, corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: anthropicAPIKey})
		}

		return Rendered{
			Containers: []corev1.Container{
				{
					Env:            env,
					Image:          "lscr.io/linuxserver/code-server:latest",
					Lifecycle:      disableCasualEnvDumpHook(),
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
			// host).
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
	// 1.3.0: reverted to the official Claude Code installer (latest/
	// "stable", full OAuth-token support) now that the actual root cause of
	// the install hang — this cluster's Proxmox VM using a generic,
	// feature-minimal CPU type — was fixed at the infrastructure level
	// (switched to host CPU passthrough, confirmed live). 1.2.0's
	// claudeCodeNpmVersion pin and its ANTHROPIC_API_KEY-only workaround
	// are no longer needed for that reason, but anthropicApiKey stays as a
	// genuinely useful alternative credential, not just a leftover. 1.1.0
	// dropped password/sudoPassword/defaultWorkspace — redundant with the
	// operator's own gateway auth, and a fixed workspace path serves this
	// template's actual purpose (a consistent Claude Code install
	// location) better than per-deploy configurability did.
	Version: "1.3.0",
	Parameters: []Parameter{
		{
			Description: "OAuth token for Claude Code (from `claude setup-token`). Leave blank to skip authentication — the CLI is still installed but requires a manual `claude login` from the integrated terminal.",
			Key:         paramKeyClaudeToken,
			Label:       "Claude Code OAuth token",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
		{
			Description: "Anthropic API key (from console.anthropic.com) — an alternative to the OAuth token above, useful if you'd rather bill Claude Code usage directly to an API key than a Claude subscription. Leave blank if using the OAuth token instead.",
			Key:         paramKeyAnthropicAPIKey,
			Label:       "Anthropic API key",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
	},
}
