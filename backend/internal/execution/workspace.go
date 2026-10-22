package execution

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var errSymlink = errors.New("workspace symlinks are not allowed")

// SafeRelative rejects absolute paths, parent traversal, and empty names.
// It does not touch the filesystem.
func SafeRelative(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || strings.ContainsAny(path, `\:`) {
		return "", invalidInput("workspace path is empty or contains a forbidden character")
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
		return "", scopeDenied("workspace path must be relative")
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", scopeDenied("workspace path escapes the work item directory")
	}
	rel := filepath.ToSlash(clean)
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return "", scopeDenied("workspace path escapes the work item directory")
	}
	return rel, nil
}

func rejectSymlinkPath(root *os.Root, rel string, allowMissingFinal bool) error {
	probe := ""
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return scopeDenied("workspace path escapes the work item directory")
		}
		if probe == "" {
			probe = part
		} else {
			probe = probe + "/" + part
		}
		info, err := root.Lstat(probe)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && allowMissingFinal && i == len(parts)-1 {
				return nil
			}
			if errors.Is(err, os.ErrNotExist) && i < len(parts)-1 {
				return nil
			}
			if errors.Is(err, os.ErrNotExist) {
				return err
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return scopeDenied("workspace symlinks are not allowed")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return invalidInput("workspace parent is not a directory")
		}
	}
	return nil
}

func ensureParents(root *os.Root, rel string) error {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." || dir == "" {
		return nil
	}
	probe := ""
	for _, part := range strings.Split(dir, "/") {
		if probe == "" {
			probe = part
		} else {
			probe = probe + "/" + part
		}
		info, err := root.Lstat(probe)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(probe, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return scopeDenied("workspace symlinks are not allowed")
		}
		if !info.IsDir() {
			return invalidInput("workspace parent is not a directory")
		}
	}
	return nil
}

// WriteWithin writes content under root. Existing symlink components are rejected
// and the destination outside the root is left untouched.
func WriteWithin(rootDir, rel string, content []byte) error {
	safe, err := SafeRelative(rel)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := rejectSymlinkPath(root, safe, true); err != nil {
		return err
	}
	if err := ensureParents(root, safe); err != nil {
		return err
	}
	f, err := root.OpenFile(safe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0644)
	if err != nil {
		if errors.Is(err, os.ErrExist) || isFollow(err) {
			return scopeDenied("workspace symlinks are not allowed")
		}
		return err
	}
	defer f.Close()
	_, err = f.Write(content)
	return err
}

// ReadWithin reads a file that stays inside root. Symlinks are not followed.
func ReadWithin(rootDir, rel string, offset, limit int) ([]byte, error) {
	safe, err := SafeRelative(rel)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := rejectSymlinkPath(root, safe, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFound("workspace file does not exist")
		}
		return nil, err
	}
	f, err := root.OpenFile(safe, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFound("workspace file does not exist")
		}
		if isFollow(err) {
			return nil, scopeDenied("workspace symlinks are not allowed")
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, invalidInput("workspace path is a directory")
	}
	if offset < 0 {
		return nil, invalidInput("offset must be non-negative")
	}
	if limit <= 0 || limit > 32768 {
		limit = 8192
	}
	if offset > 0 {
		if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, limit)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// ApproveMount rejects a workspace path that resolves outside root or through a symlink.
func ApproveMount(root, path string) error {
	if root == "" || path == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return unavailable("execution workspace is not ready")
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return unavailable("execution workspace is not ready")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return unavailable("execution workspace is not ready")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return scopeDenied("execution workspace must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return scopeDenied("execution workspace must be a real directory")
	}
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return scopeDenied("execution workspace escapes the server root")
	}
	if strings.Contains(resolved, "docker.sock") || strings.HasSuffix(resolved, ".env") {
		return scopeDenied("execution mount is not allowed")
	}
	return nil
}

func isFollow(err error) bool {
	return err != nil && (errors.Is(err, errSymlink) || strings.Contains(strings.ToLower(err.Error()), "symlink"))
}
