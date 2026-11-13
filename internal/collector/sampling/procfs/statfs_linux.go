//go:build linux

package procfs

import (
	"fmt"
	"math"
	"syscall"
)

// OSStatFS reads capacity for one mount point through statfs(2).
func OSStatFS(path string) (StatFSSnapshot, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return StatFSSnapshot{}, err
	}
	blockSize := int64(stats.Bsize)
	if blockSize <= 0 {
		return StatFSSnapshot{}, fmt.Errorf("statfs %s: invalid block size %d", path, blockSize)
	}
	blocks, ok := uintToInt64(uint64(stats.Blocks))
	if !ok {
		return StatFSSnapshot{}, fmt.Errorf("statfs %s: block count overflows int64", path)
	}
	free, ok := uintToInt64(uint64(stats.Bfree))
	if !ok {
		return StatFSSnapshot{}, fmt.Errorf("statfs %s: free block count overflows int64", path)
	}
	snapshot := StatFSSnapshot{Bsize: blockSize, Blocks: blocks, Bfree: free}
	// Inode counts are optional, but they are a pair: if either conversion
	// fails the snapshot carries neither. Leaving a converted total next to a
	// zeroed free count would report every inode as used.
	if files, ffree, ok := inodePair(uint64(stats.Files), uint64(stats.Ffree)); ok {
		snapshot.Files = files
		snapshot.Ffree = ffree
	}
	return snapshot, nil
}

// inodePair converts both inode counters together. Either one overflowing
// int64 makes the pair unavailable.
func inodePair(files, ffree uint64) (int64, int64, bool) {
	total, ok := uintToInt64(files)
	if !ok {
		return 0, 0, false
	}
	free, ok := uintToInt64(ffree)
	if !ok {
		return 0, 0, false
	}
	return total, free, true
}

func uintToInt64(value uint64) (int64, bool) {
	if value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}
