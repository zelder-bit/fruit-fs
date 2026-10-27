package files

import (
	"os"
	"syscall"
)

// openRead opens p without blocking, so a FIFO or device in the data folder
// can't hang the request. Callers check the file type afterwards.
func openRead(root *os.Root, p string) (*os.File, error) {
	return root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
