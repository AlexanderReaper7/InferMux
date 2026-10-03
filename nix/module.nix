# InferMux as a system service: llama-swap's router with the warden in front.
# `settings` is llama-swap's config.yaml as an attribute set: what Nix owns, such
# as the runtimes' store paths as macros. `configDir` is a directory outside the
# store with `models/` and `warden.yaml`, which the web UI edits and the daemon
# reloads without a rebuild (0005). Without it, `warden` is the warden's file.
# The model servers run as children of this unit, so they share its sandbox and
# its cgroup, which is what makes their GPU work ours.
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
      ggufDirs = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "Directories the UI offers GGUF files from.";
      };
    };

    warden = lib.mkOption {
      inherit (yaml) type;
      default = { };
      example = lib.literalExpression ''
        {
          comfyui_url = "http://127.0.0.1:8188";
          desktop_processes = [ "cosmic-comp" "electron" ];
          batch_api_keys = [ "episteme-batch" ];
          consumers = [ { name = "episteme"; url = "http://127.0.0.1:8200"; } ];
        }
      '';
      description = ''
        The warden's file, as an attribute set, used when `configDir` is
        null. Every key is optional; see warden.example.yaml in the repository
        for what each one does.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.ui.enable -> cfg.configDir != null;
        message = "services.infermux.ui needs services.infermux.configDir: the UI edits those files.";
      }
    ];

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
          ++ lib.optionals (cfg.configDir != null) [
            "-config-dir"
            modelsDir
          ]
        );
        Restart = "always";
        RestartSec = 5;

        DynamicUser = true;
        StateDirectory = "infermux";
        CacheDirectory = "infermux";
        WorkingDirectory = "/var/lib/infermux";

        # CUDA and NVML need /dev/nvidia*, which are 0666 on NixOS. A private
        # /dev would hide them.
        PrivateDevices = false;
        # The probe reads other users' /proc/<pid>/cmdline and cgroup to tell
        # our processes from everyone else's. ProtectProc=invisible, which the
        # nixpkgs llama-cpp and llama-swap units set, would blind it, and so
        # would ProcSubset=pid. PrivateUsers is left off for the same reason.
        ProtectSystem = "strict";
        # tmpfs rather than true, so the config directory can be bound in
        # when it lives under /home.
        ProtectHome = if cfg.configDir != null then "tmpfs" else true;
        BindReadOnlyPaths = lib.optional (cfg.configDir != null) cfg.configDir;
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
    };

    # As the user, not a system service: it edits and commits files in the
    # user's repository with the user's git identity.
    systemd.user.services.infermux-ui = lib.mkIf cfg.ui.enable {
      description = "InferMux web UI";
      wantedBy = [ "default.target" ];
      unitConfig.ConditionUser = cfg.ui.user;
      path = [ pkgs.git ];
      serviceConfig = {
        ExecStart = lib.escapeShellArgs [
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
        ];
        Restart = "always";
        RestartSec = 5;
      };
    };
  };
}
