//go:build !windows

package main

func tryRunAsWindowsService() bool {
	return false
}
