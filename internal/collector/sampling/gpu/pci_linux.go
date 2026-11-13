//go:build linux

package gpu

import (
	"context"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// defaultRegisterReadTimeout is the prototype's python helper budget.
const defaultRegisterReadTimeout = 1500 * time.Millisecond

// MappedSource reads 32-bit words from a PCI resource file (`resource0`, the
// kernel's mapped view of BAR0) through read-only, shared file mappings.
//
// Linux memory BARs require mmap: read(2)/pread(2) return EIO. Bus-master
// memory BARs are not RAM, so reads go straight to the device; a device that
// disappears mid-read faults and is reported as unreadable rather than
// crashing the process (see [guardFault]).
//
// This is not the production source: [HelperSource] runs the same mapping in
// a short-lived child process (`rigscope internal pci-read`) so that a device
// that hangs cannot stall sampling. MappedSource remains for tests and for
// the helper child itself, where abandoning a goroutine on the deadline is
// harmless because the parent kills the whole process.
type MappedSource struct {
	// FS is rooted at "/" (or a fixture root); nil disables reads.
	FS fs.FS
	// Timeout bounds one batch, 1.5 s by default.
	Timeout time.Duration
}

// ReadRegisters implements [RegisterSource].
//
// The read runs on its own goroutine with a deadline so a hung device access
// cannot stall sampling. If the deadline fires the goroutine is abandoned; it
// exits when the kernel call returns, and a buffered channel means it never
// blocks on the way out.
func (m MappedSource) ReadRegisters(ctx context.Context, resourcePath string, offsets []int64) ([]*uint32, error) {
	if len(offsets) == 0 {
		return []*uint32{}, nil
	}
	nils := make([]*uint32, len(offsets))
	if len(offsets) > maxRegisterOffsets {
		return nils, nil
	}
	for _, offset := range offsets {
		if offset < 0 || offset%4 != 0 || offset > maxRegisterOffset {
			return nils, nil
		}
	}
	if m.FS == nil {
		return nil, fmt.Errorf("gpu: no filesystem configured for %s", resourcePath)
	}

	timeout := m.Timeout
	if timeout <= 0 {
		timeout = defaultRegisterReadTimeout
	}

	type result struct {
		values []*uint32
		err    error
	}
	done := make(chan result, 1)
	go func() {
		values, err := readMappedWords(m.FS, resourcePath, offsets)
		done <- result{values: values, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nils, ctx.Err()
	case <-timer.C:
		return nils, fmt.Errorf("gpu: PCI resource read timed out after %s", timeout)
	case outcome := <-done:
		return outcome.values, outcome.err
	}
}

// readHostRegisters maps BAR0 of a real /sys resource file. It runs inside
// the pci-read helper child on Linux and returns an error only when the
// resource as a whole cannot be opened or mapped; offsets outside the BAR stay
// nil in the returned slice.
//
// The request path is resolved before it is opened, then the resolved final
// component is opened with O_NOFOLLOW: only a device directory the kernel
// actually exposes can be read, not an arbitrary file that happens to have a
// resource0-shaped path.
func readHostRegisters(resourcePath, root string, offsets []int64) ([]*uint32, error) {
	real, err := resolveResourcePath(resourcePath, root)
	if err != nil {
		return nil, err
	}
	file, err := openResourceNoFollow(real)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return mapNativeWords(file, offsets, real)
}

// openResourceNoFollow opens a resolved BAR0 path without following a symlink
// in its final component, closing the race where the path is replaced between
// resolution and open.
func openResourceNoFollow(resourcePath string) (*os.File, error) {
	return os.OpenFile(resourcePath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// ProbeRegisterAccess proves that path can be opened and mapped read-only,
// without touching the device: the first page is mapped PROT_READ|MAP_SHARED
// and unmapped immediately. Memory BARs do not support read(2)/pread(2) (they
// return EIO), so an mmap is the only permission test that matches what the
// collector does.
//
// No word is dereferenced, so no fault guard and no timeout are needed: a
// suspended or wedged device is never asked for data. Opening a suspended
// device's resource0 is harmless; callers that care about runtime power state
// skip such devices before probing. Open and mmap errors keep their errno, so
// callers can still distinguish EACCES/EPERM (denied) from ENOENT (missing)
// and from anything else (an error).
func ProbeRegisterAccess(path string) error {
	return probeRegisterAccessWith(os.Open, path)
}

// probeRegisterAccessWith is the opener seam: a test can supply a descriptor
// that supports read(2) but not mmap, proving this path never falls back to
// read.
func probeRegisterAccessWith(open func(string) (*os.File, error), path string) error {
	file, err := open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	pageSize := os.Getpagesize()
	data, err := syscall.Mmap(int(file.Fd()), 0, pageSize, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	return syscall.Munmap(data)
}

// readMappedWords opens the resource once and maps each needed page
// read-only. Values outside the file (a BAR shorter than the register map)
// stay nil, matching the prototype's helper.
func readMappedWords(root fs.FS, resourcePath string, offsets []int64) ([]*uint32, error) {
	file, err := root.Open(cleanPath(resourcePath))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	native, ok := file.(*os.File)
	if !ok {
		return nil, fmt.Errorf("gpu: %s is not a native file; cannot map BAR0", resourcePath)
	}
	return mapNativeWords(native, offsets, resourcePath)
}

// mapNativeWords maps the requested pages of an already opened resource.
func mapNativeWords(native *os.File, offsets []int64, resourcePath string) ([]*uint32, error) {
	info, err := native.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()

	pageSize := int64(os.Getpagesize())
	values := make([]*uint32, len(offsets))
	pages := make([]int64, 0, len(offsets))
	indexes := make(map[int64][]int, len(offsets))
	for index, offset := range offsets {
		if offset+4 > size {
			continue
		}
		page := offset - offset%pageSize
		if _, seen := indexes[page]; !seen {
			pages = append(pages, page)
		}
		indexes[page] = append(indexes[page], index)
	}
	for _, page := range pages {
		pageIndexes := indexes[page]
		words := readMappedPage(native, page, pageSize, offsets, pageIndexes)
		for position, index := range pageIndexes {
			values[index] = words[position]
		}
	}
	return values, nil
}

func readMappedPage(file *os.File, page, pageSize int64, offsets []int64, indexes []int) []*uint32 {
	values := make([]*uint32, len(indexes))
	faulted := guardFault(func() {
		data, err := syscall.Mmap(int(file.Fd()), page, int(pageSize), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return
		}
		defer func() { _ = syscall.Munmap(data) }()
		for position, index := range indexes {
			word := binary.LittleEndian.Uint32(data[offsets[index]-page:])
			values[position] = pointer(word)
		}
	})
	if faulted {
		return make([]*uint32, len(indexes))
	}
	return values
}
