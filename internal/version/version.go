// Package version holds build-time metadata injected via -ldflags. Values are
// overridden at link time; the defaults here are what a `go run`/`go build`
// without ldflags reports, so they must never look like a real release.
package version

var (
	// Version is the semantic version of this build, e.g. "0.1.0".
	Version = "dev"
	// Commit is the git commit SHA this build was produced from.
	Commit = "unknown"
	// BuildDate is the RFC 3339 UTC timestamp this build was produced at.
	BuildDate = "unknown"
)

// String returns a single-line human-readable build identifier, e.g.
// "0.1.0 (commit abc1234, built 2026-08-09T00:00:00Z)".
func String() string {
	return Version + " (commit " + Commit + ", built " + BuildDate + ")"
}
