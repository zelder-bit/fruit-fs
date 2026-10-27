//go:build !linux

package files

func diskSpace(string) (total, free uint64) { return 0, 0 }
