# godbus-monitor.nix -- Home Manager module.
#
# Imported from `home-manager.users.<name>.imports`, NOT from the NixOS-level
# imports. Uses HM's systemd.user.services directly, so no NixOS-level wiring
# is needed at all.
#
# After changing dependencies: set vendorHash to lib.fakeHash, rebuild, and
# paste the "got:" hash from the error.

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
    # Whether ./. is this repo or a fetched store path, copy only what the build
    # needs: a bare ./. would pull local binaries and .direnv into the store.
    src = lib.fileset.toSource {
      root = ./.;
      fileset = lib.fileset.unions [
        ./go.mod
        ./go.sum
        ./triggers_example.json # loaded by the tests
        (lib.fileset.fileFilter (file: file.hasExt "go") ./.)
      ];
    };
    vendorHash = "sha256-y+3dL8WEiN4PDHLX9/iNijS20wUheiJ+mZrM7af69G0=";
    # The integration tests spawn a private dbus-daemon, and skip without one.
    nativeCheckInputs = [ pkgs.dbus ];
  };

  # The Go binary reads this JSON array verbatim -- the module is a thin
  # pass-through of the daemon's config format (see Trigger in config.go).
  # Validating with the daemon itself keeps the rules in one place: a bad
  # trigger fails the build rather than the service.
  configFile = pkgs.writeTextFile {
    name = "godbus-monitor.json";
    text = builtins.toJSON cfg.triggers;
    checkPhase = ''${triggerPkg}/bin/godbus-monitor -validate -config "$target"'';
  };

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
        description = "Which bus to watch the property or signal on.";
      };
      sender = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Restrict to signals from this bus name (e.g. org.freedesktop.login1). Well-known names are resolved to the current owner. Empty = any sender, which is rejected on the system bus because any local user can emit signals there.";
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
        default = "";
        example = "LockedHint";
        description = "Property to watch via org.freedesktop.DBus.Properties.PropertiesChanged on the configured interface. Mutually exclusive with signal; exactly one must be nonempty.";
      };
      signal = lib.mkOption {
        type = lib.types.str;
        default = "";
        example = "ActiveChanged";
        description = "Signal member to watch on the configured interface. Mutually exclusive with property; exactly one must be nonempty.";
      };
      argument = lib.mkOption {
        type = lib.types.ints.unsigned;
        default = 0;
        description = "Zero-based signal argument index whose value is compared with expected_value. Only used for signal triggers.";
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
        description = "Command to run, passed straight to exec (no shell unless you add one). For shell commands use [ \"sh\" \"-c\" \"...\" ] -- a `sh = cmd: [ \"sh\" \"-c\" cmd ]` helper in your config keeps this to one line. Runs with a minimal environment (see safeEnv in executor.go).";
      };
      only_on_change = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Only run when the selected value (property value or signal argument) differs from its previous observed value. The first observation is treated as a change.";
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
        # knobs don't apply, and mount-namespace ones like ProtectProc are
        # unsupported on user units -- but these still work and are worth
        # having for a daemon whose whole job is running shell commands.
        NoNewPrivileges = true;
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
