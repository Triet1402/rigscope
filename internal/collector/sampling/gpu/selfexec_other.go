//go:build !linux

package gpu

import "os"

// selfExecutablePath falls back to the executable pathname on platforms
// without /proc/self/exe. Rigscope only ships Linux collector builds.
func selfExecutablePath() (string, error) {
	return os.Executable()
}
