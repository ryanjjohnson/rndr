//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package main

import "syscall"

const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
