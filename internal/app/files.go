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
	workspace     *Workspace
	maxBytes      int64
	deleteEnabled bool
}

func (f Files) List(tenant, path string, recursive bool) ([]string, error) {
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
	if !info.IsDir() {
		return nil, errors.New("path is not a directory")
	}
	var items []string
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
			return nil
		})
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
		}
	}
	return items, err
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

func (f Files) Search(tenant, pattern, query, path string) ([]string, error) {
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
	var matches []string
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
		return nil
	}
	if info.IsDir() {
		err = filepath.WalkDir(target, visit)
	} else {
		err = visit(target, fileEntry{info}, nil)
	}
	return matches, err
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
