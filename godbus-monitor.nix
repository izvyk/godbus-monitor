# godbus-monitor.nix -- Home Manager module.
#
# Imported from `home-manager.users.<name>.imports`, NOT from the NixOS-level
# imports. Uses HM's systemd.user.services directly, so no NixOS-level wiring
# is needed at all.
#
# Nix invocation to get the real vendorHash (run on your NixOS machine):
#   nix build --no-link --print-out-paths \
#     --expr 'let pkgs = import <nixpkgs> {}; in pkgs.buildGoModule { pname = "x"; version = "0"; src = <godbusMonitorSrc>; vendorHash = pkgs.lib.fakeHash; }' 2>&1 | grep "got:"

{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.godbus-monitor;

  triggerPkg = pkgs.buildGoModule {
    pname = "godbus-monitor";
    version = "0.1.0";
    # When this file lives in the godbus-monitor repo itself, ./. is the source.
    # When you import it from your config via "${godbusMonitorSrc}/godbus-monitor.nix",
    # ./. is the fetched store path of the repo -- same thing, right result.
    src = ./.;
    vendorHash = "sha256-WUTGAYigUjuZLHO1YpVhFSWpvULDZfGMfOXZQqVYAfs=";
  };

  # The Go binary reads this JSON array verbatim -- the module is a thin
  # pass-through of the daemon's config format (see Trigger in config.go).
  configFile = pkgs.writeText "godbus-monitor.json" (builtins.toJSON cfg.triggers);

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
        description = "Restrict to signals from this bus name (e.g. org.freedesktop.login1). Well-known names are resolved to the current owner. Empty = any sender.";
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
      argv = lib.mkOption {
        type = lib.types.nonEmptyListOf lib.types.str;
        example = [
          "sh"
          "-c"
          "notify-send hello"
        ];
        description = "Command to run, passed straight to exec (no shell unless you add one). For shell commands use [ \"sh\" \"-c\" \"...\" ] -- a `sh = cmd: [ \"sh\" \"-c\" cmd ]` helper in your config keeps this to one line. Runs with a minimal environment (PATH, HOME, USER, XDG_RUNTIME_DIR, DBUS_SESSION_BUS_ADDRESS, WAYLAND_DISPLAY, DISPLAY).";
      };
      only_on_change = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Only run when this property's value differs from its previous observed value. The first observation is treated as a change.";
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
  options.services.godbus-monitor = {
    enable = lib.mkEnableOption "Generic D-Bus Trigger Daemon";

    debug = lib.mkEnableOption "verbose debug logging";

    triggers = lib.mkOption {
      type = lib.types.listOf triggerModule;
      default = [ ];
      description = ''
        List of D-Bus triggers. All attribute sets across your Home Manager
        configuration merge into one list -- no manual combining needed.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    # assertions = [
    #   {
    #     assertion = triggerPkg.vendorHash != "sha256-YOUR_VENDOR_HASH_HERE";
    #     message = "services.godbus-monitor: still has the placeholder vendorHash. Build once with lib.fakeHash and paste the 'got:' hash here.";
    #   }
    # ];

    systemd.user.services.godbus-monitor = {
      Unit = {
        Description = "Generic D-Bus Trigger";
        PartOf = [ "graphical-session.target" ];
        After = [ "graphical-session.target" ];
      };

      Service = {
        ExecStart = "${triggerPkg}/bin/godbus-monitor -config ${configFile}" + lib.optionalString cfg.debug " -debug";
        Restart = "on-failure";
        RestartSec = "5s";

        # Process-level hardening. Runs as your user, so DynamicUser-style
        # knobs don't apply -- but these still work on user units and are
        # worth having for a daemon whose whole job is running shell commands.
        NoNewPrivileges = true;
        ProtectProc = "invisible";
        RestrictSUIDSGID = true;
        LockPersonality = true;
        RestrictRealtime = true;
        SystemCallFilter = [ "@system-service" ];
        SystemCallErrorNumber = "EPERM";
        # If a trigger script dies with EPERM in the journal, some interpreter
        # or tool it calls needs a syscall outside @system-service. Loosen or
        # drop SystemCallFilter rather than guessing.
      };

      Install = {
        WantedBy = [ "graphical-session.target" ];
      };
    };
  };
}
