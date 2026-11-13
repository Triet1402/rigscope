package processes

import (
	"io/fs"
	"sync"
)

// UserNames resolves numeric UIDs to login names.
type UserNames interface {
	UserName(uid int) (name string, ok bool)
}

// PasswdFile is a lazily loaded /etc/passwd reader. A failed read is not
// cached, so a transient error can recover on a later scan.
type PasswdFile struct {
	fsys fs.FS
	path string

	mu     sync.Mutex
	users  map[int]string
	loaded bool
}

// NewPasswdFile reads path from fsys. In production fsys is os.DirFS("/etc")
// and path is "passwd"; tests pass a fixture file. A nil fsys or an invalid
// path yields an empty lookup (UID 0 still maps to root).
func NewPasswdFile(fsys fs.FS, path string) *PasswdFile {
	return &PasswdFile{fsys: fsys, path: path}
}

// UserName returns the login name for uid. UID 0 falls back to "root" even
// when the password database is unreadable, which covers kernel threads.
func (p *PasswdFile) UserName(uid int) (string, bool) {
	if p == nil {
		if uid == 0 {
			return "root", true
		}
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded && p.fsys != nil && p.path != "" {
		if text, err := fs.ReadFile(p.fsys, p.path); err == nil {
			p.users = parsePasswd(text)
			p.loaded = true
		}
	}
	if name, ok := p.users[uid]; ok && name != "" {
		return truncateText(name, maxUserLength), true
	}
	if uid == 0 {
		return "root", true
	}
	return "", false
}
