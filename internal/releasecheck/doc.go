// Package releasecheck holds the process-tier tests of scripts/release-check.sh:
// when it compares the committed SHA256SUMS with a fresh build and when it does
// not. It has no code of its own; each test runs a copy of the script in a
// temp tree whose build-release.sh is a stub.
package releasecheck
