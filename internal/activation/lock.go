package activation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
)

// Locker serialises installation and upgrade operations. The returned function
// releases the lock and must be safe to call once per call.
//
// owner identifies one operation. A Locker may re-enter for the same owner
// (recovery, then the upgrade itself) but a different owner is a separate
// operation and must not borrow a lock held by someone else in the same
// process.
//
// CLI wiring implements this over the same internal/platform/lock helper
// install and uninstall use, so upgrade, rollback, recovery and migrate share
// one installation lock.
type Locker interface {
	Lock(ctx context.Context, owner string) (unlock func(), err error)
}

// LockFileProvider is implemented by a Locker whose held lock has an open
// descriptor the staged migration child can inherit. Without it the child
// would have to acquire the lock itself and deadlock behind its parent.
type LockFileProvider interface {
	File() *os.File
}

// ErrLocked is returned when another installation or upgrade holds the shared
// installation lock.
var ErrLocked = errors.New("another rigscope installation or upgrade is already running")

// NewOwnerToken returns an unpredictable token that scopes re-entrant locking
// to one operation.
func NewOwnerToken() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("owner-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}
