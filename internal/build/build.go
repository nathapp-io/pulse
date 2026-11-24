// Package build carries build-time identity.
package build

// Version is the release version, overridable at build time with
// -ldflags "-X pulse/internal/build.Version=1.2.3".
var Version = "0.1.0-dev"
