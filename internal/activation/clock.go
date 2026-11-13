package activation

import (
	"os"
	"time"
)

// Clock is the time source used for journal timestamps and lock retries.
type Clock interface {
	Now() time.Time
	Sleep(time.Duration)
}

// RealClock uses the system clock.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now().UTC() }

// Sleep implements Clock.
func (RealClock) Sleep(d time.Duration) { time.Sleep(d) }

// FS is the filesystem surface the transaction needs. Tests commonly use the
// OS implementation against a temporary root.
type FS interface {
	ReadFile(name string) ([]byte, error)
	WriteFileAtomic(name string, data []byte, perm os.FileMode) error
	MkdirAll(name string, perm os.FileMode) error
	Remove(name string) error
	RemoveAll(name string) error
	Stat(name string) (os.FileInfo, error)
}
