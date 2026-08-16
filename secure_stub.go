//go:build !windows

package main

// restrictFilePermissions é no-op fora do Windows: o 0600 do os.WriteFile/os.Chmod já é a ACL
// (POSIX honra o modo). O teto de ACL de verdade (SYSTEM/Administradores) é responsabilidade só do
// Windows — ver secure_windows.go.
func restrictFilePermissions(path string) error { return nil }
