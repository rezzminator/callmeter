// Package wrappertest holds the process-tier tests of the plugin's sh wrapper,
// plugins/callmeter/libexec/callmeter: binary resolution, the checksum-verified
// download, the lock, the cache and the missed.log line. It has no code of its
// own; each test spawns the wrapper against a local httptest server.
package wrappertest
