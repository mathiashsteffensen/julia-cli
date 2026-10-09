package tools

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// parentNoFollow pins every path component by descriptor and refuses symlinks
// atomically at open time. os.Root holds the original workspace identity; openat
// with O_NOFOLLOW prevents a path-check/open race into a protected directory.
func (e *Executor) parentNoFollow(name string, create bool) (*os.File, string, error) {
	current, err := e.root.Open(".")
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(name)), "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "." {
			continue
		}
		if part == ".." || protected(part) {
			current.Close()
			return nil, "", errors.New("forbidden path component")
		}
		fd, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if mkdirErr := unix.Mkdirat(int(current.Fd()), part, 0755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				current.Close()
				return nil, "", mkdirErr
			}
			fd, err = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		current.Close()
		if err != nil {
			return nil, "", fmt.Errorf("open directory without symlinks: %w", err)
		}
		current = os.NewFile(uintptr(fd), part)
	}
	base := parts[len(parts)-1]
	if base == ".." || protected(base) {
		current.Close()
		return nil, "", errors.New("forbidden path component")
	}
	return current, base, nil
}
func (e *Executor) openNoFollow(name string) (*os.File, error) {
	parent, base, err := e.parentNoFollow(name, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (e *Executor) atomicWriteNoFollow(name string, b []byte, mode os.FileMode) error {
	parent, base, err := e.parentNoFollow(name, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := ".jarvis-write-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(parent.Fd()), temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer unix.Unlinkat(int(parent.Fd()), temp, 0)
	f := os.NewFile(uintptr(fd), temp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = unix.Renameat(int(parent.Fd()), temp, int(parent.Fd()), base); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	return nil
}

// walk never follows a directory symlink, even if it changes after enumeration.
func (e *Executor) walk(name string, fn fs.WalkDirFunc) error {
	f, err := e.openNoFollow(name)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	var visit func(string, fs.DirEntry) error
	visit = func(path string, d fs.DirEntry) error {
		if err := fn(path, d, nil); err != nil {
			if err == fs.SkipDir {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		dir, err := e.openNoFollow(path)
		if err != nil {
			return fn(path, d, err)
		}
		entries, err := dir.ReadDir(10001)
		dir.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fn(path, d, err)
		}
		if len(entries) > 10000 {
			return errors.New("directory exceeds 10000 entries; narrow the search path")
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := visit(filepath.Join(path, entry.Name()), entry); err != nil {
				return err
			}
		}
		return nil
	}
	err = visit(name, fs.FileInfoToDirEntry(info))
	if err == fs.SkipAll {
		return nil
	}
	return err
}
