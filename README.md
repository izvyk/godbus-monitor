# godbus-monitor

A D-Bus trigger daemon that runs commands when an observed value matches a condition.

## Usage

```sh
go build
./godbus-monitor -validate -config triggers.json  # check a config and exit
./godbus-monitor -config triggers.json            # add -debug to see every evaluation
./godbus-monitor -dry-run -config triggers.json   # watch the real buses, log matches, run nothing
```

`godbus-monitor.nix` is a **Home Manager** module: import it into your user's Home Manager configuration, not the NixOS-level imports. It builds the daemon, validates your triggers at build time, and runs it as a user service.

`triggers_example.json` shows the config format. Review it before using it: its commands really do suspend the machine and change desktop state.

## Trigger configuration

The config is a JSON array of triggers. The Home Manager options have the same names.

| Field | Default | Meaning |
| --- | --- | --- |
| `bus` | required | `system` or `session`. |
| `interface` | required | D-Bus interface to watch. |
| `property` | | Property on `interface`, observed through `PropertiesChanged`. Set exactly one of `property` and `signal`. |
| `signal` | | Signal member on `interface`. |
| `argument` | `0` | Zero-based signal argument to compare. Signal triggers only. |
| `operator` | required (`==` in the module) | `==` or `!=` for any comparable value; `>` or `<` for numbers. |
| `expected_value` | `""` | String to compare with, parsed as the value's type: `"true"`, `"5"`, `"/org/example"`. |
| `argv` | required | Command to run. |
| `name` | `trigger-N` | Unique label for logs and debounce state. |
| `sender` | required on `system`, otherwise any | Only accept signals from this bus name. Well-known names are resolved to their current owner. |
| `path` | any | Only accept signals from this object path. |
| `only_on_change` | `false` | Skip observations equal to the previous one. |
| `debounce_ms` | `250` | Minimum gap between runs. |
| `timeout_sec` | `30` | Stop the command after this long. |

### Behaviour

- **Comparison.** Strings, object paths, booleans and numbers can be compared. Other values, such as arrays, never match. If a trigger never fires, `-debug` logs each value it rejected, with its type. A signal must carry the selected argument, so signals with no arguments cannot fire a trigger.
- **`only_on_change`.** Nonmatching values are tracked too: with expected value `"true"`, `true → false → true` runs twice, while repeated `true` values run once. The first observation counts as a change. There is one tracked value per trigger, so a trigger spanning several objects (no `path`, for example every login1 session) compares across them.
- **Debounce.** At most one process runs per trigger. A burst of events during a run or inside the debounce window coalesces into one follow-up run.
- **Commands.** `argv` is executed directly, without a shell unless you include one (`["sh", "-c", "..."]`). The environment is minimal; see `safeEnv` in `executor.go`.
- **Stopping.** On timeout or shutdown the command's process group gets SIGTERM, then SIGKILL 2 seconds later.
- **Background processes.** A command that leaves a process running should redirect its output (`app >/dev/null 2>&1 &`). Otherwise that output pipe is closed 2 seconds after the command exits, and the process can die from SIGPIPE on its next write.
- **Security.** Any client on a bus can emit any signal, and the default system bus policy allows this for every local user, so system bus triggers must set `sender`: policy there restricts who can own a well-known name. On the session bus any client can claim a free name, so `sender` only guards against mistakes.

## GNOME lock and screen events

These are different events on different buses. Times are from one manual lock on GNOME 50, measured with this daemon on the real buses.

| Moment | After lock | Bus | Interface | Trigger on |
| --- | --- | --- | --- | --- |
| Shield starts falling | 0 s | system | `org.freedesktop.login1.Session` | property `LockedHint` `==` `true` |
| Screen has faded to black | 0.81 s | session | `org.gnome.ScreenSaver` | signal `ActiveChanged` `==` `true` |
| Display powers off | 0.99 s | session | `org.gnome.Mutter.DisplayConfig` | property `PowerSaveMode` `>` `0` |
| Display powers back on, still locked | on input | session | `org.gnome.Mutter.DisplayConfig` | property `PowerSaveMode` `==` `0` |
| Shield lifts | on unlock | session | `org.gnome.ScreenSaver` | signal `ActiveChanged` `==` `false` |
| Unlocked | on unlock | system | `org.freedesktop.login1.Session` | property `LockedHint` `==` `false` |

Use the interface as `sender`, except for login1, whose sender is `org.freedesktop.login1`.

- **Idle locks run in a different order.** This comes from reading the shell's source and has not been measured. After `idle-delay` the screen fades for 10 s and `ActiveChanged(true)` fires, and `LockedHint` follows only once `lock-delay` has passed. Waking in that gap means no lock happens, so a black screen does not imply a locked session.
- **Set `sender` on `ActiveChanged`.** GNOME 50 emits it twice, from gnome-shell and from a separate `org.gnome.ScreenSaver` service, so a trigger without `sender` runs twice.
- **Signals to avoid.** `WakeUpScreen` and login1's `Lock` and `Unlock` have no arguments, so they cannot fire a trigger. `WakeUpScreen` also arrived at unlock, not when the display woke.
- **`IdleHint`.** It flipped with the screen going black here, but it means idle, not locked or black.
- **Suspend and resume.** Signal `PrepareForSleep` on `org.freedesktop.login1.Manager` carries `true` before sleep and `false` after. This has not been measured.

### Example: suspend when the display turns off

This is a user Home Manager configuration, assuming `./godbus-monitor.nix` is available alongside it:

```nix
{ pkgs, ... }:
{
  imports = [ ./godbus-monitor.nix ];

  services.godbus-monitor = {
    enable = true;
    triggers = [
      {
        name = "suspend-on-display-off";
        bus = "session";
        sender = "org.gnome.Mutter.DisplayConfig";
        interface = "org.gnome.Mutter.DisplayConfig";
        property = "PowerSaveMode";
        operator = ">";
        expected_value = "0";
        argv = [ "${pkgs.systemd}/bin/systemctl" "--no-block" "suspend" ];
      }
    ];
  };
}
```

A manual lock turns the display off within a second, so this suspends almost immediately on every manual lock. The command requests suspend as the user, subject to system authorization and inhibitors.
