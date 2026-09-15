package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Files struct {
	workspace         *Workspace
	maxBytes          int64
	maxWorkspaceBytes int64
	maxWorkspaceFiles int
	deleteEnabled     bool
}

// errLimitReached stops a directory walk once a configured cap is hit. It never
// leaves the package.
var errLimitReached = errors.New("walk limit reached")

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return err
	}
	return target.Close()
}

func (f Files) List(tenant, path string, recursive bool, limit int) ([]string, bool, error) {
	root, err := f.workspace.Root(tenant)
	if err != nil {
		return nil, false, err
	}
	target, err := resolve(root, path, false)
	if err != nil {
		return nil, false, friendlyPathError(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, false, friendlyPathError(err)
	}
	if !info.IsDir() {
		return nil, false, errors.New("path is not a directory")
	}
	var items []string
	truncated := false
	if recursive {
		err = filepath.WalkDir(target, func(item string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if item == target {
				return nil
			}
			rel, _ := filepath.Rel(root, item)
			items = append(items, filepath.ToSlash(rel))
			if limit > 0 && len(items) >= limit {
				truncated = true
				return errLimitReached
			}
			return nil
		})
		if errors.Is(err, errLimitReached) {
			err = nil
		}
	} else {
		entries, listErr := os.ReadDir(target)
		err = listErr
		for _, entry := range entries {
			rel, _ := filepath.Rel(root, filepath.Join(target, entry.Name()))
			name := filepath.ToSlash(rel)
			if entry.IsDir() {
				name += "/"
			}
			items = append(items, name)
			if limit > 0 && len(items) >= limit {
				truncated = true
				break
			}
		}
	}
	return items, truncated, err
}

func (f Files) Read(tenant, path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > f.maxBytes {
		maxBytes = f.maxBytes
	}
	root, err := f.workspace.Root(tenant)
	if err != nil {
		return nil, err
	}
	target, err := resolve(root, path, false)
	if err != nil {
		return nil, friendlyPathError(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, friendlyPathError(err)
	}
	if info.IsDir() {
		return nil, errors.New("path is a directory")
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("file exceeds read limit of %d bytes", maxBytes)
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, friendlyPathError(err)
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maxBytes+1))
}

func (f Files) Write(tenant, path, content string, overwrite bool) error {
	if int64(len(content)) > f.maxBytes {
		return fmt.Errorf("content exceeds write limit of %d bytes", f.maxBytes)
	}
	root, err := f.workspace.Root(tenant)
	if err != nil {
		return err
	}
	target, err := resolve(root, path, true)
	if err != nil {
		return friendlyPathError(err)
	}
	if !overwrite {
		if _, err := os.Lstat(target); err == nil {
			return errors.New("file already exists; set overwrite to replace it")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	var existingSize int64
	isNew := true
	if info, statErr := os.Stat(target); statErr == nil {
		existingSize = info.Size()
		isNew = false
	}
	if err := f.enforceQuota(root, int64(len(content)), existingSize, isNew); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".chat2work-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, strings.NewReader(content)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0640); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// enforceQuota checks the per-workspace byte and file-count caps before a write.
// Usage is measured with an early-abort walk, so it stays bounded even when the
// workspace is already over quota.
func (f Files) enforceQuota(root string, newSize, existingSize int64, isNew bool) error {
	if f.maxWorkspaceBytes <= 0 && f.maxWorkspaceFiles <= 0 {
		return nil
	}
	used, count, err := f.usage(root)
	if err != nil {
		return err
	}
	if f.maxWorkspaceBytes > 0 && used-existingSize+newSize > f.maxWorkspaceBytes {
		return fmt.Errorf("workspace storage quota exceeded (%d bytes)", f.maxWorkspaceBytes)
	}
	if isNew && f.maxWorkspaceFiles > 0 && count >= f.maxWorkspaceFiles {
		return fmt.Errorf("workspace file count quota exceeded (%d files)", f.maxWorkspaceFiles)
	}
	return nil
}

// CheckWriteAllowed applies a coarse pre-check for writes whose final size is
// unknown (for example OfficeCLI-created documents): reject when the workspace
// is already over the byte cap, and reject a new file when the count cap is
// reached.
func (f Files) CheckWriteAllowed(tenant string, isNew bool) error {
	if f.maxWorkspaceBytes <= 0 && f.maxWorkspaceFiles <= 0 {
		return nil
	}
	root, err := f.workspace.Root(tenant)
	if err != nil {
		return err
	}
	used, count, err := f.usage(root)
	if err != nil {
		return err
	}
	if f.maxWorkspaceBytes > 0 && used > f.maxWorkspaceBytes {
		return fmt.Errorf("workspace storage quota exceeded (%d bytes)", f.maxWorkspaceBytes)
	}
	if isNew && f.maxWorkspaceFiles > 0 && count >= f.maxWorkspaceFiles {
		return fmt.Errorf("workspace file count quota exceeded (%d files)", f.maxWorkspaceFiles)
	}
	return nil
}

// CheckOfficeOutputAllowed verifies the quota using the completed OfficeCLI
// output while excluding its transient sibling copy from workspace usage.
func (f Files) CheckOfficeOutputAllowed(tenant, existingPath, outputPath string, isNew bool) error {
	if f.maxWorkspaceBytes <= 0 && f.maxWorkspaceFiles <= 0 {
		return nil
	}
	root, err := f.workspace.Root(tenant)
	if err != nil {
		return err
	}
	output, err := os.Stat(outputPath)
	if err != nil {
		return err
	}
	var existingSize int64
	if !isNew {
		existing, err := os.Stat(existingPath)
		if err != nil {
			return err
		}
		existingSize = existing.Size()
	}
	used, count, err := usageExactExcluding(root, outputPath)
	if err != nil {
		return err
	}
	if f.maxWorkspaceBytes > 0 && used-existingSize+output.Size() > f.maxWorkspaceBytes {
		return fmt.Errorf("workspace storage quota exceeded (%d bytes)", f.maxWorkspaceBytes)
	}
	if isNew && f.maxWorkspaceFiles > 0 && count+1 > f.maxWorkspaceFiles {
		return fmt.Errorf("workspace file count quota exceeded (%d files)", f.maxWorkspaceFiles)
	}
	return nil
}

func (f Files) usage(root string) (int64, int, error) {
	return f.usageExcluding(root, "")
}

func (f Files) usageExcluding(root, excluded string) (int64, int, error) {
	var bytes int64
	var count int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == excluded {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		bytes += info.Size()
		count++
		if f.maxWorkspaceBytes > 0 && bytes > f.maxWorkspaceBytes {
			return errLimitReached
		}
		if f.maxWorkspaceFiles > 0 && count > f.maxWorkspaceFiles {
			return errLimitReached
		}
		return nil
	})
	if errors.Is(err, errLimitReached) {
		return bytes, count, nil
	}
	return bytes, count, err
}

// usageExactExcluding deliberately scans the complete workspace. Replacement
// quota checks must account for the old file even when the workspace is
// already over a configured cap, so the early-abort scan is not sufficient.
func usageExactExcluding(root, excluded string) (int64, int, error) {
	var bytes int64
	var count int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == excluded || d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		bytes += info.Size()
		count++
		return nil
	})
	return bytes, count, err
}

func (f Files) Edit(tenant, path, oldText, newText string) error {
	if oldText == "" {
		return errors.New("old_text must not be empty")
	}
	data, err := f.Read(tenant, path, f.maxBytes)
	if err != nil {
		return err
	}
	count := bytes.Count(data, []byte(oldText))
	if count == 0 {
		return errors.New("old_text was not found")
	}
	if count > 1 {
		return errors.New("old_text matches multiple locations; provide more context")
	}
	return f.Write(tenant, path, strings.Replace(string(data), oldText, newText, 1), true)
}

func (f Files) Move(tenant, src, dst string) error {
	root, err := f.workspace.Root(tenant)
	if err != nil {
		return err
	}
	source, err := resolve(root, src, false)
	if err != nil {
		return friendlyPathError(err)
	}
	destination, err := resolve(root, dst, true)
	if err != nil {
		return friendlyPathError(err)
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0750); err != nil {
		return err
	}
	return os.Rename(source, destination)
}

func (f Files) Delete(tenant, path string, confirm bool) error {
	if !f.deleteEnabled {
		return errors.New("deletion is disabled by the server administrator")
	}
	if !confirm {
		return errors.New("set confirm to true to delete a file")
	}
	root, err := f.workspace.Root(tenant)
	if err != nil {
		return err
	}
	target, err := resolve(root, path, false)
	if err != nil {
		return friendlyPathError(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return friendlyPathError(err)
	}
	if info.IsDir() {
		return errors.New("directory deletion is not supported")
	}
	return os.Remove(target)
}

func (f Files) Search(tenant, pattern, query, path string, limit int) ([]string, bool, error) {
	root, err := f.workspace.Root(tenant)
	if err != nil {
		return nil, false, err
	}
	target, err := resolve(root, path, false)
	if err != nil {
		return nil, false, friendlyPathError(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, false, friendlyPathError(err)
	}
	var matches []string
	truncated := false
	visit := func(item string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, item)
		ok, matchErr := filepath.Match(pattern, filepath.Base(item))
		if matchErr != nil {
			return fmt.Errorf("invalid glob pattern: %w", matchErr)
		}
		if !ok {
			return nil
		}
		if query != "" {
			info, infoErr := d.Info()
			if infoErr != nil || info.Size() > f.maxBytes {
				return nil
			}
			data, readErr := os.ReadFile(item)
			if readErr != nil || !strings.Contains(string(data), query) {
				return nil
			}
		}
		matches = append(matches, filepath.ToSlash(rel))
		if limit > 0 && len(matches) >= limit {
			truncated = true
			return errLimitReached
		}
		return nil
	}
	if info.IsDir() {
		err = filepath.WalkDir(target, visit)
	} else {
		err = visit(target, fileEntry{info}, nil)
	}
	if errors.Is(err, errLimitReached) {
		err = nil
	}
	return matches, truncated, err
}

type fileEntry struct{ fs.FileInfo }

func (fileEntry) Type() fs.FileMode            { return 0 }
func (e fileEntry) Info() (fs.FileInfo, error) { return e.FileInfo, nil }

func friendlyPathError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("file or directory not found")
	}
	return err
}
