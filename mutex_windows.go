//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

const singleInstanceMutexName = "Global\\GestorEscolar-SingleInstance"

// acquireSingleInstanceMutex cria um mutex nomeado global. Retorna false se outra
// instância (serviço ou tray) já mantém o mutex.
func acquireSingleInstanceMutex() bool {
	name, err := windows.UTF16PtrFromString(singleInstanceMutexName)
	if err != nil {
		return true
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return false
	}
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return true
	}
	// Mantém handle aberto até o processo terminar.
	return true
}
