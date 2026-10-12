package tools

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSeedModuleCopiesWithoutSharedWritesOrSymlinks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	module := "example.test/pkg@v1.0.0"
	os.MkdirAll(filepath.Join(src, module), 0700)
	sourceFile := filepath.Join(src, module, "code.go")
	os.WriteFile(sourceFile, []byte("original"), 0644)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("secret"), 0600)
	os.Symlink(secret, filepath.Join(src, module, "link"))
	source, _ := os.OpenRoot(src)
	defer source.Close()
	destination, _ := os.OpenRoot(dst)
	defer destination.Close()
	if err := seedModule(context.Background(), source, destination, "example.test/pkg", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dst, module, "code.go")
	if err := os.WriteFile(target, []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(sourceFile)
	if string(got) != "original" {
		t.Fatal("workspace cache poisoned installed module", string(got))
	}
	a, _ := os.Stat(sourceFile)
	b, _ := os.Stat(target)
	if os.SameFile(a, b) {
		t.Fatal("cache uses shared inode")
	}
	if _, err := os.Lstat(filepath.Join(dst, module, "link")); !os.IsNotExist(err) {
		t.Fatal("module symlink copied")
	}
}
func TestCacheRootSwapRejectedEvenAfterGoModRemoval(t *testing.T) {
	for _, removeMod := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged module", true: "removed module"}[removeMod], func(t *testing.T) {
			e, dir := fixture(t, "auto", nil)
			data := []byte("module example.test/test\n")
			os.WriteFile(filepath.Join(dir, "go.mod"), data, 0600)
			base := t.TempDir()
			os.Mkdir(filepath.Join(base, "go-build"), 0700)
			os.Mkdir(filepath.Join(base, "go-mod"), 0700)
			e.goEnv = &goEnvironment{Binary: "unused", CacheRoot: base, HostModuleCache: t.TempDir(), Seeded: true, ModuleHash: sha256.Sum256(data)}
			if removeMod {
				os.Remove(filepath.Join(dir, "go.mod"))
			}
			os.Remove(filepath.Join(base, "go-build"))
			os.Symlink(t.TempDir(), filepath.Join(base, "go-build"))
			if _, err := e.EnvironmentInfo(context.Background()); err == nil {
				t.Fatal("reused swapped cache root")
			}
		})
	}
}
func TestModuleCacheDestinationCannotEscape(t *testing.T) {
	src, dst, outside := t.TempDir(), t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(src, "example.test/pkg@v1.0.0"), 0700)
	os.WriteFile(filepath.Join(src, "example.test/pkg@v1.0.0/code.go"), []byte("code"), 0600)
	os.Symlink(outside, filepath.Join(dst, "example.test"))
	source, _ := os.OpenRoot(src)
	defer source.Close()
	destination, _ := os.OpenRoot(dst)
	defer destination.Close()
	if err := seedModule(context.Background(), source, destination, "example.test/pkg", "v1.0.0"); err == nil {
		t.Fatal("accepted outside cache symlink")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote outside isolated cache")
	}
}
func TestInstalledAndWorkspaceCacheMustNotOverlap(t *testing.T) {
	for _, pair := range [][2]string{{"/a", "/a"}, {"/a", "/a/b"}, {"/a/b", "/a"}} {
		if !cachePathsOverlap(pair[0], pair[1]) {
			t.Fatal("missed overlap", pair)
		}
	}
	if cachePathsOverlap("/a", "/abc") {
		t.Fatal("unrelated roots treated as overlap")
	}
}
func TestInvalidModuleCachePath(t *testing.T) {
	for _, path := range []string{"../outside", "/absolute", "example.test/../outside"} {
		if _, err := moduleCacheComponent(path); err == nil {
			t.Fatal("accepted traversal", path)
		}
	}
}
func TestGoCachesPersistAndRemainWorkspaceSpecific(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e, dir := fixture(t, "auto", nil)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/cache\n\ngo 1.27.0\n"), 0600)
	first, err := e.EnvironmentInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondExecutor, err := New(Config{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer secondExecutor.Close()
	second, err := secondExecutor.EnvironmentInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.GoRoot == "" || first.GoBuildCache == "" || first != second {
		t.Fatal("cache paths not stable", first, second)
	}
	other, otherDir := fixture(t, "auto", nil)
	os.WriteFile(filepath.Join(otherDir, "go.mod"), []byte("module example.test/other\n\ngo 1.27.0\n"), 0600)
	third, err := other.EnvironmentInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.GoBuildCache == first.GoBuildCache || third.GoModuleCache == first.GoModuleCache {
		t.Fatal("workspaces share writable cache")
	}
}

// A GOROOT is granted to the sandbox only when it holds the toolchain that
// reported it: GOROOT=/ in the host environment must not open the disk.
func TestInstalledGoRootMustHoldTheToolchain(t *testing.T) {
	binary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain")
	}
	if binary, err = filepath.EvalSymlinks(binary); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(binary))
	if got, err := installedGoRoot(binary, root); err != nil || got != root {
		t.Fatalf("installedGoRoot(%s) = %q, %v", root, got, err)
	}
	for _, bad := range []string{"/", "", "relative/go", t.TempDir(), filepath.Join(root, "bin")} {
		if got, err := installedGoRoot(binary, bad); err == nil {
			t.Errorf("installedGoRoot(%q) accepted %q", bad, got)
		}
	}
}
