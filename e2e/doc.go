// Package e2e is callmeter's live end-to-end suite: real headless Claude Code
// sessions (haiku, project and local settings only) load the plugin through
// --plugin-dir, and the test reads the store they wrote. It costs tokens and
// needs the host's Claude Code login, so it runs only with CALLMETER_E2E=1.
// The plugin is never enabled on the host; every run keeps its state in a
// fresh directory under /tmp/callmeter/e2e/.
package e2e
