package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"julia/internal/core"
)

type Store struct {
	Directory string
	mu        sync.Mutex
	locks     map[string]*os.File
	closed    bool
}

var validID = regexp.MustCompile(`^[a-f0-9]{24}$`)

func Default() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(home, ".local", "state")
	}
	return &Store{Directory: filepath.Join(base, "julia", "sessions")}, nil
}
func New(directory, provider, model string) *core.Session {
	now := time.Now()
	return &core.Session{ID: hex.EncodeToString(randomBytes()), Title: "New conversation", Directory: directory, Provider: provider, Model: model, CreatedAt: now, UpdatedAt: now}
}
func randomBytes() []byte {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func Title(prompt string) string {
	t := strings.Join(strings.Fields(prompt), " ")
	r := []rune(t)
	if len(r) > 72 {
		return string(r[:69]) + "…"
	}
	return t
}
func (s *Store) Save(v *core.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == nil {
		return errors.New("session is required")
	}
	if err := s.lock(v.ID); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Directory, ".session-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(s.Directory, v.ID+".json")); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	directory, err := os.OpenFile(s.Directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open session directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync session directory: %w", err)
	}
	return nil
}

// Lock reserves a session for this Store's lifetime. The lock file remains on
// disk after Close: unlinking it would let another process lock a different inode.
func (s *Store) Lock(id string) error { s.mu.Lock(); defer s.mu.Unlock(); return s.lock(id) }
func (s *Store) lock(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid session ID")
	}
	if s.closed {
		return errors.New("session store is closed")
	}
	if s.locks[id] != nil {
		return nil
	}
	if err := os.MkdirAll(s.Directory, 0700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	directory, err := os.OpenFile(s.Directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open session directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Chmod(0700); err != nil {
		return fmt.Errorf("protect session directory: %w", err)
	}
	fd, err := unix.Openat(int(directory.Fd()), id+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("open session lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), id+".lock")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		f.Close()
		return fmt.Errorf("inspect session lock: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Getuid()) {
		f.Close()
		return errors.New("session lock must be a regular file owned by this user with no hardlinks")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return fmt.Errorf("protect session lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return errors.New("session is in use by another JULIA process")
		}
		return fmt.Errorf("lock session: %w", err)
	}
	if s.locks == nil {
		s.locks = make(map[string]*os.File)
	}
	s.locks[id] = f
	return nil
}

// Close releases every session reservation. It is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var failures []error
	for id, f := range s.locks {
		if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
			failures = append(failures, fmt.Errorf("unlock session %s: %w", id, err))
		}
		if err := f.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close session lock %s: %w", id, err))
		}
	}
	s.locks = nil
	return errors.Join(failures...)
}
func (s *Store) Load(id string) (*core.Session, error) {
	if !validID.MatchString(id) {
		return nil, errors.New("invalid session ID")
	}
	b, err := os.ReadFile(filepath.Join(s.Directory, id+".json"))
	if err != nil {
		return nil, err
	}
	var v core.Session
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if v.ID != id {
		return nil, errors.New("session ID does not match its file")
	}
	return &v, nil
}
func (s *Store) List() ([]*core.Session, error) {
	entries, err := os.ReadDir(s.Directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*core.Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		v, err := s.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}
func (s *Store) Latest(directory string) (*core.Session, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, v := range all {
		// A subagent's session has a parent, and the consolidation pass a
		// role: neither is a conversation to continue.
		if v.Directory == directory && v.ParentID == "" && v.Role == "" {
			return v, nil
		}
	}
	return nil, errors.New("no saved session for this directory")
}
