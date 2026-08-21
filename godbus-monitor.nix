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

  # The Go binary reads a JSON array of triggers (see Trigger in config.go).
  # `script` is translated to argv = ["sh" "-c" script] below, matching the
  # description on the script option.
  configFile = pkgs.writeText "godbus-monitor.json" (builtins.toJSON
    (map (t:
      (builtins.removeAttrs t [ "script" ]) // {
        argv = [
          "sh"
          "-c"
          t.script
        ];
      }) cfg.triggers));

  triggerModule = lib.types.submodule {
    options = {
      name = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Label used in logs and for debounce bookkeeping. Auto-generated as trigger-N if left empty.";
      };
      bus = lib.mkOption {
        type = lib.types.enum [
          "system"
          "session"
        ];
        description = "Which bus to watch this property on.";
      };
      sender = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Restrict to signals from this bus name (e.g. org.freedesktop.login1). Empty = any sender.";
      };
      path = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Restrict to signals on this object path. Empty = any path.";
      };
      interface = lib.mkOption {
        type = lib.types.str;
        example = "org.freedesktop.login1.Session";
      };
      property = lib.mkOption {
        type = lib.types.str;
        example = "LockedHint";
      };
      operator = lib.mkOption {
        type = lib.types.enum [
          "=="
          "!="
          ">"
          "<"
        ];
        default = "==";
      };
      expected_value = lib.mkOption {
        type = lib.types.str;
        example = "true";
      };
      script = lib.mkOption {
        type = lib.types.str;
        description = "Shell command run via `sh -c` when the property matches. Runs with a minimal environment (PATH, HOME, USER, XDG_RUNTIME_DIR, DBUS_SESSION_BUS_ADDRESS, WAYLAND_DISPLAY, DISPLAY) -- not the daemon's full environment.";
      };
      debounce_ms = lib.mkOption {
        type = lib.types.ints.positive;
        default = 250;
        description = "Minimum gap between runs of this trigger, to absorb signal storms during state transitions.";
      };
      timeout_sec = lib.mkOption {
        type = lib.types.ints.positive;
        default = 30;
        description = "Kill the script if it's still running after this long.";
      };
    };
  };
in
{
  options.services.dbus-trigger = {
    enable = lib.mkEnableOption "Generic D-Bus Trigger Daemon";

    debug = lib.mkEnableOption "verbose debug logging";

    triggers = lib.mkOption {
      type = lib.types.listOf triggerModule;
      default = [ ];
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
    assertions = [
      {
        assertion = triggerPkg.vendorHash != "sha256-YOUR_VENDOR_HASH_HERE";
        message = "services.dbus-trigger: godbus-monitor.nix still has the placeholder vendorHash -- build once with an empty string to get the real one before enabling this module.";
      }
    ];

    systemd.user.services.dbus-trigger = {
      Unit = {
        Description = "Generic D-Bus Trigger";
        PartOf = [ "graphical-session.target" ];
        After = [ "graphical-session.target" ];
      };

      Service = {
        ExecStart = "${triggerPkg}/bin/godbus-monitor -config ${configFile}" + lib.optionalString cfg.debug " -debug";
        Restart = "on-failure";
        RestartSec = "5s";

        # Process-level hardening. This is a *user* unit tied to
        # graphical-session.target, so the DynamicUser/ProtectSystem=strict
        # knobs used for system-level services don't really apply -- but the
        # sandboxing primitives below are still honored for user units on a
        # reasonably recent systemd and are worth having, since this daemon's
        # whole job is running shell commands in response to external events.
        NoNewPrivileges = true;
        ProtectProc = "invisible";
        RestrictSUIDSGID = true;
        LockPersonality = true;
        RestrictRealtime = true;
        SystemCallFilter = [ "@system-service" ];
        SystemCallErrorNumber = "EPERM";
        # If a trigger script needs something outside @system-service (some
        # interpreters, sandboxing tools, etc.) you'll see it die with EPERM
        # in the journal -- loosen or drop SystemCallFilter rather than
        # guessing at the right syscall set.
      };

      Install = {
        WantedBy = [ "graphical-session.target" ];
      };
    };
  };
}
