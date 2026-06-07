// Package version exposes build metadata. It never contacts the network:
// there is no update check by design (see AUDIT.md).
package version

// These are set at build time via -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable build identifier.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
