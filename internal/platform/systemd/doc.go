// Package systemd renders Rigscope's systemd units and manages them through an
// injectable command runner.
//
// It owns unit names, hardening, user vs system scope differences and the
// Manager abstraction. It does not decide which components are enabled, write
// configuration, or install binaries.
package systemd
