//go:build !windows

package main

func ensureGUIServer(cfg *Config) {}

func tryOpenGUIPanel() bool { return false }

func openBrowserToPanel() {}
