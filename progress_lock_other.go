//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"sync"
)

var storageLocks sync.Map

func withStorageLock(dir string, run func() error) error {
	lock, _ := storageLocks.LoadOrStore(filepath.Clean(dir), &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	return run()
}

func replaceStoredFile(source, destination string) error {
	return os.Rename(source, destination)
}
