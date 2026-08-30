//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func ensureAtomicReplaceSupported() error {
	return nil
}

func replaceFileWithBackup(targetPath, replacementPath, backupPath, expectedTargetHash string) error {
	target, errTarget := windows.UTF16PtrFromString(targetPath)
	if errTarget != nil {
		return errTarget
	}
	attributes, errAttributes := windows.GetFileAttributes(target)
	if errAttributes != nil {
		return errAttributes
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("config path must not be a reparse point")
	}

	targetHandle, errOpen := windows.CreateFile(
		target,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_SEQUENTIAL_SCAN,
		0,
	)
	if errOpen != nil {
		return fmt.Errorf("open config for atomic verification: %w", errOpen)
	}
	lockedTarget := os.NewFile(uintptr(targetHandle), targetPath)
	if lockedTarget == nil {
		_ = windows.CloseHandle(targetHandle)
		return fmt.Errorf("open config for atomic verification")
	}
	defer func() { _ = lockedTarget.Close() }()
	digest := sha256.New()
	if _, errHash := io.Copy(digest, lockedTarget); errHash != nil {
		return fmt.Errorf("hash config at atomic boundary: %w", errHash)
	}
	actualTargetHash := hex.EncodeToString(digest.Sum(nil))
	if actualTargetHash != expectedTargetHash {
		return fmt.Errorf("source SHA-256 mismatch at atomic boundary: expected %s, found %s", expectedTargetHash, actualTargetHash)
	}

	replacement, errReplacement := windows.UTF16PtrFromString(replacementPath)
	if errReplacement != nil {
		return errReplacement
	}
	backup, errBackup := windows.UTF16PtrFromString(backupPath)
	if errBackup != nil {
		return errBackup
	}
	result, _, errCall := replaceFileW.Call(
		uintptr(unsafe.Pointer(target)),
		uintptr(unsafe.Pointer(replacement)),
		uintptr(unsafe.Pointer(backup)),
		0,
		0,
		0,
	)
	if result == 0 {
		if errCall == syscall.Errno(0) {
			return fmt.Errorf("ReplaceFileW failed")
		}
		return errCall
	}
	return nil
}
