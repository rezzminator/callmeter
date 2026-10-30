# The checks GitHub runs on a pull request to main (.github/workflows/release.yml),
# runnable here first: `make release-check` runs them all.
GO_VERSION := $(shell awk '/^go /{print $$2}' go.mod)
# go.mod's toolchain exactly, as setup-go gives CI; build-release.sh refuses any other.
export GOTOOLCHAIN := go$(GO_VERSION)
MANIFEST := plugins/callmeter/.claude-plugin/plugin.json
JSON_VERSION := grep -o '"version"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n 1 | sed 's/^"version"[[:space:]]*:[[:space:]]*"//; s/"$$//'

.PHONY: release-check check leak release

release-check: check leak release

check:
	unformatted=$$(sh -c '"$$(go env GOROOT)/bin/gofmt" -l .'); \
	[ -z "$$unformatted" ] || { printf 'gofmt: not formatted:\n%s\n' "$$unformatted"; exit 1; }
	go vet ./...
	go test ./...
	claude plugin validate --strict plugins/callmeter

# LEAK_TERMS names the terms file (CI writes the secret to one); unset, scripts/leak-terms.txt.
leak:
	scripts/leak-check.sh

# The version must move past main's: release-check.sh takes main's version as its base.
release:
	git rev-parse --verify --quiet origin/main >/dev/null || { echo "release: origin/main not found; fetch it first" >&2; exit 1; }
	base=$$(git show origin/main:$(MANIFEST) 2>/dev/null | $(JSON_VERSION)); \
	scripts/release-check.sh --release "$${base:-0.0.0}"
	v=$$(cat $(MANIFEST) | $(JSON_VERSION)); \
	awk -v heading="## [$$v]" 'index($$0, heading) == 1 { found = 1; next } found && /^## \[/ { exit } found' CHANGELOG.md | grep -q '' \
	  || { echo "release: CHANGELOG.md section for $$v yields no release notes" >&2; exit 1; }
