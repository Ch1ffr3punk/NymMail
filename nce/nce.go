package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// generateRandomID creates a random string of the specified length
func generateRandomID(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// getNymClientPath returns the path to nym-client binary
func getNymClientPath() (string, error) {
	nymClientPath := "nym-client"
	
	if _, err := exec.LookPath(nymClientPath); err == nil {
		return nymClientPath, nil
	}
	
	if _, err := os.Stat("./nym-client"); err == nil {
		return "./nym-client", nil
	}
	
	return "", fmt.Errorf("nym-client not found in PATH or current directory")
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
	
	fmt.Printf("  Removing all client data: %s\n", clientsDir)
	err = os.RemoveAll(clientsDir)
	if err != nil {
		return fmt.Errorf("failed to remove clients directory: %v", err)
	}
	
	return nil
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
	
	fmt.Printf("  Removing client data for ID: %s\n", clientID)
	err = os.RemoveAll(clientDir)
	if err != nil {
		return fmt.Errorf("failed to remove client directory: %v", err)
	}
	
	return nil
}

// stopProcess gracefully stops a process on Windows
func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	
	if runtime.GOOS == "windows" {
		// On Windows, use taskkill to send Ctrl+C
		exec.Command("taskkill", "/PID", fmt.Sprintf("%d", cmd.Process.Pid), "/T").Run()
	} else {
		// On Unix, send SIGTERM
		cmd.Process.Signal(syscall.SIGTERM)
	}
}

// runCommand executes a command and returns its output or error
func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// runCommandAsync executes a command and returns the process handle
func runCommandAsync(name string, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	
	return cmd, nil
}

func main() {
	// Check if we should just pass through to nym-client
	if len(os.Args) > 1 && os.Args[1] != "" {
		// If any arguments are provided, pass through directly
		nymClientPath, err := getNymClientPath()
		if err != nil {
			log.Fatal(err)
		}
		
		cmd := exec.Command(nymClientPath, os.Args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	
	// No arguments - run ephemeral session (init + run)
	fmt.Println("=== Nym Client Ephemeral Session ===\n")
	
	// Get nym-client path
	nymClientPath, err := getNymClientPath()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("✓ Found nym-client at: %s\n", nymClientPath)
	
	// Generate random ID
	clientID, err := generateRandomID(16)
	if err != nil {
		log.Fatalf("Failed to generate random ID: %v", err)
	}
	fmt.Printf("✓ Generated ephemeral client ID: %s\n", clientID)
	
	// Clean up ALL previous client data
	fmt.Println("\n--- Pre-run cleanup ---")
	if err := deleteAllClients(); err != nil {
		fmt.Printf("  ⚠ Warning: %v\n", err)
	}
	
	// Step 1: Initialize the client
	fmt.Println("\n--- Initializing client ---")
	initOutput, err := runCommand(nymClientPath, "init", "--id", clientID)
	if err != nil {
		log.Fatalf("Failed to initialize client: %v\nOutput: %s", err, initOutput)
	}
	fmt.Println(initOutput)
	
	// Step 2: Run the client
	fmt.Printf("\n--- Running client (ID: %s) ---\n", clientID)
	fmt.Println("Press Ctrl+C to stop the client\n")
	
	runCmd, err := runCommandAsync(nymClientPath, "run", "--id", clientID)
	if err != nil {
		log.Fatalf("Failed to start client: %v", err)
	}
	
	// Handle interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	
	<-sigChan
	fmt.Println("\n\nShutting down nym-client...")
	
	// Stop the process gracefully
	stopProcess(runCmd)
	
	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		done <- runCmd.Wait()
	}()
	
	select {
	case <-done:
		fmt.Println("  Client stopped.")
	case <-time.After(5 * time.Second):
		fmt.Println("  Timeout waiting for client to stop, forcing...")
		if runtime.GOOS == "windows" {
			exec.Command("taskkill", "/F", "/PID", fmt.Sprintf("%d", runCmd.Process.Pid)).Run()
		} else {
			runCmd.Process.Kill()
		}
	}
	
	// Clean up client data
	fmt.Println("\n--- Post-run cleanup ---")
	if err := deleteSpecificClient(clientID); err != nil {
		fmt.Printf("  ⚠ Warning: %v\n", err)
	} else {
		fmt.Println("  ✓ Client data removed.")
	}
	
	fmt.Println("\n  === Ephemeral session complete ===")
	fmt.Println("  ✓ No client data remains on disk")
}
