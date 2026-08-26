{
  description = "Flox containerd NRI plugin (fork) — injects flox (nix-built) environments into Kubernetes pods via a store-resolved overlay";

  inputs = {
    # Externals follow the seedmatic aggregator so the whole closure (this flake
    # + rke2lab which inputs it) resolves to ONE nixpkgs — no skew.
    flake-commons.url = "github:seedmatic/nix-flake-commons/develop";
    nixpkgs.follows = "flake-commons/nixpkgs";
    flake-utils.follows = "flake-commons/flake-utils";

    # This flake only consumes the aggregator's nixpkgs + flake-utils. Its other
    # ~26 inputs (a full darwin/home-manager/browser fleet) would otherwise drag
    # their whole closures into our lock (~8.5 MB). We never evaluate flake-commons'
    # outputs, so redirect every unused input to `nixpkgs` — this collapses their
    # subtrees out of the lock. Keep this list in sync if the aggregator grows.
    flake-commons.inputs.bird.follows = "nixpkgs";
    flake-commons.inputs.cachix.follows = "nixpkgs";
    flake-commons.inputs.chromium-bin.follows = "nixpkgs";
    flake-commons.inputs.darwin.follows = "nixpkgs";
    flake-commons.inputs.determinate.follows = "nixpkgs";
    flake-commons.inputs.devenv.follows = "nixpkgs";
    flake-commons.inputs.disko.follows = "nixpkgs";
    flake-commons.inputs.extra-container.follows = "nixpkgs";
    flake-commons.inputs.flake-compat.follows = "nixpkgs";
    flake-commons.inputs.flox.follows = "nixpkgs";
    flake-commons.inputs.home-manager.follows = "nixpkgs";
    flake-commons.inputs.impermanence.follows = "nixpkgs";
    flake-commons.inputs.incus-compose.follows = "nixpkgs";
    flake-commons.inputs.lix-module.follows = "nixpkgs";
    flake-commons.inputs.maven-mvnd.follows = "nixpkgs";
    flake-commons.inputs.nix.follows = "nixpkgs";
    flake-commons.inputs.nix-snapshotter.follows = "nixpkgs";
    flake-commons.inputs.nixos-generators.follows = "nixpkgs";
    flake-commons.inputs.nixos-hardware.follows = "nixpkgs";
    flake-commons.inputs.nixpkgs-unstable.follows = "nixpkgs";
    flake-commons.inputs.nvfetcher.follows = "nixpkgs";
    flake-commons.inputs.ripvcs.follows = "nixpkgs";
    flake-commons.inputs.socket-vmnet.follows = "nixpkgs";
    flake-commons.inputs.sops-nix.follows = "nixpkgs";
    flake-commons.inputs.treefmt-nix.follows = "nixpkgs";
    flake-commons.inputs.zen-browser.follows = "nixpkgs";
  };

  outputs = {
    self,
    nixpkgs,
    flake-utils,
    ...
  }:
    flake-utils.lib.eachSystem [
      "aarch64-darwin"
      "aarch64-linux"
    ] (system: let
      pkgs = import nixpkgs {inherit system;};
      lib = pkgs.lib;

      # Single source of the plugin version — bumped on release + matched by a
      # `vX.Y.Z` git tag. Stamped into the binary via ldflags (-X main.pluginVersion).
      version = lib.fileContents ./VERSION;

      # `doCheck = false`: lab-only build; upstream tests add build time without
      # catching anything the rke2lab use case cares about.
      common = {
        pname = "flox-nri-plugin";
        src = builtins.path {
          path = ./nri-plugin;
          name = "nri-plugin-src";
        };
        subPackages = ["cmd/flox-nri-plugin"];
        vendorHash = "sha256-f2tBtvXqS4XfkKsNQJnwxobVNRFCwEusKC4Et7rySEk=";
        env.CGO_ENABLED = "0";
        tags = ["netgo"];
        doCheck = false;
        meta = with lib; {
          description = "Flox NRI plugin for containerd - injects flox environments into containers";
          license = licenses.mit;
          platforms = platforms.unix;
        };
      };

      flox-nri-plugin = pkgs.buildGoModule (common
        // {
          inherit version;
          ldflags = [
            "-s"
            "-w"
            "-X main.pluginVersion=${version}"
          ];
        });

      flox-nri-plugin-debug = pkgs.buildGoModule (common
        // {
          pname = "flox-nri-plugin-debug";
          inherit version;
          dontStrip = true;
          buildFlagsArray = ["-gcflags=all=-N -l"];
          ldflags = ["-X main.pluginVersion=${version}-debug"];
        });

      # The OCI hooks the plugin references at fixed /usr/local/sbin paths. Owned
      # here with the binary (the shim = binary + hooks + their contract); rke2lab
      # only bakes this package onto the node via tmpfiles. Linux-only (util-linux
      # is Linux-only, and the hooks run inside Linux containers): runc executes
      # them with a minimal PATH, so the shebang is pinned to the store bash
      # (patchShebangs) and the hooks' commands are wrapped onto PATH; the overlay
      # hook also carries a STATIC `mount` (FLOX_NRI_MOUNT_BIN) for its pre-pivot
      # chroot, since the target container image ships none.
      flox-nri-hooks = pkgs.runCommandLocal "flox-nri-hooks" {
        nativeBuildInputs = [pkgs.makeWrapper];
      } ''
        mkdir -p $out/sbin
        install -m0755 ${./hooks/flox-nri-overlay-hook.sh}  $out/sbin/flox-nri-overlay-hook.sh
        install -m0755 ${./hooks/flox-nri-env-link-hook.sh} $out/sbin/flox-nri-env-link-hook.sh
        install -m0755 ${./hooks/flox-nri-chown-hook.sh}    $out/sbin/flox-nri-chown-hook.sh
        patchShebangs $out/sbin
        for f in $out/sbin/*.sh; do
          wrapProgram "$f" \
            --prefix PATH : ${lib.makeBinPath [pkgs.bash pkgs.coreutils pkgs.util-linux pkgs.gnused]} \
            --set FLOX_NRI_MOUNT_BIN ${pkgs.pkgsStatic.util-linux}/bin/mount
        done
      '';
    in {
      packages =
        {
          inherit flox-nri-plugin flox-nri-plugin-debug;
          default = flox-nri-plugin;
        }
        # flox-nri-hooks is Linux-only (util-linux); don't expose it on darwin.
        // lib.optionalAttrs pkgs.stdenv.isLinux {inherit flox-nri-hooks;};

      formatter = pkgs.alejandra;
    });
}
