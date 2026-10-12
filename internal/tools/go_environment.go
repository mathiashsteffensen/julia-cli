package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// EnvironmentInfo describes host-measured Go paths and workspace-isolated caches
// used by sandboxed commands. Host credentials and the host's writable caches are
// not passed to sandboxed commands.
type EnvironmentInfo struct {
	GoRoot        string `json:"go_root,omitempty"`
	GoBuildCache  string `json:"go_build_cache,omitempty"`
	GoModuleCache string `json:"go_module_cache,omitempty"`
}
type goEnvironment struct {
	Info                               EnvironmentInfo
	Binary, HostModuleCache, CacheRoot string
	ModuleHash                         [32]byte
	Seeded                             bool
}

// EnvironmentInfo returns the effective Go environment without fetching modules.
func (e *Executor) EnvironmentInfo(ctx context.Context) (EnvironmentInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	env, err := e.prepareGoEnvironment(ctx)
	if err != nil {
		return EnvironmentInfo{}, err
	}
	return env.Info, nil
}

// discoverGo locates the installed Go toolchain once and measures its GOROOT and
// host module cache. It does no workspace cache work, so escalated commands,
// which use the host's own caches, need nothing more than this.
func (e *Executor) discoverGo(ctx context.Context) (*goEnvironment, error) {
	if e.goEnv == nil {
		env := &goEnvironment{}
		binary, err := exec.LookPath("go")
		if err != nil {
			e.goEnv = env
			return env, nil
		}
		binary, err = filepath.EvalSymlinks(binary)
		if err != nil {
			return nil, fmt.Errorf("resolve Go executable: %w", err)
		}
		rel, _ := filepath.Rel(e.directory, binary)
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("Go executable must be installed outside the workspace")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(queryCtx, binary, "env", "-json", "GOROOT", "GOMODCACHE")
		cmd.Dir = os.TempDir()
		cmd.Env = []string{"HOME=" + home, "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off"}
		// GOROOT is left to the toolchain, which finds its own root from its
		// executable: a host GOROOT would choose what the sandbox may read.
		for _, key := range []string{"GOPATH", "GOMODCACHE"} {
			if value := os.Getenv(key); value != "" {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("discover installed Go environment: %w", err)
		}
		var measured struct{ GOROOT, GOMODCACHE string }
		if err = json.Unmarshal(output, &measured); err != nil {
			return nil, fmt.Errorf("decode Go environment: %w", err)
		}
		root, err := installedGoRoot(binary, measured.GOROOT)
		if err != nil {
			return nil, err
		}
		env.Binary = binary
		env.Info.GoRoot = root
		env.HostModuleCache = measured.GOMODCACHE
		e.goEnv = env
	}
	return e.goEnv, nil
}

// installedGoRoot checks a reported GOROOT against the toolchain itself: it is
// granted to sandboxed commands, so it must be the directory whose bin/go is
// the executable that reported it, and never the filesystem root.
func installedGoRoot(binary, reported string) (string, error) {
	if reported == "" || !filepath.IsAbs(reported) {
		return "", fmt.Errorf("Go reported GOROOT %q, not an absolute path", reported)
	}
	root, err := filepath.EvalSymlinks(reported)
	if err != nil {
		return "", fmt.Errorf("resolve GOROOT: %w", err)
	}
	if root == filepath.Dir(root) {
		return "", fmt.Errorf("Go reported GOROOT %q, the filesystem root", reported)
	}
	tool, err := filepath.EvalSymlinks(filepath.Join(root, "bin", "go"))
	if err != nil || tool != binary {
		return "", fmt.Errorf("GOROOT %q does not hold the installed Go executable %s", reported, binary)
	}
	return root, nil
}

func (e *Executor) prepareGoEnvironment(ctx context.Context) (*goEnvironment, error) {
	env, err := e.discoverGo(ctx)
	if err != nil {
		return nil, err
	}
	if env.CacheRoot != "" {
		if err := validateGoCacheRoots(env); err != nil {
			return nil, err
		}
	}
	if env.Binary == "" {
		return env, nil
	}
	moduleFile, _, err := e.readFile("go.mod")
	if errors.Is(err, os.ErrNotExist) {
		return env, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read workspace go.mod: %w", err)
	}
	hash := sha256.Sum256(moduleFile)
	if env.Seeded && hash == env.ModuleHash {
		return env, nil
	}
	if env.CacheRoot == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		id := sha256.Sum256([]byte(e.directory))
		env.CacheRoot = filepath.Join(base, "jarvis", "workspaces", hex.EncodeToString(id[:16]))
		if err := privateCacheDirectory(env.CacheRoot); err != nil {
			return nil, err
		}
		env.CacheRoot, err = filepath.EvalSymlinks(env.CacheRoot)
		if err != nil {
			return nil, err
		}
		env.Info.GoBuildCache = filepath.Join(env.CacheRoot, "go-build")
		env.Info.GoModuleCache = filepath.Join(env.CacheRoot, "go-mod")
		for _, dir := range []string{env.Info.GoBuildCache, env.Info.GoModuleCache} {
			if err := privateCacheDirectory(dir); err != nil {
				return nil, err
			}
		}
	}
	if err := validateGoCacheRoots(env); err != nil {
		return nil, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(queryCtx, env.Binary, "mod", "edit", "-json", filepath.Join(e.directory, "go.mod"))
	cmd.Dir = e.directory
	cmd.Env = []string{"HOME=" + os.TempDir(), "GOROOT=" + env.Info.GoRoot, "GOTOOLCHAIN=local", "GOENV=off", "GOPROXY=off", "GOSUMDB=off"}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect workspace module dependencies: %w", err)
	}
	type module struct{ Path, Version string }
	var graph struct {
		Require []module
		Replace []struct{ Old, New module }
	}
	if err := json.Unmarshal(output, &graph); err != nil {
		return nil, err
	}
	modules := append([]module(nil), graph.Require...)
	for _, replace := range graph.Replace {
		if replace.New.Version != "" {
			modules = append(modules, replace.New)
		}
	}
	destination, err := openCacheRoot(env.CacheRoot, "go-mod")
	if err != nil {
		return nil, err
	}
	defer destination.Close()
	source, err := os.OpenRoot(env.HostModuleCache)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("open installed module cache: %w", err)
		}
	} else {
		defer source.Close()
		for _, module := range modules {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			path, err := moduleCacheComponent(module.Path)
			if err != nil {
				return nil, err
			}
			version, err := moduleCacheComponent(module.Version)
			if err != nil {
				return nil, err
			}
			if version == "" {
				continue
			}
			if err := seedModule(ctx, source, destination, path, version); err != nil {
				return nil, err
			}
		}
	}
	env.ModuleHash = hash
	env.Seeded = true
	return env, nil
}
func privateCacheDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create isolated Go cache: %w", err)
	}
	directory, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("isolated Go cache must be a real directory: %w", err)
	}
	defer directory.Close()
	return directory.Chmod(0700)
}
func validateGoCacheRoots(env *goEnvironment) error {
	source, err := filepath.Abs(env.HostModuleCache)
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(source); err == nil {
		source = real
	} else if !os.IsNotExist(err) {
		return err
	}
	if cachePathsOverlap(source, env.CacheRoot) {
		return errors.New("installed module cache overlaps the isolated workspace cache")
	}
	for _, name := range []string{"go-build", "go-mod"} {
		root, err := openCacheRoot(env.CacheRoot, name)
		if err != nil {
			return fmt.Errorf("isolated Go cache root changed or is unavailable: %w", err)
		}
		root.Close()
	}
	return nil
}
func cachePathsOverlap(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
func moduleCacheComponent(value string) (string, error) {
	if filepath.IsAbs(value) || strings.ContainsAny(value, "\\\x00") {
		return "", errors.New("invalid module cache path")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." {
			return "", errors.New("invalid module path traversal")
		}
	}
	var escaped strings.Builder
	for _, c := range value {
		if c >= 'A' && c <= 'Z' {
			escaped.WriteByte('!')
			escaped.WriteRune(c - 'A' + 'a')
		} else {
			escaped.WriteRune(c)
		}
	}
	return escaped.String(), nil
}
func openCacheRoot(base, child string) (*os.Root, error) {
	parent, err := os.OpenFile(base, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), child, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return os.OpenRoot(fmt.Sprintf("/dev/fd/%d", fd))
}
func seedModule(ctx context.Context, source, destination *os.Root, path, version string) error {
	module := path + "@" + version
	if info, err := destination.Lstat(module); err != nil && !os.IsNotExist(err) {
		return err
	} else if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("isolated module directory must not be a symlink")
	}
	if _, err := destination.Lstat(module); os.IsNotExist(err) {
		if info, err := source.Lstat(module); err == nil && info.IsDir() {
			nonce := make([]byte, 12)
			if _, err := rand.Read(nonce); err != nil {
				return err
			}
			temp := ".seed-" + hex.EncodeToString(nonce)
			if err := destination.Mkdir(temp, 0700); err != nil {
				return err
			}
			defer destination.RemoveAll(temp)
			err = fs.WalkDir(source.FS(), module, func(name string, d fs.DirEntry, walkErr error) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if walkErr != nil {
					return walkErr
				}
				rel, err := filepath.Rel(module, name)
				if err != nil {
					return err
				}
				out := filepath.Join(temp, rel)
				if d.Type()&os.ModeSymlink != 0 {
					return nil
				}
				if d.IsDir() {
					return destination.MkdirAll(out, 0700)
				}
				if !d.Type().IsRegular() {
					return nil
				}
				return copyCacheFile(ctx, source, name, destination, out)
			})
			if err != nil {
				return fmt.Errorf("seed Go module %s: %w", module, err)
			}
			if err := destination.MkdirAll(filepath.Dir(module), 0700); err != nil {
				return err
			}
			if err := destination.Rename(temp, module); err != nil {
				if info, statErr := destination.Stat(module); statErr != nil || !info.IsDir() {
					return err
				}
			}
		}
	}
	for _, suffix := range []string{".mod", ".info", ".ziphash"} {
		name := filepath.ToSlash(filepath.Join("cache/download", path, "@v", version+suffix))
		if _, err := destination.Lstat(name); err == nil {
			continue
		}
		if info, err := source.Lstat(name); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := destination.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		temp := filepath.Join(filepath.Dir(name), ".seed-"+hex.EncodeToString(nonce))
		if err := copyCacheFile(ctx, source, name, destination, temp); err != nil {
			destination.Remove(temp)
			return err
		}
		if err := destination.Rename(temp, name); err != nil {
			destination.Remove(temp)
			return err
		}
	}
	return nil
}
func cacheParent(root *os.Root, name string) (*os.File, string, error) {
	current, err := root.Open(".")
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(name)), "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "." || part == ".." {
			current.Close()
			return nil, "", errors.New("invalid cache component")
		}
		fd, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		current.Close()
		if err != nil {
			return nil, "", err
		}
		current = os.NewFile(uintptr(fd), part)
	}
	return current, parts[len(parts)-1], nil
}
func copyCacheFile(ctx context.Context, source *os.Root, name string, destination *os.Root, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := source.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("module cache source is not a regular file")
	}
	parent, base, err := cacheParent(destination, target)
	if err != nil {
		return err
	}
	defer parent.Close()
	if cloned, err := cloneCacheFile(f, parent, base); cloned || err != nil {
		return err
	}
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(info.Mode().Perm()))
	if err != nil {
		return err
	}
	out := os.NewFile(uintptr(fd), base)
	_, copyErr := io.Copy(out, contextCacheReader{ctx: ctx, reader: f})
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

type contextCacheReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextCacheReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
