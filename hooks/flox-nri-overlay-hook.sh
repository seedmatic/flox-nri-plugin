#!/usr/bin/env -S bash -euxo pipefail
# OCI CreateContainer hook: mount an overlayfs on a path in the container rootfs.
#
# Runs in the container's mount namespace before pivot_root, so mounts performed
# here persist into the running container after pivot_root. (CreateRuntime hooks
# run in the host namespace and don't propagate in because the container ns is
# unshared with rprivate propagation.)
#
# Reads the OCI container state JSON on stdin (per OCI runtime spec) to extract
# the bundle path; the container rootfs is "${bundle}/rootfs".
#
# Layout: scratch dirs for every overlay are grouped under /.overlays.d/<name>
# inside the container rootfs:
#   /.overlays.d/<name>/lower         bind of <lower-source> from the host (ro)
#   /.overlays.d/<name>/rw            single tmpfs hosting upper/ and work/
#                                     (overlayfs requires upperdir and workdir
#                                     on the same mount)
#   /.overlays.d/<name>/rw/upper      upperdir
#   /.overlays.d/<name>/rw/work       workdir
# An overlay is then mounted at <target>.
#
# The default upper/work backing is an ephemeral 2g tmpfs — right for a sidecar
# whose store overlay only records a few activation writes. A heavy consumer that
# BUILDS into the store (e.g. the in-cluster render's `nix build`, which
# materialises a maven closure into $out) blows past 2g, so it passes an optional
# <upper-backing>: a container-absolute path where a sized, writable volume (a PVC)
# is already mounted. We then host upper/ and work/ THERE instead of a tmpfs — real
# disk + inodes, and (if the volume persists) the built store paths survive across
# runs so the next render is a cache hit.
#
# Usage: flox-nri-overlay-hook.sh <name> <lower-source> <target> [<upper-backing>]
#   <name>          short identifier for the overlay; used as the subfolder
#                   name under /.overlays.d/
#   <lower-source>  host filesystem path used as the read-only lower layer
#   <target>        absolute path inside the container rootfs for the overlay
#                   mountpoint
#   <upper-backing> OPTIONAL container-absolute path of a pre-mounted writable
#                   volume to host upper/ + work/ (both land on it → same mount, as
#                   overlayfs requires). Omitted/empty → the 2g tmpfs default.
#
# Logging: the OCI runtime (containerd/runc) captures the hook's stdout/stderr and
# surfaces it on the pod's events + containerd log, so we log there directly. We do
# NOT reroute through logger(1)/journald: a CreateContainer hook runs in the
# container's restricted mount namespace before pivot_root, where /dev/log and the
# /dev/fd process-substitution plumbing may be absent — `exec > >(logger …)` then
# fails the hook (exit 127) before it can mount anything.

overlay_name="$1"
lower_source="$2"
target_rel="$3"
upper_backing_rel="${4:-}"

state="$(cat)"
bundle="$(printf '%s' "$state" | sed -n 's/.*"bundle"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
container_id="$(printf '%s' "$state" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"

echo "hook invoked container=${container_id} bundle=${bundle} name=${overlay_name} lower=${lower_source} target=${target_rel}"

if [ -z "$bundle" ]; then
    echo "ERROR: could not parse bundle path from OCI state" >&2
    exit 1
fi

rootfs="${bundle%/}/rootfs"
if [ ! -d "$rootfs" ]; then
    echo "ERROR: rootfs does not exist: ${rootfs}" >&2
    exit 1
fi

target="${rootfs}${target_rel}"
overlay_root="${rootfs}/.overlays.d/${overlay_name}"
lower="${overlay_root}/lower"

mkdir -p "$target" "$lower"

mount --bind "$lower_source" "$lower"
mount -o remount,bind,ro "$lower"

# upper/ + work/ backing. overlayfs requires both on the SAME mount.
if [ -n "$upper_backing_rel" ]; then
    # PVC-backed: the volume is already mounted by the runtime at this
    # container-absolute path. Host upper/ + work/ on it directly — real disk +
    # inodes, and it persists across runs so built store paths stay cached.
    rw="${rootfs}${upper_backing_rel}"
    container_rw="${upper_backing_rel}"
    if [ ! -d "$rw" ]; then
        echo "ERROR: upper-backing path not mounted in container: ${upper_backing_rel}" >&2
        exit 1
    fi
    echo "overlay ${overlay_name}: PVC-backed upper/work at ${upper_backing_rel}"
else
    # Ephemeral default: a single 2g tmpfs hosting upper/ and work/.
    rw="${overlay_root}/rw"
    container_rw="/.overlays.d/${overlay_name}/rw"
    mkdir -p "$rw"
    mount -t tmpfs -o mode=0755,size=2g tmpfs "$rw"
    echo "overlay ${overlay_name}: ephemeral 2g tmpfs upper/work"
fi
upper="${rw}/upper"
work="${rw}/work"
mkdir -p "$upper" "$work"

# We MUST chroot into ${rootfs} to create the overlay: overlayfs records
# upperdir/workdir by the PATH given, and reaches workdir by that recorded path
# later during copy-up. A host-absolute path (/run/.../rootfs/.overlays.d/...) is
# gone after pivot_root → copy-up fails with EACCES (regression fixed in db4b06412).
# Chrooting makes overlayfs record CONTAINER-relative paths (/.overlays.d/...),
# valid once the rootfs becomes /.
#
# The chroot severs the host PATH, and the workload image (busybox/alpine/distroless)
# ships no usable `mount` — requiring one would be an unenforceable contract on every
# flox-annotated image. So we bring our OWN `mount` from the nix store. It MUST be
# STATICALLY linked (FLOX_NRI_MOUNT_BIN, set by the hook wrapper): a dynamic store
# binary resolves its interpreter + libs at absolute /nix/store paths, and /nix/store
# does not exist in the container yet — THIS hook is what overlays it (chicken-and-egg).
# A static binary is self-contained, so a plain copy into a scratch dir execs cleanly.
install -Dm0755 "$FLOX_NRI_MOUNT_BIN" "${rootfs}/.flox-nri/sbin/mount"

container_lower="/.overlays.d/${overlay_name}/lower"
container_upper="${container_rw}/upper"
container_work="${container_rw}/work"

chroot "$rootfs" /.flox-nri/sbin/mount -t overlay overlay \
    -o "lowerdir=${container_lower},upperdir=${container_upper},workdir=${container_work}" \
    "${target_rel}"

# A daemonless container must not expose the host's Nix daemon socket. Overlaying
# /nix brings the host socket with it; a stale socket makes nix's `auto` store pick
# daemon mode and hit a dead socket ("cannot connect … Connection refused"). Drop it
# (whiteout in the writable upper) so nix stays on the local store — the same
# socket-absent condition under which it already resolves locally. No-op for any
# overlay that doesn't carry one.
rm -f "${rootfs}/nix/var/nix/daemon-socket/socket" 2>/dev/null || true

# The overlay is now held by the kernel via its backing dentries; the injected
# binary has done its job — drop it so the container rootfs stays clean.
rm -rf "${rootfs}/.flox-nri"

exit 0
