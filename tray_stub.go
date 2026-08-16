//go:build !windows

package main

// runTray mantém o processo vivo em plataformas sem systray (Linux headless / systemd).
func runTray(cfg *Config) {
	select {}
}

// openGUI é no-op em plataformas não-Windows.
func openGUI(cfg *Config) {}

// showMessageBox é no-op em plataformas não-Windows.
func showMessageBox(title, msg string) {}

// setClipboard é no-op em plataformas não-Windows.
func setClipboard(text string) error { return nil }
