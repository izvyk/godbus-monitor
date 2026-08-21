{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.dbus-trigger;

  triggerPkg = pkgs.buildGoModule {
    pname = "godbus-monitor";
    version = "0.1.0";
    src = ./.; # Path to your Go code
    vendorHash = "sha256-YOUR_VENDOR_HASH_HERE"; 
  };

  # Generate the JSON configuration file dynamically
  configFile = pkgs.writeText "godbus-monitor.json" (builtins.toJSON cfg.triggers);

in
{
  options.services.dbus-trigger = {
    enable = lib.mkEnableOption "Generic D-Bus Trigger Daemon";

    triggers = lib.mkOption {
      type = lib.types.listOf lib.types.attrs;
      default = [];
      description = ''
        List of D-Bus triggers.
        Example:
        [
          {
            bus = "session";
            interface = "org.gnome.Mutter.DisplayConfig";
            property = "PowerSaveMode";
            operator = ">";
            expected_value = "0";
            script = "systemctl suspend";
          }
        ]
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.user.services.dbus-trigger = {
      Unit = {
        Description = "Generic D-Bus Trigger";
        PartOf = [ "graphical-session.target" ];
        After = [ "graphical-session.target" ];
      };

      Service = {
        ExecStart = "${triggerPkg}/bin/godbus-monitor -config ${configFile}";
        Restart = "on-failure";
        RestartSec = "5s";
      };

      Install = {
        WantedBy = [ "graphical-session.target" ];
      };
    };
  };
}
