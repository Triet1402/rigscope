package gpu

import (
	"context"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeResource(t *testing.T, size int, words map[int]uint32) (dir, name string) {
	t.Helper()
	dir = t.TempDir()
	name = filepath.Join(dir, "resource0")
	data := make([]byte, size)
	for offset, word := range words {
		binary.LittleEndian.PutUint32(data[offset:], word)
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatalf("write resource: %v", err)
	}
	return dir, name
}

func requireWord(t *testing.T, values []*uint32, index int, want uint32) {
	t.Helper()
	if index >= len(values) || values[index] == nil {
		t.Fatalf("values[%d] = nil, want 0x%08x", index, want)
	}
	if *values[index] != want {
		t.Fatalf("values[%d] = 0x%08x, want 0x%08x", index, *values[index], want)
	}
}

func TestMappedSourceReadsAlignedWordsAcrossPages(t *testing.T) {
	dir, _ := writeResource(t, 0x21000, map[int]uint32{
		0x2046c: 0xc0002480,
		0xe2a8:  0x80008500,
		0x1ffc:  0x11111111,
		0x2004:  0x22222222,
	})
	source := MappedSource{FS: os.DirFS(dir), Timeout: time.Second}
	values, err := source.ReadRegisters(context.Background(), "resource0",
		[]int64{0x2046c, 0xe2a8, 0x1ffc, 0x2004})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	requireWord(t, values, 0, 0xc0002480)
	requireWord(t, values, 1, 0x80008500)
	requireWord(t, values, 2, 0x11111111)
	requireWord(t, values, 3, 0x22222222)
}

func TestMappedSourceUnreadableAndOutOfRangeStayNil(t *testing.T) {
	dir, _ := writeResource(t, 0x21000, map[int]uint32{0: 0xaabbccdd})
	source := MappedSource{FS: os.DirFS(dir), Timeout: time.Second}

	if _, err := source.ReadRegisters(context.Background(), "resource0-missing", []int64{0}); err == nil {
		t.Fatal("missing resource read err = nil, want an error")
	}

	values, err := source.ReadRegisters(context.Background(), "resource0", []int64{0x22000})
	if err != nil {
		t.Fatalf("beyond-EOF read: %v", err)
	}
	if len(values) != 1 || values[0] != nil {
		t.Fatalf("beyond-EOF values = %v, want one nil", values)
	}

	values, err = source.ReadRegisters(context.Background(), "resource0", []int64{1})
	if err != nil || len(values) != 1 || values[0] != nil {
		t.Fatalf("unaligned read = %v, %v; want one nil and no error", values, err)
	}

	values, err = source.ReadRegisters(context.Background(), "resource0", []int64{0x1000004})
	if err != nil || len(values) != 1 || values[0] != nil {
		t.Fatalf("over-limit read = %v, %v; want one nil and no error", values, err)
	}

	tooMany := make([]int64, maxRegisterOffsets+1)
	values, err = source.ReadRegisters(context.Background(), "resource0", tooMany)
	if err != nil || len(values) != len(tooMany) {
		t.Fatalf("too-many read = %d values, %v", len(values), err)
	}
	for index, value := range values {
		if value != nil {
			t.Fatalf("value[%d] = %v, want nil", index, *value)
		}
	}

	empty, err := source.ReadRegisters(context.Background(), "resource0", nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty read = %v, %v", empty, err)
	}
}

// blockingFS stalls Open so the deadline path is exercised deterministically.
type blockingFS struct {
	release chan struct{}
}

func (b blockingFS) Open(string) (fs.File, error) {
	<-b.release
	return nil, fs.ErrNotExist
}

func TestMappedSourceTimesOutInsteadOfStallingSampling(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	source := MappedSource{FS: blockingFS{release: release}, Timeout: 5 * time.Millisecond}
	started := time.Now()
	values, err := source.ReadRegisters(context.Background(), "resource0", []int64{0})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if len(values) != 1 || values[0] != nil {
		t.Fatalf("timed-out values = %v, want one nil", values)
	}
	if elapsed > time.Second {
		t.Fatalf("timeout took %s, want the deadline to bound it", elapsed)
	}
}

func TestGuardFaultConvertsFaultPanics(t *testing.T) {
	if guardFault(func() {}) {
		t.Fatal("guardFault reported a fault for a clean function")
	}
	if !guardFault(func() { panic("simulated fault") }) {
		t.Fatal("guardFault did not report the panic")
	}
}

// TestGuardFaultRecoversFromRealSIGBUS truncates a file under a live mapping,
// which is exactly what a disappearing PCI device looks like: the next access
// faults. Without SetPanicOnFault this test would kill the test process.
func TestGuardFaultRecoversFromRealSIGBUS(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "resource0")
	if err := os.WriteFile(name, make([]byte, 8192), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	file, err := os.Open(name)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer file.Close()
	data, err := syscall.Mmap(int(file.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer func() { _ = syscall.Munmap(data) }()

	if err := os.Truncate(name, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	sink := 0
	faulted := guardFault(func() { sink += int(data[0]) })
	if !faulted {
		t.Fatalf("guardFault missed the SIGBUS (read %d)", sink)
	}
}

func TestProbeRegisterAccessMapsWithoutReading(t *testing.T) {
	_, name := writeResource(t, 4096, map[int]uint32{0: 0xcafebabe})
	if err := ProbeRegisterAccess(name); err != nil {
		t.Fatalf("probe resource: %v", err)
	}

	// A pipe supports read(2) but not mmap. If the probe ever fell back to
	// read(2) this would succeed (or block), so an error proves the mapping.
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probeRegisterAccessWith(func(string) (*os.File, error) { return readEnd, nil }, "pipe"); err == nil {
		t.Fatal("pipe descriptor mapped successfully; the probe did not use mmap")
	}

	// A mapping past the end of a short file maps successfully but faults on
	// the first access. The probe must not dereference it.
	short := filepath.Join(t.TempDir(), "resource0")
	if err := os.WriteFile(short, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ProbeRegisterAccess(short); err != nil {
		t.Fatalf("probe of a short file failed; it dereferenced the mapping: %v", err)
	}
}
