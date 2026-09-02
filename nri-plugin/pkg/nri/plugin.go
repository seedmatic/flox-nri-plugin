package nri

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
)

const (
	// Per-container annotation prefixes. The plugin reads
	// "<prefix>.<container-name>" — there is NO fallback to the bare key or
	// to a pod-wide annotation. Each container in a pod opts in independently.
	floxEnvAnnotationPrefix   = "flox.seedmatic.io/environment" // <prefix>.<container> = "category/name" (opt into a flox env)
	floxHomeAnnotationPrefix  = "flox.seedmatic.io/home"        // <prefix>.<container> = override HOME (optional)
	floxUIDAnnotationPrefix   = "flox.seedmatic.io/uid"         // <prefix>.<container> = desired UID (optional, default: 0)
	floxGIDAnnotationPrefix   = "flox.seedmatic.io/gid"         // <prefix>.<container> = desired GID (optional, default: 0)
	floxDebugAnnotationPrefix = "flox.seedmatic.io/debug"       // <prefix>.<container> = "true" to enable debug pause
	floxDebugPortPrefix       = "flox.seedmatic.io/debug-port"  // <prefix>.<container> = delve port (default: 2345)
	// nixBuildAnnotationPrefix opts a container into the nix-build capability:
	// flox.seedmatic.io/nix-build.<container> = "<pvc-name>". The pod-mutating webhook ensures that
	// PVC + mounts it at nixBuildStoreMount; we host the /nix store overlay's upper/work THERE
	// (instead of the default 2g tmpfs) so the container's `nix build` — which materialises a whole
	// maven closure into the store — has room + a persistent warm cache. We also put `nix` on PATH.
	nixBuildAnnotationPrefix = "flox.seedmatic.io/nix-build"
	// nixBuildStoreMount is where the webhook mounts the assigned nix-store PVC — the shared contract
	// between the webhook and this plugin's overlay upper_backing. Keep in sync with the webhook.
	nixBuildStoreMount = "/var/lib/flox-nri/nix-build-store"
	// Store-resolved env handoff. The runtime installer realizes each env's
	// static subtree (env/{manifest.toml,manifest.lock}) into /nix/store and
	// GC-roots it at <floxEnvGcrootBase>/<category>/<name>. We readlink that
	// gcroot to get the immutable store path, then flox-nri-env-link-hook.sh
	// builds a fine-grained .flox symlink farm in the container (env/ -> store,
	// run/cache/log local). The container's /nix/store overlay lowers from the
	// host store, so the same store path resolves inside the container.
	// This supersedes the old /var/run env-dir copy + overlay-mount of .flox.
	floxEnvGcrootBase   = "/nix/var/nix/gcroots/flox-runtime/env"
	floxOverlayHookPath = "/usr/local/sbin/flox-nri-overlay-hook.sh"
	floxEnvLinkHookPath = "/usr/local/sbin/flox-nri-env-link-hook.sh"
	floxChownHookPath   = "/usr/local/sbin/flox-nri-chown-hook.sh"
	// System-wide flox config maintained on the host by rke2lab-env-load.sh.
	// We bind-mount it read-only into every flox-injected container so the in-
	// container `flox` invocation honors host policy (telemetry off, channel
	// lock, ...) without each pod having to set FLOX_* env vars.
	floxSystemConfigPath = "/etc/flox.toml"
	// The container's command is `flox activate …`, so flox must be on PATH BEFORE
	// activation. flox is NOT in the injected env (that holds the workload's own
	// packages) — it lives in the node's NixOS system profile. We resolve its
	// /nix/store bin (visible in the container via the /nix/store overlay) and put
	// it on PATH ourselves: the plugin already depends on flox, so it owns bringing
	// it in (retires the flox-env `NIX_DEFAULT_PROFILE_BIN_STORE_PATH` ConfigMap key).
	nixosSystemFloxBin = "/run/current-system/sw/bin/flox"
	// nixosSystemNixBin: the node's `nix` CLI (NixOS system profile). A nix-build container gets it
	// on PATH the same way flox does — the plugin owns bringing the nix runtime in, so no flox env
	// has to ship `nix`. Resolved to its store bin dir (visible via the /nix overlay).
	nixosSystemNixBin = "/run/current-system/sw/bin/nix"
	defaultDebugPort  = "2345"
	defaultUID        = "0"
	defaultGID        = "0"
	rootHome          = "/root"
)

// FloxPlugin implements the NRI plugin interface for flox environment injection
//
// Annotations are PER-CONTAINER. The plugin reads
// "<prefix>.<container-name>" on the pod's annotations map. There is NO
// fallback to a bare "<prefix>" key — each container must opt in by name.
// This lets a pod mix flox-injected and unmodified containers freely.
//
// Supported keys (substitute <c> with the container name):
//   - flox.seedmatic.io/environment.<c>: Flox environment "category/name" (opt into a flox env)
//   - flox.seedmatic.io/home.<c>:        Override HOME directory (optional, defaults to HOME env var, then /root)
//   - flox.seedmatic.io/uid.<c>:         Desired UID for flox env ownership (optional, default: 0)
//   - flox.seedmatic.io/gid.<c>:         Desired GID for flox env ownership (optional, default: 0)
//   - flox.seedmatic.io/debug.<c>:       Set to "true" to enable debug mode (optional)
//   - flox.seedmatic.io/debug-port.<c>:  Delve debugger port (optional, default: 2345)
//   - flox.seedmatic.io/nix-build.<c>:   Opt into the nix-build runtime — value is the PVC the
//     webhook mounts at nixBuildStoreMount; we host the /nix overlay upper there + put nix on PATH
//
// Behavior:
//   - Determines HOME directory: flox.seedmatic.io/home.<c> > HOME env var > /root
//   - Resolves environment: "category/name" -> the store path GC-rooted at
//     /nix/var/nix/gcroots/flox-runtime/env/{category}/{name} by the installer
//   - Materializes a fine-grained .flox symlink farm at $HOME/.flox (env/ -> store,
//     run/cache/log local) via flox-nri-env-link-hook.sh
//   - Mounts /nix/store with overlayfs protection (read-only lower, writable ephemeral upper)
//   - Allows `flox activate --dir $HOME` to automatically discover the environment
//
// Note: Since NRI doesn't expose the container's UID at the protocol level on
// older runtimes, pods may set the HOME env var to match the user they run as
// (e.g., HOME=/root for UID 0, HOME=/home/user for others).
type FloxPlugin struct {
	stub stub.Stub
	ctx  context.Context
}

// NewFloxPlugin creates a new flox NRI plugin instance
func NewFloxPlugin(ctx context.Context) (*FloxPlugin, error) {
	return &FloxPlugin{
		ctx: ctx,
	}, nil
}

// Configure handles plugin configuration (required by NRI)
func (p *FloxPlugin) Configure(ctx context.Context, config string) (stub.EventMask, error) {
	log.Printf("Flox NRI plugin configured")

	// Subscribe to container lifecycle events
	return api.MustParseEventMask(
		"CreateContainer",
	), nil
}

// Synchronize handles state sync after plugin restart (required by NRI)
func (p *FloxPlugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	log.Printf("Synchronizing state: %d pods, %d containers", len(pods), len(containers))
	return nil, nil
}

// Shutdown handles plugin shutdown (required by NRI)
func (p *FloxPlugin) Shutdown(ctx context.Context) {
	log.Println("Flox NRI plugin shutting down")
}

// CreateContainer handles container creation and injects flox environment
func (p *FloxPlugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	containerName := container.GetName()

	// Per-container annotation lookup. NO fallback: a container without one of the
	// flox.seedmatic.io/<capability>.<container-name> annotations is left untouched, even if a
	// sibling container in the same pod opted in. A container may opt into a flox env, the
	// nix-build runtime, or both.
	envKey := perContainerKey(floxEnvAnnotationPrefix, containerName)
	floxEnv := getAnnotation(pod, envKey)
	nixBuild := getAnnotation(pod, perContainerKey(nixBuildAnnotationPrefix, containerName))
	if floxEnv == "" && nixBuild == "" {
		return nil, nil, nil // opted into nothing
	}

	adjustment := &api.ContainerAdjustment{}
	var createHooks []*api.Hook
	var prestartHooks []*api.Hook

	// The /nix overlay is needed by BOTH capabilities. CreateContainer hooks run in the container's
	// mount namespace before pivot_root, so what they mount persists after pivot_root (CreateRuntime
	// runs in the host ns and would NOT reach the container). ONE overlay over all of /nix gives a
	// VALID store — the files + the registration DB (/nix/var/nix/db) — so flox activation is a
	// cache-hit and a nix build has a writable store. upper/work default to the hook's 2g tmpfs; a
	// nix-build container hosts them on its assigned PVC (mounted by the webhook at
	// nixBuildStoreMount) for room + a persistent warm cache. The hook strips the host's stale
	// daemon socket so nix stays on the local store.
	nixOverlayArgs := []string{floxOverlayHookPath, "nix", "/nix", "/nix"}
	if nixBuild != "" {
		nixOverlayArgs = append(nixOverlayArgs, nixBuildStoreMount)
		log.Printf("nix-build %s/%s: /nix overlay upper on the assigned store PVC at %s (annotation=%q)",
			pod.GetNamespace(), containerName, nixBuildStoreMount, nixBuild)
	}
	createHooks = append(createHooks, &api.Hook{Path: floxOverlayHookPath, Args: nixOverlayArgs})
	log.Printf("Adding CreateContainer hook (nix overlay): %s /nix -> /nix", floxOverlayHookPath)

	// nix-build: the plugin owns the nix runtime (no flox env ships nix).
	if nixBuild != "" {
		// (1) the node's `nix` CLI on PATH.
		if nixBin, nerr := resolveNixStoreBin(); nerr != nil {
			log.Printf("WARNING: could not resolve nix store bin (%v) — nix-build container PATH not adjusted", nerr)
		} else {
			adjustment.AddEnv("PATH", prependPath(nixBin, getEnvVar(container, "PATH")))
			log.Printf("Injected nix onto container PATH: %s", nixBin)
		}
		// (2) NIX_SSL_CERT_FILE → the node's CA bundle. nix fetches flake inputs + substituters over
		// HTTPS, but the workload image (debian-slim) has no CA bundle, and the node's
		// /etc/ssl/certs/… is NOT overlaid (only /nix is). Resolve the node cacert to its /nix/store
		// path (which IS overlaid → visible in the pod) so TLS verification works. NIX_CONFIG (the
		// daemonless knobs + min-free/max-free store GC) is injected by the pod-mutating webhook.
		if caBundle, cerr := resolveNixCaBundle(); cerr != nil {
			log.Printf("WARNING: could not resolve a /nix/store CA bundle (%v) — nix HTTPS fetches may fail", cerr)
		} else {
			adjustment.AddEnv("NIX_SSL_CERT_FILE", caBundle)
			log.Printf("Injected NIX_SSL_CERT_FILE: %s", caBundle)
		}
	}

	// flox env injection — only when the container opted into a flox env.
	if floxEnv != "" {
		log.Printf("Container %s/%s requests flox environment: %s (via %s)",
			pod.GetNamespace(), containerName, floxEnv, envKey)

		// Optional debug pause — also per-container.
		if getAnnotation(pod, perContainerKey(floxDebugAnnotationPrefix, containerName)) == "true" {
			debugPort := getAnnotation(pod, perContainerKey(floxDebugPortPrefix, containerName))
			if debugPort == "" {
				debugPort = defaultDebugPort
			}
			log.Printf("DEBUG MODE: Waiting for debugger on port %s for %s/%s", debugPort, pod.GetNamespace(), containerName)
			log.Printf("Attach with: dlv connect localhost:%s", debugPort)
		}

		// Get UID/GID from container user (NRI v0.12.0+)
		// Priority: 1) Container.User, 2) per-container annotation, 3) default to root
		var uid, gid string
		user := container.GetUser()
		if user != nil {
			uid = fmt.Sprintf("%d", user.GetUid())
			gid = fmt.Sprintf("%d", user.GetGid())
			log.Printf("Container user UID=%s, GID=%s", uid, gid)
		} else {
			uid = getAnnotation(pod, perContainerKey(floxUIDAnnotationPrefix, containerName))
			if uid == "" {
				uid = defaultUID
			}
			gid = getAnnotation(pod, perContainerKey(floxGIDAnnotationPrefix, containerName))
			if gid == "" {
				gid = defaultGID
			}
			log.Printf("Using annotations/defaults: UID=%s, GID=%s", uid, gid)
		}

		// Determine HOME where .flox will be mounted.
		// Priority: flox.seedmatic.io/home.<c> > HOME env var > infer from UID
		homeDir := getAnnotation(pod, perContainerKey(floxHomeAnnotationPrefix, containerName))
		if homeDir == "" {
			homeDir = getEnvVar(container, "HOME")
		}
		if homeDir == "" {
			if uid == "0" {
				homeDir = rootHome
			} else {
				homeDir = fmt.Sprintf("/home/user-%s", uid)
			}
		}

		log.Printf("Using HOME directory: %s (will mount .flox at %s/.flox)", homeDir, homeDir)
		log.Printf("Flox environment ownership: UID=%s, GID=%s", uid, gid)

		floxEnvPath, err := p.resolveFloxEnvironment(floxEnv)
		if err != nil {
			log.Printf("ERROR: Failed to resolve flox environment %s: %v", floxEnv, err)
			return nil, nil, fmt.Errorf("failed to resolve flox environment: %w", err)
		}
		log.Printf("Flox environment %s resolved to store path: %s", floxEnv, floxEnvPath)

		// Target path where we materialize the env's .flox symlink farm: $HOME/.flox
		floxMountTarget := filepath.Join(homeDir, ".flox")

		if uid != defaultUID || gid != defaultGID {
			log.Printf("WARNING: UID/GID mapping requested (uid=%s, gid=%s) but not yet implemented", uid, gid)
			log.Printf("The flox environment source must have correct ownership on the host filesystem")
		}

		// env-link: build the fine-grained .flox symlink farm — env/ symlinks into the resolved
		// store path (immutable default env), run/cache/log are real local writable dirs.
		createHooks = append(createHooks, &api.Hook{
			Path: floxEnvLinkHookPath,
			Args: []string{floxEnvLinkHookPath, floxEnvPath, floxMountTarget},
		})
		log.Printf("Adding CreateContainer hook (env-link): %s %s -> %s", floxEnvLinkHookPath, floxEnvPath, floxMountTarget)

		// Prestart hook fixes ownership of $HOME and $HOME/.flox via ${bundle}/rootfs (host ns).
		prestartHooks = append(prestartHooks, &api.Hook{
			Path: floxChownHookPath,
			Args: []string{floxChownHookPath, uid, gid, homeDir},
		})
		log.Printf("Adding Prestart hook: %s %s %s %s", floxChownHookPath, uid, gid, homeDir)

		// Project host /etc/flox.toml into the container if the host has one. The
		// host file is the system-wide opt-out for flox telemetry (and any future
		// host-wide flox policy). Read-only — pods can't shadow it. Skipping
		// silently when the host file is absent keeps the plugin permissive on
		// nodes that haven't run rke2lab-env-load.sh yet.
		if _, err := os.Stat(floxSystemConfigPath); err == nil {
			adjustment.AddMount(&api.Mount{
				Source:      floxSystemConfigPath,
				Destination: floxSystemConfigPath,
				Type:        "bind",
				Options:     []string{"bind", "ro"},
			})
			log.Printf("Bind-mounted host %s into container", floxSystemConfigPath)
		}

		// Put flox on the container PATH so `flox activate` (the container command)
		// can find it. Prepend flox's store bin to any existing PATH; otherwise seed a
		// sane default alongside it.
		if floxBin, ferr := resolveFloxStoreBin(); ferr != nil {
			log.Printf("WARNING: could not resolve flox store bin (%v) — container PATH not adjusted, `flox activate` may fail", ferr)
		} else {
			newPath := floxBin
			if existing := getEnvVar(container, "PATH"); existing != "" {
				newPath = floxBin + ":" + existing
			} else {
				newPath = floxBin + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
			}
			adjustment.AddEnv("PATH", newPath)
			log.Printf("Injected flox onto container PATH: %s", floxBin)
		}

		// Disable flox's background "check-for-upgrades" in injected containers.
		//
		// WHY: every `flox activate` spawns a DETACHED check-for-upgrades process that
		// runs `nix eval --refresh 'builtins.lockFlakeInstallable "<installable>"'` to
		// look for newer package versions. That eval loads nixpkgs and (with --refresh)
		// bypasses the eval cache, so it spikes memory UNBOUNDEDLY a few seconds AFTER
		// activation returns — OOMKilling the container at ANY cgroup limit. It is
		// pointless here: injected environments are baked, locked, and air-gapped, and
		// their flake installables reference a source tree that isn't present at runtime.
		// Measured: a mesh sidecar OOM'd >256Mi without this; with it, activation peaks
		// at ~8Mi.
		//
		// FRAGILE: `_FLOX_TESTING_DISABLE_BG_SIDE_EFFECTS` is an INTERNAL flox testing
		// knob, not a supported config key — flox reads it in
		// cli/flox/src/commands/check_for_upgrades.rs
		// (spawn_detached_check_for_upgrades_process: `if let Ok(true) =
		// std::env::var("_FLOX_TESTING_DISABLE_BG_SIDE_EFFECTS").unwrap_or_default().parse()
		// { return Ok(()) }`, value must be "true"). It can change or vanish across flox
		// releases. Revisit and switch to a supported mechanism if flox ever exposes one
		// (there is no config-key / non-testing env var for this today).
		adjustment.AddEnv("_FLOX_TESTING_DISABLE_BG_SIDE_EFFECTS", "true")
		log.Printf("Disabled flox background check-for-upgrades (avoids unbounded nix-eval OOM in injected containers)")
	}

	if len(createHooks) > 0 || len(prestartHooks) > 0 {
		adjustment.AddHooks(&api.Hooks{CreateContainer: createHooks, Prestart: prestartHooks})
	}

	log.Printf("Successfully configured flox injection for container %s/%s", pod.GetNamespace(), container.GetName())
	return adjustment, nil, nil
}

// resolveFloxStoreBin returns the /nix/store bin directory that holds the `flox`
// binary. It resolves the node's NixOS system-profile symlink to its store path
// (visible inside the container through the /nix/store overlay), falling back to a
// PATH lookup. Returned as the directory so it can be prepended to $PATH.
func resolveFloxStoreBin() (string, error) {
	if resolved, err := filepath.EvalSymlinks(nixosSystemFloxBin); err == nil {
		return filepath.Dir(resolved), nil
	}
	if p, err := exec.LookPath("flox"); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
			return filepath.Dir(resolved), nil
		}
		return filepath.Dir(p), nil
	}
	return "", fmt.Errorf("flox not found at %s or on PATH", nixosSystemFloxBin)
}

// resolveNixStoreBin returns the /nix/store bin dir holding the node's `nix` CLI (NixOS system
// profile symlink resolved to its store path, visible in the container via the /nix overlay),
// falling back to a PATH lookup. The plugin owns bringing the nix runtime in for nix-build pods.
func resolveNixStoreBin() (string, error) {
	if resolved, err := filepath.EvalSymlinks(nixosSystemNixBin); err == nil {
		return filepath.Dir(resolved), nil
	}
	if p, err := exec.LookPath("nix"); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
			return filepath.Dir(resolved), nil
		}
		return filepath.Dir(p), nil
	}
	return "", fmt.Errorf("nix not found at %s or on PATH", nixosSystemNixBin)
}

// resolveNixCaBundle returns a CA bundle path UNDER /nix/store — the only tree overlaid into the
// pod, so the only cacert the container can actually read. It resolves the node's NIX_SSL_CERT_FILE
// (set on NixOS) or the standard /etc/ssl/certs/ca-certificates.crt symlink to its store target.
func resolveNixCaBundle() (string, error) {
	for _, candidate := range []string{os.Getenv("NIX_SSL_CERT_FILE"), "/etc/ssl/certs/ca-certificates.crt"} {
		if candidate == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if strings.HasPrefix(resolved, "/nix/store/") {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("no /nix/store CA bundle via NIX_SSL_CERT_FILE or /etc/ssl/certs/ca-certificates.crt")
}

// prependPath puts bin at the front of an existing PATH, or seeds a sane default when empty.
func prependPath(bin, existing string) string {
	if existing != "" {
		return bin + ":" + existing
	}
	return bin + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

// RemoveContainer is called when a container is removed
// No cleanup needed with simple bind mount approach
func (p *FloxPlugin) RemoveContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) error {
	return nil
}

// resolveFloxEnvironment resolves a flox environment name to its immutable
// /nix/store path by reading the GC-root the runtime installer published.
//
// Format: "category/name" (e.g., "networking/kdns"). A bare "name" defaults to
// the "networking" category. The installer GC-roots each env at
// <floxEnvGcrootBase>/<category>/<name>; we readlink it to the real store path,
// which exposes <store-path>/env/{manifest.toml,manifest.lock}.
func (p *FloxPlugin) resolveFloxEnvironment(floxEnv string) (string, error) {
	var category, envName string

	envParts := strings.Split(floxEnv, "/")
	if len(envParts) == 2 {
		category = envParts[0]
		envName = envParts[1]
	} else if len(envParts) == 1 {
		category = "networking"
		envName = envParts[0]
	} else {
		return "", fmt.Errorf("invalid flox environment format: %s (expected 'category/name' or 'name')", floxEnv)
	}

	gcroot := filepath.Join(floxEnvGcrootBase, category, envName)

	storePath, err := os.Readlink(gcroot)
	if err != nil {
		return "", fmt.Errorf("flox environment %s not GC-rooted at %s: %w", floxEnv, gcroot, err)
	}

	// Confirm the resolved store path carries the env subtree the link hook expects.
	envSubtree := filepath.Join(storePath, "env")
	if _, err := os.Stat(envSubtree); err != nil {
		return "", fmt.Errorf("flox environment %s store path missing env/ subtree (%s): %w", floxEnv, envSubtree, err)
	}

	log.Printf("Resolving Flox environment '%s' -> category=%s, name=%s, store=%s", floxEnv, category, envName, storePath)

	return storePath, nil
}

// perContainerKey builds a per-container annotation key by suffixing the
// prefix with ".<container-name>". The plugin uses ONLY this form — there is
// no fallback to a bare prefix or a pod-wide annotation.
func perContainerKey(prefix, containerName string) string {
	return prefix + "." + containerName
}

// Helper to get annotation from container or pod
func getAnnotation(obj interface{}, key string) string {
	var annotations map[string]string

	switch v := obj.(type) {
	case *api.PodSandbox:
		annotations = v.GetAnnotations()
	case *api.Container:
		annotations = v.GetAnnotations()
	default:
		return ""
	}

	if annotations == nil {
		return ""
	}

	return annotations[key]
}

// getEnvVar retrieves an environment variable from the container spec
func getEnvVar(container *api.Container, key string) string {
	env := container.GetEnv()
	if env == nil {
		return ""
	}

	// GetEnv returns []string in "KEY=VALUE" format
	// We need to parse it
	prefix := key + "="
	for _, envStr := range env {
		if strings.HasPrefix(envStr, prefix) {
			return strings.TrimPrefix(envStr, prefix)
		}
	}

	return ""
}
