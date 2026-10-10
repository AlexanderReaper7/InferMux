# InferMux as a system service: llama-swap's router with the warden in front.
# `settings` is llama-swap's config.yaml as an attribute set: what Nix owns, such
# as the runtimes' store paths as macros. `configDir` is a directory outside the
# store with `models/` and `warden.yaml`, which the web UI edits and the daemon
# reloads without a rebuild (0005). Without it, `warden` is the warden's file.
# The model servers run as children of this unit, so they share its sandbox and
# its cgroup, which is what makes their GPU work ours.
#
# With `signedConfig.trustedKeys` set, the one config (0022) decides the
# models, their places and the other hosts instead: the daemon keeps it under
# its state directory, takes it from the other hosts, and derives its own
# part into /var/lib/infermux/derived. `configDir`'s `models/` is then unused.
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.infermux;
  yaml = pkgs.formats.yaml { };
  baseConfig = yaml.generate "infermux.yaml" cfg.settings;
  modelsDir = "${cfg.configDir}/models";
  signed = cfg.signedConfig.trustedKeys != [ ];
  trustedKeys = pkgs.writeText "infermux-trusted-keys" (lib.concatLines cfg.signedConfig.trustedKeys);
  cpuSocket = "/run/infermux-cpu/socket";
  # The model servers' sandbox, the daemon's and the CPU runner's alike: the
  # runner's children are model servers too.
  sandbox = {
    DynamicUser = true;
    StateDirectory = "infermux";
    CacheDirectory = [
      "infermux"
      "infermux/home"
    ];
    WorkingDirectory = "/var/lib/infermux";
    # A DynamicUser has no HOME, and the model processes inherit the
    # daemon's environment, so anything that writes ~/.cache or a dotfile
    # (huggingface_hub, gunicorn, Triton) would write under /, read-only
    # here. One shared home in the cache directory, which survives
    # restarts.
    Environment = [ "HOME=/var/cache/infermux/home" ];

    # CUDA and NVML need /dev/nvidia*, which are 0666 on NixOS. A private
    # /dev would hide them.
    PrivateDevices = false;
    # The probe reads other users' /proc/<pid>/cmdline and cgroup to tell
    # our processes from everyone else's. ProtectProc=invisible, which the
    # nixpkgs llama-cpp and llama-swap units set, would blind it, and so
    # would ProcSubset=pid. PrivateUsers is left off for the same reason.
    ProtectSystem = "strict";
    # tmpfs rather than true, so the config directory and the seed can
    # be bound in when they live under /home.
    ProtectHome = if cfg.configDir != null || cfg.signedConfig.seed != null then "tmpfs" else true;
    BindReadOnlyPaths =
      lib.optional (cfg.configDir != null) cfg.configDir
      ++ lib.optional (cfg.signedConfig.seed != null) cfg.signedConfig.seed;
    PrivateTmp = true;
    NoNewPrivileges = true;
    CapabilityBoundingSet = "";
    ProtectClock = true;
    ProtectHostname = true;
    ProtectKernelLogs = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectControlGroups = true;
    MemoryDenyWriteExecute = true;
    RestrictAddressFamilies = [
      "AF_INET"
      "AF_INET6"
      "AF_UNIX"
    ];
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictSUIDSGID = true;
    RemoveIPC = true;
    LockPersonality = true;
    SystemCallArchitectures = "native";
    SystemCallFilter = [
      "@system-service"
      "~@privileged"
    ];
  };
  # The signing tool alone on PATH, not the daemon's binaries.
  configTool = pkgs.runCommand "infermux-config" { meta.mainProgram = "infermux-config"; } ''
    mkdir -p $out/bin
    ln -s ${lib.getExe' cfg.package "infermux-config"} $out/bin/infermux-config
  '';
  wardenFile =
    if cfg.configDir != null then
      "${cfg.configDir}/warden.yaml"
    else
      yaml.generate "warden.yaml" cfg.warden;
in
{
  options.services.infermux = {
    enable = lib.mkEnableOption "InferMux, the model router that yields the GPU to other work";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "infermux.packages.\${system}.default";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:5001";
      description = "Where clients reach the models and /warden/*.";
    };

    settings = lib.mkOption {
      inherit (yaml) type;
      default = { };
      description = ''
        llama-swap's config.yaml, as an attribute set: models, groups, ttl.
        See llama-swap's config.example.yaml in the repository.
      '';
    };

    configDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/home/alice/nixcfg/infermux";
      description = ''
        A directory outside the Nix store holding `models/`, one llama-swap
        YAML file per model, and `warden.yaml`. The daemon reads it through a
        read-only bind mount and reloads on change; the web UI writes it. A
        string, not a path, so Nix never copies it into the store.
      '';
    };

    credentials = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = {
        remote-zbox = "/run/secrets/infermux/remote-zbox";
      };
      description = ''
        Secrets the daemon reads, such as its key for another host, by name:
        the file is at /run/credentials/infermux.service/<name>, which a
        remote's key_file in warden.yaml points at. Passed with LoadCredential,
        so the files may be root's.
      '';
    };

    signedConfig = {
      trustedKeys = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [ "q6NnXhYc3E5gq0n2m8l0Z8m0g6Yy3eJmWm4mVq3Zr2Q= the user's config key" ];
        description = ''
          The public keys a config must be signed with, as `infermux-config
          keygen` prints them, each optionally followed by a comment. Set, the
          one config runs this host (0022): `warden.yaml` must then name
          `host` and `remote_key_file`, and may not have `remotes` or
          `failover_file`. Kept in Nix, never in the config, since a config
          that names its own trusted key proves nothing.
        '';
      };
      seed = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "/home/alice/nixcfg/infermux/config";
        description = ''
          A signed config directory to start from, nixcfg's copy: used when
          it is newer than the one the daemon keeps, and ignored when older.
          Bound in read-only. A string, not a path, so Nix never copies it
          into the store.
        '';
      };
    };

    codexPrompt = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = lib.literalExpression ''"''${pkgs.codex.src}/codex-rs/models-manager/prompt.md"'';
      description = ''
        The prompt.md of the Codex that uses this host. With it, Codex's own
        request for `/v1/models` gets the models in its catalog format, with
        their context windows and reasoning efforts; Codex refuses a catalog
        entry without these base instructions. Decision 0007.
      '';
    };

    ui = {
      enable = lib.mkEnableOption "infermux-ui, the web UI, as a user service of `ui.user`";
      user = lib.mkOption {
        type = lib.types.str;
        description = "The user whose session runs the UI, and who owns `configDir`.";
      };
      listen = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1:5010";
      };
      swapListen = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = "127.0.0.1:5011";
        description = ''
          Where infermux-ui serves llama-swap's own UI with its daemon key, so
          the llama-swap tab asks for none. null: the tab frames the daemon,
          which asks for a key.
        '';
      };
      ggufDirs = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "Directories the UI offers GGUF files from.";
      };
      kvKernels = lib.mkOption {
        type = lib.types.attrsOf (lib.types.listOf lib.types.str);
        default = { };
        example = {
          llama-server = [
            "q4_0-q4_0"
            "q8_0-q8_0"
            "f16-f16"
            "bf16-bf16"
          ];
        };
        description = ''
          Per runtime macro, the K-V cache pairs its FlashAttention kernels
          were compiled for. The UI warns about a model whose pair is missing,
          which llama.cpp runs by converting the cache to f16 on every decode
          step. A runtime not listed is not checked.
        '';
      };
      keySecrets = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "/home/alice/nixcfg/secrets/infermux.yaml";
        description = ''
          The sops file the Keys tab puts a new key's plaintext in, so it can
          be shown again: the hosts' file, beside the secrets their services
          read (InferMux 0006, 4). sops finds its recipients in the .sops.yaml
          above it. null: no new keys.
        '';
      };
      ageIdentity = lib.mkOption {
        type = lib.types.str;
        default = "/home/${cfg.ui.user}/.config/sops/age/keys.txt";
        defaultText = lib.literalExpression ''"/home/''${cfg.ui.user}/.config/sops/age/keys.txt"'';
        description = ''
          The user's age identity, encrypted with a passphrase (`age -p`). The
          Keys tab asks for the passphrase and unlocks it in memory for each
          call to sops that has to decrypt.
        '';
      };
      daemonKeyFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = ''
          A file, readable by `ui.user`, with the key the UI sends the daemon.
          Needed once warden.yaml has a keys_file.
        '';
      };
      commitPassphrase = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Commit asks for the passphrase of the user's GPG key and signs in
          gpg's loopback mode. For a host with no desktop session to show a
          pinentry, where a signed commit would otherwise fail. Decision 0008.
        '';
      };
      hfDir = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "/srv/models/hf";
        description = ''
          Where a model's files named on Hugging Face (`metadata.hf`) are
          downloaded to, as org/repo/file.gguf, writable by `ui.user`. The
          daemon must be able to read it. null: a model cannot name one.
          Decision 0012.
        '';
      };
      hfTokenFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = ''
          A file, readable by `ui.user`, with a Hugging Face token for gated
          and private repos. Sent to huggingface.co only, not to its CDN.
        '';
      };

      prebuild = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = ''/home/alice/nixcfg#nixosConfigurations.host.config.systemd.units."infermux.service".unit'';
        description = ''
          A flake installable the UI's "Build now" button builds as the user,
          ahead of a switch: what the daemon's runtimes come from. null hides
          the button. Nothing is switched.
        '';
      };
    };

    warden = lib.mkOption {
      inherit (yaml) type;
      default = { };
      example = lib.literalExpression ''
        {
          comfyui_url = "http://127.0.0.1:8188";
          comfyui_unit = "comfyui.service";
          desktop_processes = [ "cosmic-comp" "electron" ];
          keys_file = "keys.yaml";
          consumers = [ { name = "episteme"; url = "http://127.0.0.1:8200"; } ];
        }
      '';
      description = ''
        The warden's file, as an attribute set, used when `configDir` is
        null. Every key is optional; see warden.example.yaml in the repository
        for what each one does.
      '';
    };

    adapters = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            listen = lib.mkOption {
              type = lib.types.str;
              example = "172.30.0.1:3003";
              description = "Where the client reaches the adapter.";
            };
            target = lib.mkOption {
              type = lib.types.str;
              example = "http://127.0.0.1:5001/upstream/immich-ml";
              description = "Where its requests go; a request's path is appended.";
            };
            keyFile = lib.mkOption {
              type = lib.types.str;
              example = "/run/secrets/immich";
              description = "The client's InferMux key. Passed with LoadCredential, so it may be root's.";
            };
            routes = lib.mkOption {
              type = lib.types.attrsOf lib.types.str;
              default = { };
              example = {
                "/ping" = "/health";
              };
              description = "A request for a path here goes to the path it maps to on the target's host instead.";
            };
          };
        }
      );
      default = { };
      description = ''
        For a client that cannot send a key, such as Immich's ML URL: an
        `infermux-adapter-<name>` service that forwards to `target` with the
        client's key. Decision 0013.
      '';
    };
  };

  config = lib.mkMerge [
    (lib.mkIf cfg.enable {
      assertions = [
        {
          assertion = cfg.ui.enable -> cfg.configDir != null;
          message = "services.infermux.ui needs services.infermux.configDir: the UI edits those files.";
        }
        {
          assertion = cfg.signedConfig.seed != null -> signed;
          message = "services.infermux.signedConfig.seed needs signedConfig.trustedKeys to check it against.";
        }
      ];

      environment.systemPackages = lib.optional signed configTool;

      # Browsers may read only InferMux's own origin. Writes from other origins
      # are refused by the warden whatever this says (0005).
      services.infermux.settings.security.cors.allowedOrigins = lib.mkDefault [ "http://${cfg.listen}" ];

      systemd.services.infermux = {
        description = "InferMux: model router that yields the GPU to other work";
        wantedBy = [ "multi-user.target" ];
        after = [ "network.target" ];
        environment.LLAMA_CACHE = "/var/cache/infermux";
        serviceConfig = {
          Type = "exec";
          ExecStart = lib.escapeShellArgs (
            [
              (lib.getExe cfg.package)
              "-listen"
              cfg.listen
              "-config"
              baseConfig
              "-warden-config"
              wardenFile
            ]
            ++ lib.optionals (cfg.configDir != null && !signed) [
              "-config-dir"
              modelsDir
            ]
            ++ lib.optionals signed [
              "-config-trusted-keys"
              trustedKeys
              # Written by the daemon from its part of the signed config.
              "-config-dir"
              "/var/lib/infermux/derived"
              "-cpu-socket"
              cpuSocket
            ]
            ++ lib.optionals (cfg.signedConfig.seed != null) [
              "-config-seed"
              cfg.signedConfig.seed
            ]
            ++ lib.optionals (cfg.codexPrompt != null) [
              "-codex-prompt"
              cfg.codexPrompt
            ]
          );
          Restart = "always";
          RestartSec = 5;
          LoadCredential = lib.mapAttrsToList (name: path: "${name}:${path}") cfg.credentials;
        }
        // sandbox;
      }
      // lib.optionalAttrs signed {
        wants = [ "infermux-cpu.service" ];
        after = [
          "network.target"
          "infermux-cpu.service"
        ];
      };

      # A slice at the root of the cgroup tree, so idle means idle against
      # everything: a dash in the name would nest it, and a nested idle slice
      # took 23% of a core against user.slice's 76% (0022).
      systemd.slices.idle = lib.mkIf signed {
        description = "Work that runs only when nothing else wants the CPU";
        sliceConfig.CPUWeight = "idle";
      };

      # The CPU device's model servers run as this runner's children, in the
      # idle slice; the daemon's own children would sit in its cgroup
      # (internal/cpurun). The same dynamic user as the daemon, which is the
      # only one its socket lets in.
      systemd.services.infermux-cpu = lib.mkIf signed {
        description = "InferMux CPU runner: the CPU device's model servers, in the idle slice";
        wantedBy = [ "multi-user.target" ];
        serviceConfig = sandbox // {
          Type = "exec";
          User = "infermux";
          Slice = "idle.slice";
          ExecStart = lib.escapeShellArgs [
            (lib.getExe' cfg.package "infermux-cpu")
            "serve"
            "-socket"
            cpuSocket
          ];
          RuntimeDirectory = "infermux-cpu";
          RuntimeDirectoryMode = "0700";
          Restart = "always";
          RestartSec = 5;
        };
      };

      # As the user, not a system service: it edits and commits files in the
      # user's repository with the user's git identity.
      systemd.user.services.infermux-ui = lib.mkIf cfg.ui.enable {
        description = "InferMux web UI";
        wantedBy = [ "default.target" ];
        unitConfig.ConditionUser = cfg.ui.user;
        path = [
          pkgs.git
          config.nix.package
        ];
        serviceConfig = {
          ExecStart = lib.escapeShellArgs (
            [
              "${cfg.package}/bin/infermux-ui"
              "-listen"
              cfg.ui.listen
              "-daemon"
              "http://${cfg.listen}"
              "-models-dir"
              modelsDir
              "-warden-config"
              wardenFile
              "-base-config"
              baseConfig
              "-gguf-dirs"
              (lib.concatStringsSep "," cfg.ui.ggufDirs)
              "-kv-kernels"
              (pkgs.writeText "kv-kernels.json" (builtins.toJSON cfg.ui.kvKernels))
            ]
            ++ lib.optionals (cfg.ui.swapListen != null) [
              "-swap-listen"
              cfg.ui.swapListen
            ]
            ++ lib.optionals (cfg.ui.prebuild != null) [
              "-prebuild"
              cfg.ui.prebuild
            ]
            ++ lib.optionals (cfg.ui.keySecrets != null) [
              "-key-secrets"
              cfg.ui.keySecrets
              "-sops"
              (lib.getExe pkgs.sops)
              "-age-identity"
              cfg.ui.ageIdentity
            ]
            ++ lib.optionals (cfg.ui.daemonKeyFile != null) [
              "-daemon-key-file"
              cfg.ui.daemonKeyFile
            ]
            ++ lib.optionals (cfg.ui.hfDir != null) [
              "-hf-dir"
              cfg.ui.hfDir
            ]
            ++ lib.optionals (cfg.ui.hfTokenFile != null) [
              "-hf-token-file"
              cfg.ui.hfTokenFile
            ]
            ++ lib.optionals cfg.ui.commitPassphrase [
              "-commit-passphrase"
              "-gpg"
              (lib.getExe' pkgs.gnupg "gpg")
            ]
          );
          Restart = "always";
          RestartSec = 5;
        };
      };
    })

    {
      systemd.services = lib.mapAttrs' (
        name: a:
        lib.nameValuePair "infermux-adapter-${name}" {
          description = "InferMux adapter for ${name}: its requests, with its key";
          wantedBy = [ "multi-user.target" ];
          after = [
            "network.target"
            "infermux.service"
          ];
          serviceConfig = {
            ExecStart = lib.escapeShellArgs (
              [
                (lib.getExe' cfg.package "infermux-adapter")
                "-listen"
                a.listen
                "-target"
                a.target
                "-key-file"
                "%d/key"
              ]
              ++ lib.concatMap (from: [
                "-route"
                "${from}=${a.routes.${from}}"
              ]) (lib.attrNames a.routes)
            );
            LoadCredential = "key:${a.keyFile}";
            # Until the client's network exists, the address cannot be bound.
            Restart = "always";
            RestartSec = 5;
            DynamicUser = true;
            CapabilityBoundingSet = "";
            NoNewPrivileges = true;
            PrivateDevices = true;
            PrivateTmp = true;
            ProtectHome = true;
            ProtectSystem = "strict";
            ProtectClock = true;
            ProtectHostname = true;
            ProtectKernelLogs = true;
            ProtectKernelModules = true;
            ProtectKernelTunables = true;
            ProtectControlGroups = true;
            ProtectProc = "invisible";
            ProcSubset = "pid";
            MemoryDenyWriteExecute = true;
            LockPersonality = true;
            RestrictAddressFamilies = [
              "AF_INET"
              "AF_INET6"
            ];
            RestrictNamespaces = true;
            RestrictRealtime = true;
            RestrictSUIDSGID = true;
            SystemCallArchitectures = "native";
            SystemCallFilter = [
              "@system-service"
              "~@privileged"
            ];
          };
        }
      ) cfg.adapters;
    }
  ];
}
