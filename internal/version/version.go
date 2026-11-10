// Package version holds build-time version information.
package version

// Version is the ContextSlim release version. Overridden with -ldflags at build time.
var Version = "0.1.0-dev"

// Compressor identifies the compression rule set. It changes whenever the
// output of any parser or of the compressor can change for the same input.
const Compressor = "cs-compress/1"
