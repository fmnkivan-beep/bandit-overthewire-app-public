//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	progressKernel32    = syscall.NewLazyDLL("kernel32.dll")
	createProgressMutex = progressKernel32.NewProc("CreateMutexW")
	waitProgressMutex   = progressKernel32.NewProc("WaitForSingleObject")
	releaseProgressLock = progressKernel32.NewProc("ReleaseMutex")
	closeProgressHandle = progressKernel32.NewProc("CloseHandle")
	replaceProgressFile = progressKernel32.NewProc("MoveFileExW")
)

func withStorageLock(dir string, run func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hash := sha256.Sum256([]byte(dir))
	name, err := syscall.UTF16PtrFromString(`Local\Bandit2-` + hex.EncodeToString(hash[:12]))
	if err != nil {
		return err
	}
	handle, _, callErr := createProgressMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return callErr
	}
	defer closeProgressHandle.Call(handle)
	result, _, waitErr := waitProgressMutex.Call(handle, 0xffffffff)
	if result != 0 && result != 0x80 {
		return waitErr
	}
	defer releaseProgressLock.Call(handle)
	return run()
}

func replaceStoredFile(source, destination string) error {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	result, _, callErr := replaceProgressFile.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), 0x1|0x8)
	if result == 0 {
		return callErr
	}
	return nil
}
