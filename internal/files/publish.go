package files

import (
	"errors"
	"io/fs"
	"os"
)

// PublishNoReplace moves from to to, failing with fs.ErrExist if to already
// exists. A hard link is created first because creating a link refuses an
// existing name atomically, so two uploads finishing at once can't overwrite
// each other. Folders, and filesystems without hard links, fall back to a
// check followed by a rename.
func PublishNoReplace(root *os.Root, from, to string) error {
	err := root.Link(from, to)
	if err == nil {
		return root.Remove(from)
	}
	if errors.Is(err, fs.ErrExist) {
		return fs.ErrExist
	}
	if _, err := root.Lstat(to); err == nil {
		return fs.ErrExist
	}
	return root.Rename(from, to)
}
