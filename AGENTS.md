# AGENTS.md

D-Bus trigger daemon: watches properties and signals, runs a command when a value matches. See `README.md` for behaviour.

## Layout

- `config.go`: `Trigger` schema and validation. Anything checkable without a bus is checked here.
- `main.go`: bus connections, signal dispatch, condition evaluation.
- `executor.go`: per-trigger debounce state machine and process lifecycle.
- `godbus-monitor.nix`: Home Manager module. Its options mirror `Trigger`, so a new field goes in both, and in the README.

## Verify

```sh
gofmt -l . && go vet ./... && staticcheck ./... && go test -race ./...
```

Changing `go.mod` dependencies also means updating `vendorHash`.

## Safety

- Tests use only the private bus from `signalTestPrivateBus` and harmless marker commands. Never point them at the real buses.
- Never run `triggers_example.json` without `-dry-run`: it suspends the machine.
- To check real desktop behaviour, only observe: narrowly filtered `dbus-monitor`, or the daemon with `-dry-run`. Locking the screen is the user's action.

## Principles

- Pick the simplest thing that is correct. KISS and DRY, without overengineering: no speculative options or abstractions, and no checks for states validated config cannot produce.
- Prefer deleting code to adding it, and use the standard library and godbus before writing your own.
- Be direct and critical. Say when something is wrong or a request is a bad idea, including about existing code.
- Reproduce a bug with a failing test or a live run on a private bus before fixing it. Report what was actually verified.
