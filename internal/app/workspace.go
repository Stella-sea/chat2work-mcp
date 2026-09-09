package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Workspace struct{ base string }

func NewWorkspace(base string) (*Workspace, error) {
	absolute, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	return &Workspace{base: absolute}, nil
}

func (w *Workspace) Root(tenant string) (string, error) {
	if tenant == "" || strings.ContainsAny(tenant, `/\\`) {
		return "", errors.New("invalid tenant")
	}
	root := filepath.Join(w.base, tenant)
	if err := os.MkdirAll(root, 0750); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	return root, nil
}

func resolve(root, supplied string, allowMissing bool) (string, error) {
	if supplied == "" {
		supplied = "."
	}
	if filepath.IsAbs(supplied) || strings.HasPrefix(supplied, "/") || strings.HasPrefix(supplied, `\`) {
		return "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(supplied)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace")
	}
	full := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace")
	}
	if allowMissing {
		if info, err := os.Lstat(full); err == nil {
			if info.Mode()&fs.ModeSymlink != 0 {
				return "", errors.New("symlink paths are not allowed")
			}
			real, err := filepath.EvalSymlinks(full)
			if err != nil {
				return "", err
			}
			rel, err = filepath.Rel(root, real)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", errors.New("symlink escapes the workspace")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := checkExistingParents(root, full); err != nil {
			return "", err
		}
		return full, nil
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	rel, err = filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("symlink escapes the workspace")
	}
	return real, nil
}

func checkExistingParents(root, path string) error {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("symlink paths are not allowed")
		}
		if current == root {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
}
