// Package fsutil owns the filesystem primitives that make installation safe:
// atomic private file writes and atomic binary replacement that retains the
// previous executable as ".prev".
//
// It does not know about Rigscope's layout, configuration or systemd. Callers
// pass exact paths and modes.
package fsutil
