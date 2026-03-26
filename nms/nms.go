package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

const (
	connectionTimeout = 30 * time.Second
	chunkSize         = 1024 * 32
	nymClientPort     = "1977"
	maxFileSize       = 64 * 1024 // 64KB max file size
)

var debug bool
var storagePasswordHash string
var authenticatedClientAddr string // Client's Nym address for response routing

// FileInfo contains metadata about a file being transferred
type FileInfo struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Chunks int    `json:"chunks"`
}

// FileChunk represents a chunk of file data during transfer
type FileChunk struct {
	Type   string `json:"type"`
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	Data   []byte `json:"data"`
	IsLast bool   `json:"is_last"`
}

// AuthRequest is sent by client for authentication
type AuthRequest struct {
	Type       string `json:"type"`
	Password   string `json:"password"`
	ClientAddr string `json:"client_addr,omitempty"`
}

// AuthResponse is sent back to client with authentication result
type AuthResponse struct {
	Type    string `json:"type"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// FileListRequest requests the list of available files
type FileListRequest struct {
	Type string `json:"type"`
}

// FileListResponse contains the list of available files
type FileListResponse struct {
	Type  string     `json:"type"`
	Files []FileInfo `json:"files"`
}

// GetFileRequest requests a specific file
type GetFileRequest struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// DeleteRequest requests deletion of files
type DeleteRequest struct {
	Type  string   `json:"type"`
	Files []string `json:"files"`
}

// ReceivedChunk stores a chunk that arrived out of order
type ReceivedChunk struct {
	Data   []byte
	Index  int
	IsLast bool
}

// logInfo prints informational messages
func logInfo(format string, args ...interface{}) {
	fmt.Printf("[INFO] "+format+"\n", args...)
}

// logDebug prints debug messages (only if debug flag is set)
func logDebug(format string, args ...interface{}) {
	if debug {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

// logError prints error messages
func logError(format string, args ...interface{}) {
	fmt.Printf("[ERROR] "+format+"\n", args...)
}

// logReceive prints file reception messages
func logReceive(format string, args ...interface{}) {
	fmt.Printf("[RECV] "+format+"\n", args...)
}

// logComplete prints file completion messages
func logComplete(format string, args ...interface{}) {
	fmt.Printf("[OK] "+format+"\n", args...)
}

// logReject prints file rejection messages
func logReject(format string, args ...interface{}) {
	fmt.Printf("[REJECT] "+format+"\n", args...)
}

// getPasswordInteractive prompts for password without echoing to terminal
// This is the most secure method for manual usage (not stored in bash history)
func getPasswordInteractive() (string, error) {
	fmt.Print("Enter password: ")
	bytePassword, err := term.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return "", err
	}
	fmt.Println() // Newline after password input
	return string(bytePassword), nil
}

// getPasswordFromFile reads password from a file with secure permissions
// Recommended for automated scripts (file should have chmod 600)
func getPasswordFromFile(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read password file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// getPasswordFromEnv reads password from environment variable
// Good for session-based usage (not persistent)
func getPasswordFromEnv() string {
	return os.Getenv("NYM_STORE_PASSWORD")
}

// main is the entry point of the server application
func main() {
	var outputDir string
	var password string
	var passwordFile string

	// Define command line flags
	flag.StringVar(&outputDir, "o", "./received", "Output directory for received files")
	flag.StringVar(&password, "p", "", "Password (not recommended - visible in history)")
	flag.StringVar(&passwordFile, "f", "", "Password file path (recommended for scripts)")
	flag.BoolVar(&debug, "d", false, "Enable debug output")
	flag.Parse()

	// Password loading priority: 1. File, 2. Env, 3. Interactive, 4. Command line
	if passwordFile != "" {
		// Load password from file (most secure for automation)
		var err error
		password, err = getPasswordFromFile(passwordFile)
		if err != nil {
			logError("Failed to read password file: %v", err)
			os.Exit(1)
		}
		logInfo("Password loaded from file: %s", passwordFile)
	} else if envPassword := getPasswordFromEnv(); envPassword != "" {
		// Load password from environment variable
		password = envPassword
		logInfo("Password loaded from environment variable")
	} else if password == "" {
		// Interactive password prompt (most secure for manual usage)
		var err error
		password, err = getPasswordInteractive()
		if err != nil {
			logError("Failed to read password: %v", err)
			os.Exit(1)
		}
	} else {
		// Command line password (least secure - warn user)
		fmt.Println("   WARNING: Password in command line may be visible in bash history!")
		fmt.Println("   Consider using -f <file> or interactive input instead.")
		fmt.Println("   To hide from history: export HISTIGNORE=\"$HISTIGNORE:*-p*\"")
	}

	// Validate password is set
	if password == "" {
		logError("Password is required. Use -p, -f, -env, or interactive input")
		os.Exit(1)
	}

	// Store SHA256 hash of password for authentication (same as client)
	hash := sha256.Sum256([]byte(password))
	storagePasswordHash = hex.EncodeToString(hash[:])

	logInfo("Connecting to nym-client on port %s", nymClientPort)

	// Connect to local Nym client WebSocket
	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:"+nymClientPort, nil)
	if err != nil {
		logError("Cannot connect to nym-client: %v", err)
		logInfo("Make sure nym-client is running on port 1977")
		os.Exit(1)
	}
	defer conn.Close()

	// Get self address for display (optional)
	req, _ := json.Marshal(map[string]string{"type": "selfAddress"})
	conn.WriteMessage(websocket.TextMessage, req)

	var resp map[string]interface{}
	conn.ReadJSON(&resp)

	if selfAddr, ok := resp["address"].(string); ok {
		shortAddr := selfAddr
		if len(shortAddr) > 20 {
			shortAddr = shortAddr[:20] + "..."
		}
		logInfo("Nym address: %s", shortAddr)
		logDebug("Full address: %s", selfAddr)
	}

	logInfo("Email storage directory: %s", outputDir)
	logInfo("Maximum file size: %s", formatBytes(maxFileSize))
	logInfo("Waiting for messages...")
	logInfo("Press Ctrl+C to exit")
	fmt.Println()

	runServer(conn, outputDir)
}

// runServer is the main loop: receives messages from nym-client and routes them
func runServer(conn *websocket.Conn, outputDir string) {
	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		logError("Creating output directory: %v", err)
		os.Exit(1)
	}

	// Handle graceful shutdown on Ctrl+C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println()
		logInfo("Shutting down...")
		os.Exit(0)
	}()

	// State for receiving files from email senders (original logic - UNCHANGED)
	var currentFile *os.File
	var currentFileName string
	var currentFileSize int64
	var totalChunks int
	var nextExpectedIndex int
	var chunksWritten int
	var receivedChunks map[int]ReceivedChunk
	var startTime time.Time
	var fileCount int

	// Main receive loop
	for {
		conn.SetReadDeadline(time.Time{})
		_, msg, err := conn.ReadMessage()
		if err != nil {
			logError("Connection: %v", err)
			break
		}

		logDebug("Received raw message, length: %d bytes", len(msg))

		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			logDebug("Failed to parse JSON: %v", err)
			continue
		}

		// Route messages based on type
		switch data["type"] {
		case "received":
			// Message arrived via Nym network (wrapped by nym-client)
			message, _ := data["message"].(string)
			if len(message) == 0 {
				continue
			}

			// Route control messages (auth/list/get/delete)
			var msgData map[string]interface{}
			if json.Unmarshal([]byte(message), &msgData) == nil {
				if msgType, ok := msgData["type"].(string); ok && msgType == "message" {
					if innerMsg, ok := msgData["message"].(string); ok {
						handleClientRequest(innerMsg, outputDir, conn)
						continue
					}
				}
			}

			// Otherwise, handle as file transfer message (original logic - UNCHANGED)
			handleReceivedMessage(message, outputDir, &currentFile, &currentFileName,
				&currentFileSize, &totalChunks, &nextExpectedIndex, &chunksWritten,
				&receivedChunks, &startTime, &fileCount)

		case "message":
			// Direct local client request (not via Nym network)
			innerMsg, _ := data["message"].(string)
			if len(innerMsg) == 0 {
				continue
			}
			handleClientRequest(innerMsg, outputDir, conn)

		default:
			logDebug("Unknown message type: %v", data["type"])
		}
	}
}

// sendResponse wraps response in nym-client format for proper routing back to client
// All responses must be wrapped so nym-client can route them through the network
func sendResponse(conn *websocket.Conn, response interface{}) {
	respJSON, err := json.Marshal(response)
	if err != nil {
		logError("Failed to marshal response: %v", err)
		return
	}

	if authenticatedClientAddr == "" {
		logError("No client address stored - cannot route response")
		return
	}

	// Wrap in nym-client send format so response reaches client
	sendMsg := fmt.Sprintf(`{"type":"send","recipient":%q,"message":%q}`,
		authenticatedClientAddr, string(respJSON))

	logDebug("Sending response to: %s", authenticatedClientAddr[:20]+"...")
	conn.WriteMessage(websocket.TextMessage, []byte(sendMsg))
}

// handleClientRequest processes authenticated client requests (auth/list/get/delete)
func handleClientRequest(innerMsg string, outputDir string, conn *websocket.Conn) {
	var msg map[string]interface{}
	if err := json.Unmarshal([]byte(innerMsg), &msg); err != nil {
		logDebug("Failed to parse client request: %v", err)
		return
	}

	msgType, ok := msg["type"].(string)
	if !ok {
		logDebug("Client request missing type field")
		return
	}

	logDebug("Client request type: %s", msgType)

	switch msgType {
	case "auth":
		password, ok := msg["password"].(string)
		if !ok {
			logDebug("Auth request missing password field")
			sendResponse(conn, AuthResponse{Type: "auth_response", Success: false, Message: "Invalid auth request"})
			return
		}
		clientAddr, _ := msg["client_addr"].(string)
		handleAuth(AuthRequest{Type: "auth", Password: password, ClientAddr: clientAddr}, conn)

	case "list":
		handleList(outputDir, conn)

	case "get":
		name, ok := msg["name"].(string)
		if !ok {
			logDebug("Get request missing name field")
			return
		}
		handleGetFile(name, outputDir, conn)

	case "delete":
		filesRaw, ok := msg["files"].([]interface{})
		if !ok {
			logDebug("Delete request missing files field")
			return
		}
		files := make([]string, len(filesRaw))
		for i, f := range filesRaw {
			files[i], _ = f.(string)
		}
		handleDelete(files, outputDir, conn)

	default:
		logDebug("Unknown client request type: %s", msgType)
		sendResponse(conn, map[string]string{"type": "error", "message": "Unknown request type"})
	}
}

// handleAuth validates client password and stores client address for response routing
func handleAuth(auth AuthRequest, conn *websocket.Conn) {
	response := AuthResponse{Type: "auth_response"}

	if auth.Password == storagePasswordHash {
		response.Success = true
		response.Message = "Authentication successful"
		if auth.ClientAddr != "" {
			authenticatedClientAddr = auth.ClientAddr
			logInfo("Client authenticated: %s", authenticatedClientAddr[:20]+"...")
		} else {
			logInfo("Client authenticated (no address provided)")
		}
	} else {
		response.Success = false
		response.Message = "Invalid password"
		logInfo("Authentication failed: invalid password")
	}

	sendResponse(conn, response)
}

// handleList returns the list of available files in the storage directory
func handleList(outputDir string, conn *websocket.Conn) {
	files, err := os.ReadDir(outputDir)
	if err != nil {
		logError("Failed to read directory: %v", err)
		return
	}

	var fileInfos []FileInfo
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		fileInfos = append(fileInfos, FileInfo{
			Type: "file_info",
			Name: f.Name(),
			Size: info.Size(),
		})
	}

	response := FileListResponse{
		Type:  "file_list",
		Files: fileInfos,
	}
	sendResponse(conn, response)
	logDebug("Sent file list: %d files", len(fileInfos))
}

// handleGetFile sends a file to the client in chunks
func handleGetFile(filename, outputDir string, conn *websocket.Conn) {
	filePath := filepath.Join(outputDir, filename)
	file, err := os.Open(filePath)
	if err != nil {
		logError("Failed to open file %s: %v", filename, err)
		sendResponse(conn, map[string]string{"type": "error", "message": "File not found"})
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		logError("Failed to stat file %s: %v", filename, err)
		return
	}

	totalChunks := int((stat.Size() + chunkSize - 1) / chunkSize)
	logInfo("Sending file %s (%s, %d chunks)", filename, formatBytes(stat.Size()), totalChunks)

	buffer := make([]byte, chunkSize)
	for i := 0; i < totalChunks; i++ {
		n, err := file.Read(buffer)
		if err != nil && err != io.EOF {
			logError("Error reading file: %v", err)
			return
		}

		chunk := FileChunk{
			Type:   "file_chunk",
			Index:  i,
			Total:  totalChunks,
			Data:   buffer[:n],
			IsLast: (i == totalChunks-1),
		}

		chunkJSON, _ := json.Marshal(chunk)

		// Wrap chunk in nym-client format for routing
		sendMsg := fmt.Sprintf(`{"type":"send","recipient":%q,"message":%q}`,
			authenticatedClientAddr, string(chunkJSON))

		if err := conn.WriteMessage(websocket.TextMessage, []byte(sendMsg)); err != nil {
			logError("Error sending chunk: %v", err)
			return
		}
	}
}

// handleDelete removes files from the storage directory
func handleDelete(files []string, outputDir string, conn *websocket.Conn) {
	deleted := []string{}
	failed := []string{}

	for _, filename := range files {
		filePath := filepath.Join(outputDir, filename)
		if err := os.Remove(filePath); err != nil {
			logError("Failed to delete %s: %v", filename, err)
			failed = append(failed, filename)
		} else {
			deleted = append(deleted, filename)
			logInfo("Deleted: %s", filename)
		}
	}

	response := map[string]interface{}{
		"type":    "delete_success",
		"deleted": deleted,
		"failed":  failed,
		"message": fmt.Sprintf("Deleted %d, failed %d", len(deleted), len(failed)),
	}
	sendResponse(conn, response)
}

func handleReceivedMessage(message, outputDir string, currentFile **os.File,
	currentFileName *string, currentFileSize *int64, totalChunks *int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk,
	startTime *time.Time, fileCount *int) {

	var msgData map[string]interface{}
	if err := json.Unmarshal([]byte(message), &msgData); err != nil {
		logDebug("Failed to parse inner message: %v", err)
		handleRawData(message, outputDir, currentFile, currentFileName,
			currentFileSize, totalChunks, nextExpectedIndex, chunksWritten,
			receivedChunks, startTime)
		return
	}

	msgType, _ := msgData["type"].(string)
	logDebug("Inner message type: %s", msgType)

	switch msgType {
	case "file_info":
		handleFileInfo(message, outputDir, currentFile, currentFileName,
			currentFileSize, totalChunks, nextExpectedIndex, chunksWritten,
			receivedChunks, startTime, fileCount)

	case "file_chunk":
		handleFileChunkWithReorder(message, currentFile, currentFileName,
			*currentFileSize, *totalChunks, nextExpectedIndex, chunksWritten,
			receivedChunks, startTime)
	}
}

func handleRawData(rawData, outputDir string, currentFile **os.File,
	currentFileName *string, currentFileSize *int64, totalChunks *int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk,
	startTime *time.Time) {

	var fileInfo FileInfo
	if err := json.Unmarshal([]byte(rawData), &fileInfo); err == nil && fileInfo.Type == "file_info" {
		handleFileInfo(rawData, outputDir, currentFile, currentFileName,
			currentFileSize, totalChunks, nextExpectedIndex, chunksWritten,
			receivedChunks, startTime, nil)
		return
	}

	var chunk FileChunk
	if err := json.Unmarshal([]byte(rawData), &chunk); err == nil && chunk.Type == "file_chunk" {
		handleFileChunkWithReorder(rawData, currentFile, currentFileName,
			*currentFileSize, *totalChunks, nextExpectedIndex, chunksWritten,
			receivedChunks, startTime)
		return
	}
}

func handleFileInfo(message, outputDir string, currentFile **os.File,
	currentFileName *string, currentFileSize *int64, totalChunks *int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk,
	startTime *time.Time, fileCount *int) {

	var fileInfo FileInfo
	if err := json.Unmarshal([]byte(message), &fileInfo); err != nil {
		logDebug("Failed to unmarshal file info: %v", err)
		return
	}

	logDebug("File info: name=%s, size=%d, chunks=%d", fileInfo.Name, fileInfo.Size, fileInfo.Chunks)

	if fileInfo.Size > maxFileSize {
		logReject("%s - size %s exceeds limit %s",
			fileInfo.Name, formatBytes(fileInfo.Size), formatBytes(maxFileSize))
		return
	}

	if *currentFile != nil {
		(*currentFile).Close()
		*currentFile = nil
	}

	logReceive("%s (%s, %d chunks)", fileInfo.Name, formatBytes(fileInfo.Size), fileInfo.Chunks)

	*currentFileName = fileInfo.Name
	*currentFileSize = fileInfo.Size
	*totalChunks = fileInfo.Chunks
	*nextExpectedIndex = 0
	*chunksWritten = 0
	*receivedChunks = make(map[int]ReceivedChunk)
	*startTime = time.Now()

	filePath := filepath.Join(outputDir, fileInfo.Name)
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		logError("Creating directory: %v", err)
		return
	}

	file, err := os.Create(filePath)
	if err != nil {
		logError("Creating file: %v", err)
		return
	}

	*currentFile = file
}

func handleFileChunkWithReorder(message string, currentFile **os.File,
	currentFileName *string, currentFileSize int64, totalChunks int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk,
	startTime *time.Time) {

	if *currentFile == nil {
		logDebug("Received chunk but no current file")
		return
	}

	var chunk FileChunk
	if err := json.Unmarshal([]byte(message), &chunk); err != nil {
		logDebug("Failed to unmarshal chunk: %v", err)
		return
	}

	logDebug("Received chunk %d/%d, size=%d, isLast=%v", chunk.Index+1, chunk.Total, len(chunk.Data), chunk.IsLast)

	(*receivedChunks)[chunk.Index] = ReceivedChunk{
		Data:   chunk.Data,
		Index:  chunk.Index,
		IsLast: chunk.IsLast,
	}

	for {
		storedChunk, exists := (*receivedChunks)[*nextExpectedIndex]
		if !exists {
			break
		}

		if _, err := (*currentFile).Write(storedChunk.Data); err != nil {
			logError("Writing chunk %d: %v", *nextExpectedIndex, err)
			return
		}

		*chunksWritten++
		delete(*receivedChunks, *nextExpectedIndex)
		*nextExpectedIndex++

		if storedChunk.IsLast || *chunksWritten == totalChunks {
			(*currentFile).Close()
			*currentFile = nil

			duration := time.Since(*startTime)

			if *chunksWritten == totalChunks {
				logComplete("%s (%s) in %v",
					*currentFileName, formatBytes(currentFileSize), duration.Round(time.Millisecond))
			} else {
				logError("Incomplete: %s (%d/%d chunks) in %v",
					*currentFileName, *chunksWritten, totalChunks, duration.Round(time.Millisecond))
			}

			*receivedChunks = nil
			break
		}
	}
}

// formatBytes converts bytes to human-readable format (KB, MB, etc.)
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
