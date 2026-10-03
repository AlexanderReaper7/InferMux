# InferMux as a system service: llama-swap's router with the warden in front.
# `settings` is llama-swap's config.yaml and `warden` is the warden's file, both
# as attribute sets. The model servers run as children of this unit, so they
# share its sandbox and its cgroup, which is what makes their GPU work ours.
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
        The warden's file, as an attribute set. Every key is optional; see
        warden.example.yaml in the repository for what each one does.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.infermux = {
      description = "InferMux: model router that yields the GPU to other work";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      environment.LLAMA_CACHE = "/var/cache/infermux";
      serviceConfig = {
        Type = "exec";
        ExecStart = lib.escapeShellArgs [
          (lib.getExe cfg.package)
          "-listen"
          cfg.listen
          "-config"
          (yaml.generate "infermux.yaml" cfg.settings)
          "-warden-config"
          (yaml.generate "warden.yaml" cfg.warden)
        ];
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
        ProtectHome = true;
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
  };
}
