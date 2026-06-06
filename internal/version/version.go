// Package version exposes build-time metadata injected via -ldflags.
package version

// These variables are populated at build time via:
//
//	-X github.com/huan/huan-agent/internal/version.Version=...
//	-X github.com/huan/huan-agent/internal/version.Commit=...
//	-X github.com/huan/huan-agent/internal/version.BuildTime=...
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// String returns a human-readable one-line version string.
func String() string {
	return Version + " (commit " + Commit + ", built " + BuildTime + ")"
}
