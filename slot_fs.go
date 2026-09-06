package reasoningeffort

import (
	"os"
	"path/filepath"
	"time"
)

// SlotFileInfo is a directory entry returned by SlotFS.ListFiles.
type SlotFileInfo struct {
	// Name is the file name, relative to the slot save directory.
	Name string
	// ModTime is the file's last modification time.
	ModTime time.Time
}

// SlotFS abstracts the file operations the slot LRU performs on the
// directory that holds llama-server's slot save files. All names are
// relative to that directory. The interface exists so the LRU logic never
// touches the OS directly; a future NFS/SMB implementation (pure-Go
// user-space clients, since Caddy may run on a different host than
// llama-server) can be dropped in without touching the LRU or middleware.
//
// Implementations must wrap os.ErrNotExist in the errors they return for
// missing files, so callers can detect absence with errors.Is.
type SlotFS interface {
	// Remove deletes the file with the given name.
	Remove(name string) error
	// Stat reports whether the file with the given name exists.
	Stat(name string) (bool, error)
	// ReadFile returns the contents of the file with the given name.
	ReadFile(name string) ([]byte, error)
	// WriteFile writes data to the file with the given name, creating it
	// if necessary and truncating an existing file.
	WriteFile(name string, data []byte) error
	// Rename replaces oldname with newname. Implementations should make
	// this atomic when the underlying filesystem allows it.
	Rename(oldname, newname string) error
	// ListFiles returns the regular files in the directory with their
	// modification times.
	ListFiles() ([]SlotFileInfo, error)
}

// localFS is the SlotFS implementation backed by the local filesystem.
type localFS struct {
	root string
}

// newLocalFS returns a SlotFS rooted at dir.
func newLocalFS(dir string) SlotFS {
	return &localFS{root: dir}
}

func (f *localFS) path(name string) string {
	return filepath.Join(f.root, name)
}

func (f *localFS) Remove(name string) error {
	return os.Remove(f.path(name))
}

func (f *localFS) Stat(name string) (bool, error) {
	_, err := os.Stat(f.path(name))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (f *localFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(f.path(name))
}

func (f *localFS) WriteFile(name string, data []byte) error {
	return os.WriteFile(f.path(name), data, 0o644)
}

func (f *localFS) Rename(oldname, newname string) error {
	return os.Rename(f.path(oldname), f.path(newname))
}

func (f *localFS) ListFiles() ([]SlotFileInfo, error) {
	entries, err := os.ReadDir(f.root)
	if err != nil {
		return nil, err
	}
	files := make([]SlotFileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Skip unreadable entries (e.g. dangling symlinks).
			continue
		}
		files = append(files, SlotFileInfo{Name: e.Name(), ModTime: info.ModTime()})
	}
	return files, nil
}
