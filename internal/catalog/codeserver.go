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

	// claudeCodeNpmVersion is pinned, not "latest" — see
	// codeServerLifecycleHook's doc comment for the full story. Short
	// version: every Claude Code release from some point on ships as a
	// Bun-compiled native binary that hangs forever (100% CPU, ~zero
	// syscalls) on this cluster's actual node hardware — a QEMU "Common KVM
	// processor" CPU model missing AVX/AVX2/SSE4/SSSE3/POPCNT, very
	// plausibly tripping a JIT-engine bug. 0.2.9 predates that switch:
	// still a plain Node.js script, confirmed working live. Bump this only
	// after testing a candidate version directly on this cluster (or
	// whatever cluster this template is running on) — "newer version might
	// have fixed it" is not something to assume.
	claudeCodeNpmVersion = "0.2.9"

	// defaultWorkspacePath is fixed, not user-configurable — this template
	// exists to give Claude Code a consistent, known install/working
	// location, and a per-deploy-customizable path bought nothing for that
	// beyond a parameter to plumb through and re-validate in the init
	// container's directory-ownership fix (see
	// prepareConfigDirsInitContainer's doc comment).
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

// prepareConfigDirsInitContainer fixes a real, confirmed directory-ownership
// gap in linuxserver/code-server's own startup (found live, 2026-07-23, via
// `kubectl exec ... stat`): code-server actually runs as the "abc" user
// (uid 1000, confirmed via `ps aux` inside the container), but
// /config/workspace, /config/data (--user-data-dir), and /config/extensions
// (--extensions-dir) all came up owned by root:root 0755 — created fresh by
// linuxserver's own startup, after which nothing chowns them to match
// PUID/PGID. Result: "EACCES: permission denied" the moment a user tries to
// save a file in the editor. Pre-creating these three directories here,
// before the main container ever starts, means linuxserver's own `mkdir
// -p`-shaped startup logic finds them already present (a no-op) instead of
// creating fresh root-owned ones — same defensive shape as
// restoreProfileInitContainer's own `mkdir -p` + chown, including reusing
// alpine as the base image (this container no longer runs anything
// libc-sensitive — see codeServerLifecycleHook's doc comment for why the
// Claude Code install itself moved to a postStart hook on the main
// container instead of living here).
func prepareConfigDirsInitContainer() corev1.Container {
	const script = `set -e
mkdir -p "` + browserConfigMountPath + `/data" "` + browserConfigMountPath + `/extensions" "` + defaultWorkspacePath + `"
chown -R 1000:1000 "` + browserConfigMountPath + `"
`

	return corev1.Container{
		Command: []string{shShellPath, "-c", script},
		Image:   alpineImage,
		Name:    "prepare-config-dirs",
		VolumeMounts: []corev1.VolumeMount{
			{MountPath: browserConfigMountPath, Name: configVolumeName},
		},
	}
}

// codeServerLifecycleHook returns the main container's postStart hook —
// there can only be one per container, so this does two unrelated jobs in
// one script rather than two hooks:
//
//  1. Wraps /usr/bin/env and /usr/bin/printenv so a bare, no-argument
//     invocation prints a message instead of dumping the environment
//     (which includes CLAUDE_CODE_OAUTH_TOKEN/ANTHROPIC_API_KEY as plain
//     env vars — the only interface the CLI reads either from). This is a
//     deterrent against a non-technical end-user casually running a
//     familiar command, not a security boundary — bash builtins
//     (export/set/declare -x), /proc/self/environ, and any one-line
//     python3/node snippet are all left untouched, deliberately, since
//     this is a coding IDE and disabling them isn't practical without
//     breaking normal use. Both commands still work fully when called
//     *with* arguments (`env some-cmd args...`, `printenv VAR`) — env
//     can't be deleted outright, it's also the standard shebang
//     interpreter-resolution mechanism (`#!/usr/bin/env python3`) real dev
//     tooling depends on. Verified directly in a throwaway container, not
//     just read: bare calls blocked, argument-form calls pass through,
//     idempotent on a second run.
//
//  2. Installs the Claude Code CLI, pinned to claudeCodeNpmVersion via npm
//     — a deliberately unusual choice with a real story behind it. The
//     official installer (https://claude.ai/install.sh, what
//     registry.coder.com/coder/claude-code's own install step wraps, and
//     what an earlier version of this template used) downloads a
//     Bun-compiled native binary that hangs indefinitely on this cluster:
//     confirmed live (2026-07-23) via root SSH + `strace -p <host-pid>`
//     run directly from the node (bypassing the container's own ptrace
//     restrictions) that the hung process burns ~100% CPU while making
//     almost no syscalls over an 8-second trace window — a genuine
//     userspace compute spin, not blocked I/O. Ruled out first: musl vs
//     glibc (identical hang on debian-slim), TTY presence,
//     DISABLE_AUTOUPDATER, CPU limits, and seccomp (`/proc/<pid>/status`
//     showed `Seccomp: 0` — not active at all, despite that being the
//     leading theory for a while). The actual culprit found last: this
//     node's CPU (`lscpu`) is QEMU's generic "Common KVM processor"
//     model — missing AVX, AVX2, SSE4, SSSE3, even POPCNT, an unusually
//     minimal instruction set. Node.js (V8) runs fine on this exact
//     hardware (tested directly); only the Bun-compiled claude binary
//     hangs — consistent with a JIT-engine bug triggered by missing CPU
//     features the runtime assumes are present. npm's own "install
//     claude-code" package turned out to be no different — as of version
//     2.1.218 it's just a thin installer that fetches the same native
//     binary via postinstall, not an escape from this at all.
//
//     claudeCodeNpmVersion=0.2.9 predates that switch entirely: its npm
//     package is a plain `#!/usr/bin/env node` script (confirmed via the
//     installed file's own shebang), so it runs on Node's V8 — verified
//     working live, `claude --version` returns instantly, no hang.
//     Trade-off, and a real one: this is a very old release, missing
//     everything shipped since. It also predates
//     `CLAUDE_CODE_OAUTH_TOKEN`-style OAuth auth entirely (grepped the
//     installed package source — not present in 0.2.9 or even the newer
//     0.2.100) — only `ANTHROPIC_API_KEY` works with this pinned version,
//     hence that parameter existing alongside the (currently inert, kept
//     for forward-compatibility) OAuth one. A slightly newer old version,
//     0.2.100, also works but pulls in `better-sqlite3`, a native addon
//     with no prebuilt binary for this platform — needs a full
//     build-essential + python3 toolchain and ~2 minutes to compile from
//     source. 0.2.9 has no such dependency (2-second install, confirmed
//     live) and still exposes a reasonably complete command set
//     (/clear, /compact, /config, /cost, /doctor, /init, /review, /login,
//     /logout, etc.) — chosen over 0.2.100 for that install-cost reason
//     specifically, not because it's meaningfully more capable.
//
//     Installed via apt (nodejs + npm from Ubuntu 24.04's own repos,
//     confirmed present in this image — no external NodeSource dependency
//     needed) rather than an init container: init containers run in a
//     separate filesystem from the main container, and both `npm install
//     -g`'s default location and the Node.js runtime itself need to end up
//     somewhere *this* container can reach at runtime — the shared
//     /config volume doesn't help here, since what's missing is Node
//     itself, not just where npm happens to place its output. A postStart
//     hook runs inside the main container's own filesystem, which is the
//     only place this actually works. Backgrounded (`&`) and given its own
//     timeout so a slow/stuck network or apt operation can't hold up
//     anything else — this container's own postStart return happens fast
//     regardless, matching the env/printenv wrapping above. Failures here
//     are silent by design (redirected to a log file, `|| true`-shaped):
//     code-server must come up either way, with or without a working
//     `claude`.
func codeServerLifecycleHook() *corev1.Lifecycle {
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

(
  timeout 180 sh -c '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends nodejs npm
    npm install -g @anthropic-ai/claude-code@` + claudeCodeNpmVersion + `
  '
) > /tmp/claude-install.log 2>&1 &

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
// Claude Code CLI installed and authenticated via a plain env var — see
// codeServerLifecycleHook's doc comment for the version pin this needed and
// why, and for why ANTHROPIC_API_KEY (not CLAUDE_CODE_OAUTH_TOKEN) is the
// parameter that actually works today.
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
		// Kept even though it's currently inert with claudeCodeNpmVersion
		// pinned this old (see codeServerLifecycleHook's doc comment) — a
		// future version bump that lands on a non-hanging release with
		// OAuth support should make this work again with no parameter
		// changes needed.
		if claudeToken != "" {
			env = append(env, corev1.EnvVar{Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: claudeToken})
		}
		// This is what claudeCodeNpmVersion's pinned release actually
		// authenticates with.
		if anthropicAPIKey != "" {
			env = append(env, corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: anthropicAPIKey})
		}

		return Rendered{
			Containers: []corev1.Container{
				{
					Env:            env,
					Image:          "lscr.io/linuxserver/code-server:latest",
					Lifecycle:      codeServerLifecycleHook(),
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
				prepareConfigDirsInitContainer(),
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
	// 1.2.0: added anthropicApiKey — the credential that actually
	// authenticates Claude Code with claudeCodeNpmVersion pinned this old
	// (see codeServerLifecycleHook's doc comment). 1.1.0 dropped
	// password/sudoPassword/defaultWorkspace — redundant with the
	// operator's own gateway auth, and a fixed workspace path serves this
	// template's actual purpose (a consistent Claude Code install
	// location) better than per-deploy configurability did.
	Version: "1.2.0",
	Parameters: []Parameter{
		{
			Description: "OAuth token for Claude Code (from `claude setup-token`). Currently has no effect: the pinned Claude Code CLI version this template installs predates OAuth-token auth entirely (see the code comment on codeServerLifecycleHook for why it's pinned this old). Kept for forward-compatibility — use anthropicApiKey instead for now.",
			Key:         paramKeyClaudeToken,
			Label:       "Claude Code OAuth token (currently inert — see anthropicApiKey)",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
		{
			Description: "Anthropic API key (from console.anthropic.com). This is what actually authenticates Claude Code today — the pinned CLI version predates OAuth-token auth. Leave blank to skip authentication — the CLI is still installed but requires a manual `claude login` from the integrated terminal.",
			Key:         paramKeyAnthropicAPIKey,
			Label:       "Anthropic API key",
			DataSource:  DataSource{Kind: DataSourceStatic},
			Type:        ParameterTypeString,
		},
	},
}
