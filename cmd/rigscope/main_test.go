package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "rigscope ") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestHelpHidesInternalCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if strings.Contains(out.String(), "internal") {
		t.Fatalf("help lists the hidden internal commands:\n%s", out.String())
	}
}

func TestUnknownInternalCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"internal", "nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", out.String())
	}
}

// TestInternalPCIReadDispatchesTheHelper proves the hidden verb reaches the
// helper without ever touching hardware: the request uses a misaligned offset,
// which the helper rejects during validation, before it resolves or opens the
// resource. A well-formed request for a real device path would map that BAR on
// any machine that has it, outside the collector's gate and lease, so this
// test must never send one. Successful BAR reads are covered inside the GPU
// package's fixture harness.
func TestInternalPCIReadDispatchesTheHelper(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	original := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = original })
	go func() {
		_, _ = fmt.Fprint(writer, `{"path":"/sys/bus/pci/devices/0000:01:00.0/resource0","offsets":[1]}`)
		_ = writer.Close()
	}()

	var out, errOut bytes.Buffer
	if code := run([]string{"internal", "pci-read"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2 (validation rejection); stderr %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", out.String())
	}
	if !strings.Contains(errOut.String(), "pci-read:") {
		t.Fatalf("stderr = %q, want a pci-read validation error", errOut.String())
	}
}

func TestLicenseIncludesTermsAndCopyright(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"license"}, &out, &errOut); code != 0 {
		t.Fatalf("license: %s", errOut.String())
	}
	for _, text := range []string{"AGPL-3.0-only", "Triet", "GNU AFFERO GENERAL PUBLIC LICENSE", "13. Remote Network Interaction"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("license output missing %q", text)
		}
	}
}
