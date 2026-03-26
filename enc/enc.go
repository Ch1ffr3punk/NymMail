package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image/color"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// ANSI regex for stripping terminal color codes from output
var ansiRegex = regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)

// stripANSI removes all ANSI escape sequences from a string
func stripANSI(text string) string {
	return ansiRegex.ReplaceAllString(text, "")
}

// Maximum number of lines to keep in output (prevents memory issues)
const maxOutputLines = 500

// CustomTheme overrides specific colors for better text visibility
type CustomTheme struct {
	fyne.Theme
	isDark bool
}

func (t CustomTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameForeground, theme.ColorNameDisabled:
		if t.isDark {
			return color.RGBA{R: 235, G: 235, B: 235, A: 255}
		}
		return color.RGBA{R: 20, G: 20, B: 20, A: 255}

	// Make border invisible by matching background color
	case theme.ColorNameInputBorder:
		if t.isDark {
			return color.RGBA{R: 28, G: 28, B: 30, A: 255}
		}
		return color.White

	// Input background for consistent look
	case theme.ColorNameInputBackground:
		if t.isDark {
			return color.RGBA{R: 28, G: 28, B: 30, A: 255}
		}
		return color.White
	}
	return t.Theme.Color(name, variant)
}

func (t CustomTheme) Font(style fyne.TextStyle) fyne.Resource {
	return t.Theme.Font(style)
}

func (t CustomTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return t.Theme.Icon(name)
}

func (t CustomTheme) Size(name fyne.ThemeSizeName) float32 {
	return t.Theme.Size(name)
}

// NymWrapper represents the main application structure
type NymWrapper struct {
	app          fyne.App
	window       fyne.Window
	outputRich   *widget.RichText
	outputScroll *container.Scroll
	startBtn     *widget.Button
	stopBtn      *widget.Button
	quitBtn      *widget.Button
	statusLabel  *widget.Label
	cmd          *exec.Cmd
	isRunning    bool
	isDarkTheme  bool
	themeSwitch  *widget.Button
	infoBtn      *widget.Button
	clientID     string
	outputMutex  sync.Mutex // Protects RichText segments from race conditions
}

// generateRandomID creates a random hex string of the specified length
func generateRandomID(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// getNymClientPath returns the absolute path to nym-client binary
// Prioritizes current directory, then falls back to PATH
func getNymClientPath() (string, error) {
	nymClientName := "nym-client"
	if runtime.GOOS == "windows" {
		nymClientName = "nym-client.exe"
	}

	// 1. Check current directory FIRST
	if _, err := os.Stat(nymClientName); err == nil {
		absPath, err := filepath.Abs(nymClientName)
		if err != nil {
			return "", fmt.Errorf("failed to get absolute path: %v", err)
		}
		return absPath, nil
	}

	// 2. Check PATH as fallback
	if path, err := exec.LookPath(nymClientName); err == nil {
		return path, nil
	}

	// 3. Windows fallback: check without extension
	if runtime.GOOS == "windows" {
		if _, err := os.Stat("nym-client"); err == nil {
			absPath, err := filepath.Abs("nym-client")
			if err != nil {
				return "", fmt.Errorf("failed to get absolute path: %v", err)
			}
			return absPath, nil
		}
	}

	return "", fmt.Errorf("nym-client not found in current directory or PATH")
}

// getNymDataDir returns the nym data directory path
func getNymDataDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %v", err)
	}
	return filepath.Join(homeDir, ".nym"), nil
}

// deleteAllClients removes ALL client directories
func deleteAllClients() error {
	nymDataDir, err := getNymDataDir()
	if err != nil {
		return err
	}
	clientsDir := filepath.Join(nymDataDir, "clients")
	if _, err := os.Stat(clientsDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to stat clients directory: %v", err)
	}
	return os.RemoveAll(clientsDir)
}

// deleteSpecificClient removes a specific client directory
func deleteSpecificClient(clientID string) error {
	nymDataDir, err := getNymDataDir()
	if err != nil {
		return err
	}
	clientDir := filepath.Join(nymDataDir, "clients", clientID)
	if _, err := os.Stat(clientDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to stat client directory: %v", err)
	}
	return os.RemoveAll(clientDir)
}

// stopProcess gracefully stops a process
func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}

	if runtime.GOOS == "windows" {
		// On Windows, send Ctrl+C via taskkill with hidden window
		taskkill := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", cmd.Process.Pid), "/T")
		taskkill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		taskkill.Run()
	} else {
		// On Unix, send SIGTERM
		cmd.Process.Signal(syscall.SIGTERM)
	}
}

// appendToOutput adds text to the output area with auto-scroll and crash protection
func (n *NymWrapper) appendToOutput(text string) {
	fyne.Do(func() {
		// Panic recovery to prevent app crash
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("Recovered from panic in appendToOutput: %v\n", r)
			}
		}()

		// Strip ANSI codes for clean text output
		cleanText := stripANSI(text)

		// Thread-safe segment modification
		n.outputMutex.Lock()
		defer n.outputMutex.Unlock()

		// Append text as new RichText segment
		n.outputRich.Segments = append(n.outputRich.Segments, &widget.TextSegment{
			Style: widget.RichTextStyle{
				TextStyle: fyne.TextStyle{Monospace: true},
				ColorName: theme.ColorNameForeground,
				Inline:    true,
			},
			Text: cleanText + "\n",
		})

		// Limit output to maxOutputLines to prevent memory issues
		if len(n.outputRich.Segments) > maxOutputLines {
			// Remove oldest segments (keep first "Ready..." segment + newest lines)
			n.outputRich.Segments = append(
				n.outputRich.Segments[:1],
				n.outputRich.Segments[len(n.outputRich.Segments)-maxOutputLines+1:]...,
			)
		}

		n.outputRich.Refresh()

		// Auto-scroll to bottom - RichText MinSize() returns actual content height!
		if n.outputScroll != nil {
			contentHeight := n.outputRich.MinSize().Height
			viewportHeight := n.outputScroll.Size().Height

			if contentHeight > viewportHeight {
				n.outputScroll.Offset.Y = contentHeight - viewportHeight
				n.outputScroll.Refresh()
			}
		}
	})
}

// updateStatus updates the status label
func (n *NymWrapper) updateStatus(status string) {
	fyne.Do(func() {
		n.statusLabel.SetText(status)
		n.statusLabel.Refresh()
	})
}

// toggleTheme switches between dark and light theme
func (n *NymWrapper) toggleTheme() {
	fyne.Do(func() {
		n.isDarkTheme = !n.isDarkTheme
		n.themeSwitch.SetText(map[bool]string{true: "☀️", false: "🌙"}[n.isDarkTheme])
		baseTheme := map[bool]fyne.Theme{true: theme.DarkTheme(), false: theme.LightTheme()}[n.isDarkTheme]
		customTheme := &CustomTheme{Theme: baseTheme, isDark: n.isDarkTheme}
		n.app.Settings().SetTheme(customTheme)
		n.window.Content().Refresh()
	})
}

// showInfoPopup shows information about the application
func (n *NymWrapper) showInfoPopup() {
	projURL, _ := url.Parse("https://github.com/Ch1ffr3punk/NymMail")
	projectLink := widget.NewHyperlink("an Open Source project", projURL)
	okButton := widget.NewButton("OK", func() {
		overlays := n.window.Canvas().Overlays()
		if overlays.Top() != nil {
			overlays.Remove(overlays.Top())
		}
	})
	okButton.Importance = widget.HighImportance
	content := container.NewVBox(
		widget.NewLabelWithStyle("Ephemeral Nym Client Wrapper v0.1.0", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), projectLink, layout.NewSpacer()),
		widget.NewLabelWithStyle("released under the Apache 2.0 license", fyne.TextAlignCenter, fyne.TextStyle{}),
		widget.NewLabelWithStyle("© 2026 Ch1ffr3punk", fyne.TextAlignCenter, fyne.TextStyle{}),
		container.NewHBox(layout.NewSpacer(), okButton, layout.NewSpacer()),
	)
	dialog.ShowCustomWithoutButtons("", content, n.window)
}

// startNymClient starts the nym-client process with robust error handling
func (n *NymWrapper) startNymClient() {
	if n.isRunning {
		n.appendToOutput("Warning: Client is already running")
		return
	}

	go func() {
		// Panic recovery for goroutine safety
		defer func() {
			if r := recover(); r != nil {
				n.appendToOutput(fmt.Sprintf("PANIC in client goroutine: %v", r))
				n.updateStatus("Error")
				fyne.Do(func() {
					n.startBtn.Enable()
					n.stopBtn.Disable()
				})
			}
		}()

		n.updateStatus("Starting...")
		n.appendToOutput("=== Nym Client Ephemeral Session ===")
		n.appendToOutput("")

		// Get nym-client path
		nymClientPath, err := getNymClientPath()
		if err != nil {
			n.appendToOutput(fmt.Sprintf("Error: %v", err))
			n.updateStatus("Error")
			return
		}
		n.appendToOutput(fmt.Sprintf("Found nym-client at: %s", nymClientPath))

		// Generate random ID
		clientID, err := generateRandomID(16)
		if err != nil {
			n.appendToOutput(fmt.Sprintf("Failed to generate random ID: %v", err))
			n.updateStatus("Error")
			return
		}
		n.clientID = clientID
		n.appendToOutput(fmt.Sprintf("Generated ephemeral client ID: %s", clientID))

		// Clean up ALL previous client data
		n.appendToOutput("")
		n.appendToOutput("--- Pre-run cleanup ---")
		if err := deleteAllClients(); err != nil {
			n.appendToOutput(fmt.Sprintf("  Warning: %v", err))
		}

		// Step 1: Initialize the client
		n.appendToOutput("")
		n.appendToOutput("--- Initializing client ---")
		initCmd := exec.Command(nymClientPath, "init", "--id", clientID)
		if runtime.GOOS == "windows" {
			initCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		}
		initOutput, err := initCmd.CombinedOutput()
		if err != nil {
			n.appendToOutput(fmt.Sprintf("Failed to initialize client: %v\nOutput: %s", err, string(initOutput)))
			n.updateStatus("Error")
			return
		}
		n.appendToOutput(string(initOutput))

		// Step 2: Run the client
		n.appendToOutput("")
		n.appendToOutput(fmt.Sprintf("--- Running client (ID: %s) ---", clientID))
		n.appendToOutput("Press Stop button to stop the client")
		n.appendToOutput("")

		runCmd := exec.Command(nymClientPath, "run", "--id", clientID)
		if runtime.GOOS == "windows" {
			runCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		}

		// Capture stdout and stderr
		stdout, err := runCmd.StdoutPipe()
		if err != nil {
			n.appendToOutput(fmt.Sprintf("Failed to capture stdout: %v", err))
			n.updateStatus("Error")
			return
		}
		stderr, err := runCmd.StderrPipe()
		if err != nil {
			n.appendToOutput(fmt.Sprintf("Failed to capture stderr: %v", err))
			n.updateStatus("Error")
			return
		}

		if err := runCmd.Start(); err != nil {
			n.appendToOutput(fmt.Sprintf("Failed to start client: %v", err))
			n.updateStatus("Error")
			return
		}

		n.cmd = runCmd
		n.isRunning = true
		n.updateStatus("Running")

		// Enable stop button, disable start button
		fyne.Do(func() {
			n.startBtn.Disable()
			n.stopBtn.Enable()
		})

		// Read stdout in a goroutine
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				n.appendToOutput(scanner.Text())
			}
		}()

		// Read stderr in a goroutine
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				n.appendToOutput(scanner.Text())
			}
		}()

		// Wait for process to complete
		err = runCmd.Wait()
		n.isRunning = false

		fyne.Do(func() {
			n.startBtn.Enable()
			n.stopBtn.Disable()
		})

		// Detailed exit code logging
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				n.appendToOutput(fmt.Sprintf("Client exited with code: %d", exitErr.ExitCode()))
				if runtime.GOOS != "windows" {
					if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
						if status.Signaled() {
							n.appendToOutput(fmt.Sprintf("Terminated by signal: %d", status.Signal()))
						}
					}
				}
			} else {
				n.appendToOutput(fmt.Sprintf("Client stopped with error: %v", err))
			}
		} else {
			n.appendToOutput("")
			n.appendToOutput("Client stopped normally")
		}

		// Clean up client data
		n.appendToOutput("")
		n.appendToOutput("--- Post-run cleanup ---")
		if err := deleteSpecificClient(clientID); err != nil {
			n.appendToOutput(fmt.Sprintf("  Warning: %v", err))
		} else {
			n.appendToOutput("  Client data removed.")
		}

		n.appendToOutput("")
		n.appendToOutput("=== Ephemeral session complete ===")
		n.appendToOutput("No client data remains on disk")
		n.updateStatus("Stopped")
	}()
}

// stopNymClient stops the running nym-client process (non-blocking)
func (n *NymWrapper) stopNymClient() {
	if !n.isRunning {
		n.appendToOutput("Warning: No client is running")
		return
	}

	n.appendToOutput("")
	n.appendToOutput("Shutting down nym-client...")

	// Send graceful shutdown signal
	stopProcess(n.cmd)

	// Update UI state immediately for responsive feedback
	fyne.Do(func() {
		n.stopBtn.Disable()
		n.updateStatus("Stopping...")
	})

	// Run the wait/force-kill logic in a separate goroutine to avoid blocking UI
	go func() {
		// Give the process time to shutdown gracefully
		time.Sleep(2 * time.Second)

		// Force kill if still running
		if n.isRunning && n.cmd != nil && n.cmd.Process != nil {
			if runtime.GOOS == "windows" {
				// Hide console window for taskkill on Windows
				cmd := exec.Command("taskkill", "/F", "/PID", fmt.Sprintf("%d", n.cmd.Process.Pid))
				cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
				cmd.Run()
			} else {
				n.cmd.Process.Kill()
			}
			n.appendToOutput("Client force killed")
			n.isRunning = false
		}

		// Update UI state after background operation completes
		fyne.Do(func() {
			n.updateStatus("Stopped")
			n.startBtn.Enable()
			n.stopBtn.Disable()
		})
	}()
}

// quitApplication quits the application
func (n *NymWrapper) quitApplication() {
	if n.isRunning {
		n.appendToOutput("Stopping client before quitting...")
		stopProcess(n.cmd)
		time.Sleep(1 * time.Second)
	}
	n.appendToOutput("Goodbye!")
	time.Sleep(500 * time.Millisecond)
	n.app.Quit()
}

// setupUI creates and configures the user interface
func (n *NymWrapper) setupUI() {
	// Create RichText output area (reliable scrolling via MinSize)
	n.outputRich = widget.NewRichText()
	n.outputRich.Wrapping = fyne.TextWrapWord
	n.outputRich.Segments = []widget.RichTextSegment{
		&widget.TextSegment{
			Style: widget.RichTextStyle{
				TextStyle: fyne.TextStyle{Monospace: true},
				ColorName: theme.ColorNameForeground,
			},
			Text: "Ready. Click Start to begin.\n",
		},
	}
	n.outputScroll = container.NewScroll(n.outputRich)

	n.statusLabel = widget.NewLabel("Ready")
	n.statusLabel.TextStyle = fyne.TextStyle{Bold: true}

	n.startBtn = widget.NewButton("Start", n.startNymClient)
	n.startBtn.Importance = widget.HighImportance

	n.stopBtn = widget.NewButton("Stop", n.stopNymClient)
	n.stopBtn.Importance = widget.HighImportance // = Blue/Accent in Fyne
	n.stopBtn.Disable()

	n.quitBtn = widget.NewButton("Quit", n.quitApplication)
	n.quitBtn.Importance = widget.HighImportance

	n.themeSwitch = widget.NewButton("☀️", n.toggleTheme)
	n.themeSwitch.Importance = widget.LowImportance

	n.infoBtn = widget.NewButtonWithIcon("", theme.InfoIcon(), n.showInfoPopup)
	n.infoBtn.Importance = widget.LowImportance

	topBar := container.NewHBox(n.infoBtn, layout.NewSpacer(), n.themeSwitch)
	buttonContainer := container.NewHBox(layout.NewSpacer(), n.startBtn, n.stopBtn, n.quitBtn, layout.NewSpacer())

	content := container.NewBorder(
		container.NewVBox(topBar, widget.NewSeparator(), buttonContainer, widget.NewSeparator()),
		container.NewVBox(widget.NewSeparator(), container.NewHBox(widget.NewLabel("Status: "), n.statusLabel, layout.NewSpacer())),
		nil, nil,
		n.outputScroll,
	)

	n.window.SetContent(content)
}

func main() {
	myApp := app.New()
	window := myApp.NewWindow("Ephemeral Nym Client Wrapper")

	wrapper := &NymWrapper{
		app:         myApp,
		window:      window,
		isRunning:   false,
		isDarkTheme: true,
	}

	customTheme := &CustomTheme{Theme: theme.DarkTheme(), isDark: true}
	myApp.Settings().SetTheme(customTheme)

	wrapper.setupUI()
	window.Resize(fyne.NewSize(640, 480))
	window.ShowAndRun()
}
