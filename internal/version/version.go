// Package version reports the build version of the rigscope binary.
//
// Release builds set Version with:
//
//	go build -ldflags "-X github.com/Triet1402/rigscope/internal/version.Version=1.0.0"
package version

// Version is the release version. Development builds report "dev".
var Version = "dev"
