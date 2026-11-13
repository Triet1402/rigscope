//go:build linux

package gpu

// selfExecutablePath returns the kernel's stable handle on the running
// binary. Executing /proc/self/exe reads the inode this process is already
// running, so an upgrade that unlinks or replaces the binary's pathname
// between locating and starting the helper cannot make it fail or run a
// different build.
func selfExecutablePath() (string, error) {
	return "/proc/self/exe", nil
}
