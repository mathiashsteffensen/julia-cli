//go:build !darwin

package tools

import "os"

func cloneCacheFile(source, parent *os.File, target string) (bool, error) { return false, nil }
