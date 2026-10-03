# Security policy

## Supported versions

Only the latest release receives fixes.

## Reporting a vulnerability

Please do not open a public issue. Report it privately through GitHub: the repository's **Security** tab → **Report a vulnerability** ([open a report](https://github.com/rezzminator/callmeter/security/advisories/new)). The report and its fix are discussed in that private advisory, and a fix ships as a new release.

## What counts

- **The binary download.** The plugin's wrapper downloads the release binary on first use and must refuse any file whose SHA-256 differs from the `SHA256SUMS` pinned in the plugin. A way past that check, or a way to make the wrapper run another file, is a vulnerability.
- **Privacy.** The store must never keep prompt, message or file content, nor any text the [README's Privacy section](./README.md#-privacy) says is cut. Any path that stores such content is a vulnerability here, not a mere bug.
- **The hook getting in the way.** A payload that makes the hook block, fail or change a Claude Code tool call.
- **Files under `CALLMETER_HOME`.** A way to make callmeter write or delete outside its own directory.
