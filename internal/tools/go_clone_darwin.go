//go:build darwin

package tools

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func cloneCacheFile(source, parent *os.File, target string) (bool, error) {
	err := unix.Fclonefileat(int(source.Fd()), int(parent.Fd()), target, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) {
		return false, nil
	}
	return false, err
}
