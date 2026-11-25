// Package version carries build metadata stamped into the binary at link time.
package version

// Revision is the build's VCS revision, injected via -ldflags in container builds, which lack .git.
// It is "unknown" otherwise, and build info then falls back to the vcs.revision build setting.
var Revision = "unknown"

// Version is the release version (a semver tag), injected via -ldflags in release images; "dev" otherwise.
var Version = "dev"
