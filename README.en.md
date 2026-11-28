# craft

[中文](README.md)

craft is a local-first tool for coordinating work across coding sessions. Watch records an external event as a Trigger, matches its Thread to an existing Work, or evaluates whether to start a new one. It assigns accepted Work to an executor and brings questions, results and requests for approval back to the user.

## What it does

- The daemon polls event sources on a schedule, processes the inbox and delivers updates to active Work. Watch keeps its state in a local SQLite database.
- The GitHub reference extension runs as a separate process and speaks JSON-RPC 2.0 over stdio. It picks up review requests and issue assignments, tracks subscribed objects and provides actions to read, comment on, open and merge pull requests.
- Claude Code and Codex share skills for checking what needs attention in the main session and reporting progress from a worker session.
- In a Git project, method records what a change should achieve, ties approval to a specific document revision, tracks tasks through commits and binds verification evidence to a code snapshot.
- outbound can run an action automatically, request approval with a preview, require the user to name the target, or deny the action. The credential vault uses the system keychain when available and encrypted local files as a fallback. Extensions receive the credentials they declare at startup.

## Install

**Available after the first release.** The distribution repository, `chhuax/craft-plugin`, has yet to be created. Run the line for your host in a terminal. Both hosts use the same package, which includes binaries for macOS and Linux on arm64 and amd64, and for Windows on amd64.

**Claude Code**

```sh
claude plugin install craft --marketplace chhuax/craft-plugin
```

**Codex**

```sh
codex plugin marketplace add chhuax/craft-plugin && codex plugin add craft@craft-plugin
```

Claude Code's Bash tool can invoke `craft` by name. In Codex, review and trust the hook, then start a new session. If the opening context includes an executable path, use it wherever the examples say `craft`. From a regular terminal, or if the path is missing, use `bin/craft` inside the installed plugin (`bin/craft.exe` on Windows). Delivery of the opening context in a live Codex session and the Codex hook on Windows still need verification. Binary signing and notarization are planned. [Package design and verification notes](docs/adr/0004-single-universal-package.md)

## Five-minute start

Run the local checks and start the daemon. If you want to try GitHub, prepare the extension as described below before starting the daemon.

```sh
craft doctor
craft daemon start
craft watch status
craft watch list --open
craft watch attention
```

The last three commands show a summary, open Work and items that need your attention. A fresh installation may have no Work yet. State lives in `~/.craft` by default. Set `CRAFT_HOME` to use another directory.

**Optional GitHub setup.** Build the reference extension separately with `make build` in the source repository. Copy `manifest.json` and `craft-ext-github` from `extensions/github/` into `extensions/github/` under the state directory. On Windows, copy the generated manifest and `craft-ext-github.exe` from `bin/extensions/github/`. Once the daemon has started and discovered the extension, run the commands below in your terminal. Enter the GitHub token on standard input when you run the login command. Restarting the daemon loads the credential and applies the enabled setting. [GitHub extension](extensions/README.md)

```sh
craft cred login github
craft ext enable github
craft daemon stop
craft daemon start
craft ext list
```

## Develop and contribute

With Git, Make and the Go version specified in [go.mod](go.mod), run the following from the repository root to build the CLI and extension, explore the commands and test both Go modules. Before the first release, you can use the resulting `bin/craft` (`bin/craft.exe` on Windows) for the walkthrough above.

```sh
make build
go run ./cmd/craft --help
make test
```

The CLI entry point is in `cmd/craft/`. The Watch kernel lives in `internal/engine/`, the daemon in `internal/app/` and the host drivers in `internal/host/`. Host manifests, hooks and shared skills live in `hosts/`. The package builder is in `tools/package/`. See the [specification](docs/spec/00-overview.md), [CLI reference](docs/spec/05-cli.md), [architecture decisions](docs/adr/) and [contributing guide](CONTRIBUTING.md).

## License

craft is licensed under the [Apache License 2.0](LICENSE). Third-party notices are in [NOTICE](NOTICE).
