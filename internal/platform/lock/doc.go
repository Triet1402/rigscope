// Package lock serialises Rigscope installation and service operations with a
// kernel flock on the installation state directory.
//
// It owns only the advisory lock file. It does not decide which operations need
// the lock, create installation directories or clean up state.
package lock
