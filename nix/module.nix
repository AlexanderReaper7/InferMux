# The warden as a system service. `settings` is warden.toml as a Nix attribute
# set; the module writes it to the store and points WARDEN_CONFIG at it.
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.llama-warden;
  toml = pkgs.formats.toml { };
in
{
  options.services.llama-warden = {
    enable = lib.mkEnableOption "llama-warden, the GPU yielder in front of llama.cpp";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "llama-warden.packages.\${system}.default";
    };

    settings = lib.mkOption {
      inherit (toml) type;
      default = { };
      example = lib.literalExpression ''
        {
          agent.comfyui_url = "http://127.0.0.1:8188";
          policy.min_free_vram_mb = 3000;
          consumers = [ { name = "episteme"; url = "http://127.0.0.1:8200"; } ];
        }
      '';
      description = ''
        warden.toml, as an attribute set. Every key is optional; see the
        repository's warden.toml for what each one does.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.llama-warden = {
      description = "llama-warden: yields the GPU from llama.cpp to other work";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      environment.WARDEN_CONFIG = toml.generate "warden.toml" cfg.settings;
      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        Restart = "always";
        RestartSec = 5;

        DynamicUser = true;
        # NVML needs /dev/nvidiactl and /dev/nvidia0, which are 0666 on NixOS.
        # A private /dev would hide them.
        PrivateDevices = false;
        # The probe reads other users' /proc/<pid>/cmdline and cgroup to tell
        # llama.cpp's processes from everyone else's. ProtectProc=invisible or
        # ProcSubset=pid would blind it, so both stay at their defaults.
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        NoNewPrivileges = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        LockPersonality = true;
        SystemCallArchitectures = "native";
      };
    };
  };
}
