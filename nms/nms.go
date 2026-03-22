package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	connectionTimeout = 30 * time.Second
	chunkSize         = 1024 * 32
	nymClientPort     = "1977"
	maxFileSize       = 24 * 1024 * 1024
)

var debug bool

type FileInfo struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Chunks int    `json:"chunks"`
}

type FileChunk struct {
	Type   string `json:"type"`
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	Data   []byte `json:"data"`
	IsLast bool   `json:"is_last"`
}

type ReceivedChunk struct {
	Data   []byte
	Index  int
	IsLast bool
}

func logInfo(format string, args ...interface{}) {
	fmt.Printf("[INFO] "+format+"\n", args...)
}

func logDebug(format string, args ...interface{}) {
	if debug {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

func logError(format string, args ...interface{}) {
	fmt.Printf("[ERROR] "+format+"\n", args...)
}

func logReceive(format string, args ...interface{}) {
	fmt.Printf("[RECV] "+format+"\n", args...)
}

func logComplete(format string, args ...interface{}) {
	fmt.Printf("[OK] "+format+"\n", args...)
}

func logReject(format string, args ...interface{}) {
	fmt.Printf("[REJECT] "+format+"\n", args...)
}

func main() {
	var outputDir string

	flag.StringVar(&outputDir, "o", "./received", "Output directory for received files")
	flag.BoolVar(&debug, "d", false, "Enable debug output")
	flag.Parse()

	logInfo("Connecting to nym-client on port %s", nymClientPort)

	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:"+nymClientPort, nil)
	if err != nil {
		logError("Cannot connect to nym-client: %v", err)
		logInfo("Make sure nym-client is running on port 1977")
		os.Exit(1)
	}
	defer conn.Close()

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

	logInfo("Waiting for incoming files...")
	logInfo("Maximum file size: %s", formatBytes(maxFileSize))
	logInfo("Output directory: %s", outputDir)
	logInfo("Press Ctrl+C to exit")
	fmt.Println()

	runReceiveMode(conn, outputDir)
}

func runReceiveMode(conn *websocket.Conn, outputDir string) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		logError("Creating output directory: %v", err)
		os.Exit(1)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println()
		logInfo("Shutting down...")
		os.Exit(0)
	}()

	var currentFile *os.File
	var currentFileName string
	var currentFileSize int64
	var totalChunks int
	var nextExpectedIndex int
	var chunksWritten int
	var receivedChunks map[int]ReceivedChunk
	var startTime time.Time
	var fileCount int

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

		logDebug("Message type: %v", data["type"])

		if data["type"] == "received" {
			message, _ := data["message"].(string)

			if len(message) == 0 {
				logDebug("Empty message received")
				continue
			}

			logDebug("Message content length: %d", len(message))

			var msgData map[string]interface{}
			if err := json.Unmarshal([]byte(message), &msgData); err != nil {
				logDebug("Failed to parse inner message: %v", err)
				handleRawData(message, outputDir, &currentFile,
					&currentFileName, &currentFileSize, &totalChunks,
					&nextExpectedIndex, &chunksWritten, &receivedChunks, &startTime)
				continue
			}

			msgType, _ := msgData["type"].(string)
			logDebug("Inner message type: %s", msgType)

			switch msgType {
			case "file_info":
				handleFileInfo(message, outputDir, &currentFile,
					&currentFileName, &currentFileSize, &totalChunks,
					&nextExpectedIndex, &chunksWritten, &receivedChunks, &startTime, &fileCount)

			case "file_chunk":
				handleFileChunkWithReorder(message, &currentFile,
					&currentFileName, currentFileSize, totalChunks,
					&nextExpectedIndex, &chunksWritten, &receivedChunks, &startTime)
			}
		}
	}
}

func handleRawData(rawData, outputDir string, currentFile **os.File,
	currentFileName *string, currentFileSize *int64, totalChunks *int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk, startTime *time.Time) {

	var fileInfo FileInfo
	if err := json.Unmarshal([]byte(rawData), &fileInfo); err == nil && fileInfo.Type == "file_info" {
		handleFileInfo(rawData, outputDir, currentFile, currentFileName,
			currentFileSize, totalChunks, nextExpectedIndex, chunksWritten, receivedChunks, startTime, nil)
		return
	}

	var chunk FileChunk
	if err := json.Unmarshal([]byte(rawData), &chunk); err == nil && chunk.Type == "file_chunk" {
		handleFileChunkWithReorder(rawData, currentFile, currentFileName,
			*currentFileSize, *totalChunks, nextExpectedIndex, chunksWritten, receivedChunks, startTime)
		return
	}
}

func handleFileInfo(message, outputDir string, currentFile **os.File,
	currentFileName *string, currentFileSize *int64, totalChunks *int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk, startTime *time.Time, fileCount *int) {

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
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk, startTime *time.Time) {

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
