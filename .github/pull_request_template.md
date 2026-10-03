## What and why

<!-- What this changes, and the issue it closes, if any. -->

## Checklist

- [ ] Targets `develop`
- [ ] A behaviour change is in `docs/design.md`
- [ ] Tests as `docs/testing.md` states: captured payloads, never hand-written ones
- [ ] `go test ./...`, `go vet ./...` and `gofmt -l .` are clean
- [ ] `claude plugin validate --strict .` and `claude plugin validate --strict plugins/callmeter` pass
- [ ] A line under `## [Unreleased]` in `CHANGELOG.md`, when a user would notice
- [ ] No prompt, message or file content reaches the store; no machine path, username or email in any file
