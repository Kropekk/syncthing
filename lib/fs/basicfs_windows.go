// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// +build windows

package fs

import (
	"bytes"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var errNotSupported = errors.New("symlinks not supported")

func (BasicFilesystem) SymlinksSupported() bool {
	return false
}

func (BasicFilesystem) ReadSymlink(path string) (string, error) {
	return "", errNotSupported
}

func (BasicFilesystem) CreateSymlink(target, name string) error {
	return errNotSupported
}

// Required due to https://github.com/golang/go/issues/10900
func (f *BasicFilesystem) mkdirAll(path string, perm os.FileMode) error {
	// Fast path: if we can tell whether path is a directory or file, stop with success or error.
	dir, err := os.Stat(path)
	if err == nil {
		if dir.IsDir() {
			return nil
		}
		return &os.PathError{
			Op:   "mkdir",
			Path: path,
			Err:  syscall.ENOTDIR,
		}
	}

	// Slow path: make sure parent exists and then call Mkdir for path.
	i := len(path)
	for i > 0 && IsPathSeparator(path[i-1]) { // Skip trailing path separator.
		i--
	}

	j := i
	for j > 0 && !IsPathSeparator(path[j-1]) { // Scan backward over element.
		j--
	}

	if j > 1 {
		// Create parent
		parent := path[0 : j-1]
		if parent != filepath.VolumeName(parent) {
			err = f.mkdirAll(parent, perm)
			if err != nil {
				return err
			}
		}
	}

	// Parent now exists; invoke Mkdir and use its result.
	err = os.Mkdir(path, perm)
	if err != nil {
		// Handle arguments like "foo/." by
		// double-checking that directory doesn't exist.
		dir, err1 := os.Lstat(path)
		if err1 == nil && dir.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

func (f *BasicFilesystem) Unhide(name string) error {
	name, err := f.rooted(name)
	if err != nil {
		return err
	}
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}

	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return err
	}

	attrs &^= syscall.FILE_ATTRIBUTE_HIDDEN
	return syscall.SetFileAttributes(p, attrs)
}

func (f *BasicFilesystem) Hide(name string) error {
	name, err := f.rooted(name)
	if err != nil {
		return err
	}
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}

	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return err
	}

	attrs |= syscall.FILE_ATTRIBUTE_HIDDEN
	return syscall.SetFileAttributes(p, attrs)
}

// Typically, attrs should be populated with value from GetFileAttributes and some bitwise operations performed on them
func (f *BasicFilesystem) SetFileAttributes(name string, attrs uint32) error {
	name, err := f.rooted(name)
	if err != nil {
		return err
	}

	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}

	return syscall.SetFileAttributes(p, attrs)
}

func (f *BasicFilesystem) GetFileAttributes(name string) (uint32, error) {
	name, err := f.rooted(name)
	if err != nil {
		return 0, err
	}
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}

	return syscall.GetFileAttributes(p)
}

// Currently only 3 file attributes are allowed: hidden, system, not_content_indexed
func (f *BasicFilesystem) AddFileAttributes(name string, newAttrs uint32) error {
	name, err := f.rooted(name)
	if err != nil {
		return err
	}

	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}

	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return err
	}

	// TODO: go 1.11: use windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED instead of 0x00002000
	newAttrs &= windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM | 0x00002000
	return syscall.SetFileAttributes(p, attrs|newAttrs)
}

func (f *BasicFilesystem) Roots() ([]string, error) {
	kernel32, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return nil, err
	}
	getLogicalDriveStringsHandle, err := kernel32.FindProc("GetLogicalDriveStringsA")
	if err != nil {
		return nil, err
	}

	buffer := [1024]byte{}
	bufferSize := uint32(len(buffer))

	hr, _, _ := getLogicalDriveStringsHandle.Call(uintptr(unsafe.Pointer(&bufferSize)), uintptr(unsafe.Pointer(&buffer)))
	if hr == 0 {
		return nil, fmt.Errorf("Syscall failed")
	}

	var drives []string
	parts := bytes.Split(buffer[:], []byte{0})
	for _, part := range parts {
		if len(part) == 0 {
			break
		}
		drives = append(drives, string(part))
	}

	return drives, nil
}

func (f *BasicFilesystem) resolveWin83(absPath string) string {
	if !isMaybeWin83(absPath) {
		return absPath
	}
	if in, err := syscall.UTF16FromString(absPath); err == nil {
		out := make([]uint16, 4*len(absPath)) // *2 for UTF16 and *2 to double path length
		if n, err := syscall.GetLongPathName(&in[0], &out[0], uint32(len(out))); err == nil {
			if n <= uint32(len(out)) {
				return syscall.UTF16ToString(out[:n])
			}
			out = make([]uint16, n)
			if _, err = syscall.GetLongPathName(&in[0], &out[0], n); err == nil {
				return syscall.UTF16ToString(out)
			}
		}
	}
	// Failed getting the long path. Return the part of the path which is
	// already a long path.
	for absPath = filepath.Dir(absPath); strings.HasPrefix(absPath, f.root); absPath = filepath.Dir(absPath) {
		if !isMaybeWin83(absPath) {
			return absPath
		}
	}
	return f.root
}

func isMaybeWin83(absPath string) bool {
	if !strings.Contains(absPath, "~") {
		return false
	}
	if strings.Contains(filepath.Dir(absPath), "~") {
		return true
	}
	return strings.Contains(strings.TrimPrefix(filepath.Base(absPath), WindowsTempPrefix), "~")
}
