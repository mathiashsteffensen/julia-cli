package session

import (
	"os"
	"testing"
)

func TestPersistenceAndConfinement(t *testing.T) {
	s := &Store{Directory: t.TempDir()}
	defer s.Close()
	v := New("/workspace", "codex", "")
	v.Title = "Research"
	v.State = []byte(`{"test":true}`)
	if err := s.Save(v); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(v.ID)
	if err != nil || got.Title != v.Title {
		t.Fatalf("load: %v %v", got, err)
	}
	info, err := os.Stat(s.Directory + "/" + v.ID + ".json")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("session must be private")
	}
	if _, err = s.Load("../../secret"); err == nil {
		t.Fatal("accepted traversal")
	}
	v.ID = "../bad"
	if err = s.Save(v); err == nil {
		t.Fatal("accepted traversal on save")
	}
}

func TestLockExclusiveAndRelease(t *testing.T) {
	dir := t.TempDir()
	first := &Store{Directory: dir}
	second := &Store{Directory: dir}
	t.Cleanup(func() { first.Close(); second.Close() })
	id := New("/workspace", "codex", "").ID
	if err := first.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := first.Lock(id); err != nil {
		t.Fatal("same store lock must be idempotent", err)
	}
	if err := second.Lock(id); err == nil {
		t.Fatal("two stores locked the same session")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Lock(id); err != nil {
		t.Fatal("lock was not released", err)
	}
}
func TestSaveAutomaticallyLocksAndCreatesDirectories(t *testing.T) {
	dir := t.TempDir() + "/missing/sessions"
	first := &Store{Directory: dir}
	second := &Store{Directory: dir}
	t.Cleanup(func() { first.Close(); second.Close() })
	v := New("/workspace", "codex", "")
	if err := first.Save(v); err != nil {
		t.Fatal(err)
	}
	if err := second.Save(v); err == nil {
		t.Fatal("save ignored another store's lock")
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("session directory is not private", err)
	}
	info, err = os.Stat(dir + "/" + v.ID + ".lock")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("session lock is not private", err)
	}
}
func TestLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Directory: dir}
	defer s.Close()
	id := New("/workspace", "codex", "").ID
	target := t.TempDir() + "/target"
	if err := os.WriteFile(target, []byte("untouched"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir+"/"+id+".lock"); err != nil {
		t.Fatal(err)
	}
	if err := s.Lock(id); err == nil {
		t.Fatal("followed lock symlink")
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("modified symlink target")
	}
}

func TestLatestExcludesChildSessions(t *testing.T) {
	store := &Store{Directory: t.TempDir()}
	defer store.Close()
	parent := New("/workspace", "codex", "model")
	child := New("/workspace", "codex", "model")
	child.ParentID = parent.ID
	if err := store.Save(parent); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(child); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Latest("/workspace")
	if err != nil || latest.ID != parent.ID {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	loaded, err := store.Load(child.ID)
	if err != nil || loaded.ParentID != parent.ID {
		t.Fatal("child relationship lost", err)
	}
}
