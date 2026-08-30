//go:build !windows

package main

import "errors"

func ensureAtomicReplaceSupported() error {
	return errors.New("atomic apply is supported only on Windows; plan mode remains available")
}

func replaceFileWithBackup(_, _, _, _ string) error {
	return ensureAtomicReplaceSupported()
}
