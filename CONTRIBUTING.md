# Contributing to callmeter

Thank you for helping. callmeter runs on every hook of every Claude Code session on a machine, and its store is the user's only record of what those sessions did. A change is judged first by what it could lose: an event, a row, or the user's privacy.

## Before you start

- For anything larger than a small fix, open an issue first so the design can be agreed. Every decision lives in [docs/design.md](./docs/design.md): a behaviour change lands there first, then in the code.
- Looking for a place to start? Try a [good first issue](https://github.com/rezzminator/callmeter/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).
- Everyone taking part follows the [code of conduct](./CODE_OF_CONDUCT.md).

## Set up

- Go 1.27.1 (the version in `go.mod`), with CGO off.
- `python3` on your `PATH`: the Python parser's tests need it, and its absence fails them.
- The Claude Code CLI, for `claude plugin validate`.

```sh
git clone https://github.com/rezzminator/callmeter.git
cd callmeter
go test ./...
```

## Branches

Work lands on `develop`, the default branch; open pull requests against it. `main` moves only by release.

## What a change owes

- Tests, as [docs/testing.md](./docs/testing.md) states: a test beside its source file, a real SQLite store under a temp dir, and captured hook payloads only, never a hand-written guess at a payload's shape. Fixtures are written only by `scripts/sanitize-capture.py`.
- [docs/design.md](./docs/design.md) updated in the same pull request when behaviour changes.
- A line under `## [Unreleased]` in [CHANGELOG.md](./CHANGELOG.md) for anything a user would notice.
- These checks passing:

```sh
go test ./...
go vet ./...
gofmt -l .                                   # prints nothing
claude plugin validate --strict .
claude plugin validate --strict plugins/callmeter
```

## Rules that never bend

- **Privacy.** The store never keeps prompt, message or file content, only its `{name}_bytes` count. A test that feeds such text asserts it appears nowhere in the store.
- **The hook never gets in the way.** It exits 0 on every path, prints nothing on stdout, and never blocks or changes a call. A gap it cannot avoid is counted as a fault, never hidden.
- **Nothing identifying in a file.** No machine-absolute path (`/Users/…`, `/home/…`), no username, no email address. Fixtures use `/tmp/demo-proj/…` and `/tmp/demo-home/…`.
- **`plugins/callmeter/` holds only what installs:** the manifest, hooks, wrapper and skill. Everything else lives outside it.

## CI and the leak gate

CI runs gofmt, vet, the tests, both plugin validates and the leak gate (`scripts/leak-check.sh`). The leak gate reads its private term list from a repository secret, and GitHub gives no secrets to pull requests from forks, so CI skips that one step on a fork's pull request: a maintainer runs it before merging, and it runs again on every push to `develop` and `main`.

To run it yourself, put your own username and host name in `scripts/leak-terms.txt` (gitignored), one lowercase regex per line, then run `scripts/leak-check.sh`.

The end-to-end suite (`CALLMETER_E2E=1 go test ./e2e/... -count=1`) runs a real `claude -p` and costs tokens; you don't need to run it, a maintainer does.

## Commits

Commit subjects follow `type(scope): summary`, for example `fix(cmdparse): attribute less operands as reads`.

## License

By contributing you agree that your contribution is licensed under the project's [MIT License](./LICENSE).
