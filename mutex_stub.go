//go:build !windows

package main

func acquireSingleInstanceMutex() bool { return true }
