//go:build !linux

package files

import "os"

func openRead(root *os.Root, p string) (*os.File, error) { return root.Open(p) }
