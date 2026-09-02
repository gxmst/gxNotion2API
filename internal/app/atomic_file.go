package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var fileWriteLocks sync.Map

func canonicalFileLockKey(path string) string {
	clean := filepath.Clean(strings.TrimSpace(path))
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	return clean
}

func fileWriteLock(path string) *sync.Mutex {
	key := canonicalFileLockKey(path)
	lock, _ := fileWriteLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func withFileWriteLock(path string, fn func(clean string) error) error {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return fmt.Errorf("empty path")
	}
	clean = filepath.Clean(clean)
	lock := fileWriteLock(clean)
	lock.Lock()
	defer lock.Unlock()
	return fn(clean)
}

func writeFileAtomically(path string, body []byte, mode os.FileMode) error {
	return withFileWriteLock(path, func(clean string) error {
		return writeFileAtomicallyLocked(clean, body, mode)
	})
}

func writeFileAtomicallyLocked(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	dirMode := os.FileMode(0o755)
	if mode.Perm()&0o077 == 0 {
		dirMode = 0o700
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keepTemp := true
	defer func() {
		_ = tmp.Close()
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if !errors.Is(err, syscall.EBUSY) {
			return err
		}
		// A Docker bind-mounted file is a mount point and cannot be replaced
		// with rename(2). Keep the normal atomic path everywhere else, but
		// synchronously overwrite this one target so admin saves still work.
		if err := overwriteMountedFile(path, body, mode); err != nil {
			return err
		}
		return nil
	}
	keepTemp = false
	return os.Chmod(path, mode)
}

func overwriteMountedFile(path string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
