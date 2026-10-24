// Package version holds build metadata injected by goreleaser through ldflags.
package version

var (
	// Version is the release version without the leading v.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the commit date of the build.
	Date = "unknown"
)
