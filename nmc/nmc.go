package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	connectionTimeout = 30 * time.Second
	chunkSize         = 1024 * 32
	maxFileSize       = 24 * 1024 * 1024
	nymClientPort     = "1977"
	configFileName    = "nmc.json"
)

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

type TransferResult struct {
	Recipient string
	Success   bool
	Error     string
	Duration  time.Duration
}

type Config struct {
	Aliases map[string]string `json:"aliases"`
}

func main() {
	var recipients string
	var configFile string
	var parallel bool
	var createConfig bool

	flag.StringVar(&recipients, "t", "", "Recipient(s) - comma separated (nym addresses or aliases)")
	flag.StringVar(&configFile, "c", "", "Config file path (default: platform-specific config directory/nmc.json)")
	flag.BoolVar(&parallel, "p", false, "Send to multiple recipients in parallel (default: sequential)")
	flag.BoolVar(&createConfig, "init", false, "Create a dummy config file with placeholder aliases")
	flag.Parse()

	if createConfig {
		if err := createDummyConfig(configFile); err != nil {
			fmt.Printf("[ERROR] Creating config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[INFO] Dummy config created at: %s\n", getConfigPath(configFile))
		fmt.Println("[INFO] Please edit the file and add your aliases")
		os.Exit(0)
	}

	if recipients == "" {
		fmt.Println("Usage:")
		fmt.Println("  Send: nmc -t <recipient1,recipient2,...> [-c nmc.json] [-p] <file>")
		fmt.Println("  Init: nmc -init")
		fmt.Println("\nRecipients can be nym addresses or aliases from config file")
		fmt.Println("Example: client -t bob@mix.nym,alice@mix.nym file.txt")
		os.Exit(1)
	}

	if len(flag.Args()) != 1 {
		fmt.Println("Error: Exactly one file must be specified")
		os.Exit(1)
	}

	filePath := flag.Arg(0)

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		fmt.Printf("[ERROR] Reading file: %v\n", err)
		os.Exit(1)
	}

	if fileInfo.Size() > maxFileSize {
		fmt.Printf("[ERROR] File size (%s) exceeds maximum allowed size of 24 MB\n",
			formatBytes(fileInfo.Size()))
		os.Exit(1)
	}

	config, err := loadConfig(configFile)
	if err != nil {
		fmt.Printf("[ERROR] Loading config: %v\n", err)
		fmt.Println("[INFO] Use -init to create a dummy config file")
		os.Exit(1)
	}

	recipientList := strings.Split(recipients, ",")
	resolvedRecipients := make([]string, 0, len(recipientList))

	for _, r := range recipientList {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}

		resolved, err := resolveRecipient(r, config.Aliases)
		if err != nil {
			fmt.Printf("[ERROR] %v\n", err)
			os.Exit(1)
		}
		resolvedRecipients = append(resolvedRecipients, resolved)
	}

	if len(resolvedRecipients) == 0 {
		fmt.Println("[ERROR] No valid recipients specified")
		os.Exit(1)
	}

	fmt.Printf("[INFO] Sending to %d recipient(s)\n", len(resolvedRecipients))
	for i, r := range resolvedRecipients {
		shortAddr := r
		if len(r) > 20 {
			shortAddr = r[:20] + "..."
		}
		fmt.Printf("  %d. %s\n", i+1, shortAddr)
	}
	fmt.Printf("[INFO] File: %s (%s)\n", filepath.Base(filePath), formatBytes(fileInfo.Size()))

	if parallel {
		fmt.Printf("[INFO] Sending in parallel mode\n")
		sendToMultipleRecipientsParallel(resolvedRecipients, filePath)
	} else {
		fmt.Printf("[INFO] Sending in sequential mode\n")
		sendToMultipleRecipientsSequential(resolvedRecipients, filePath)
	}
}

func sendToMultipleRecipientsSequential(recipients []string, filePath string) {
	successCount := 0
	failCount := 0

	for i, recipient := range recipients {
		shortAddr := recipient
		if len(recipient) > 20 {
			shortAddr = recipient[:20] + "..."
		}
		fmt.Printf("\n[%d/%d] Sending to %s\n", i+1, len(recipients), shortAddr)

		err := sendToRecipient(recipient, filePath)
		if err != nil {
			fmt.Printf("[ERROR] Failed to send to %s: %v\n", shortAddr, err)
			failCount++
		} else {
			fmt.Printf("[SUCCESS] Sent to %s\n", shortAddr)
			successCount++
		}
	}

	fmt.Printf("\n[SUMMARY] Complete: %d successful, %d failed\n", successCount, failCount)
}

func sendToMultipleRecipientsParallel(recipients []string, filePath string) {
	var wg sync.WaitGroup
	results := make(chan TransferResult, len(recipients))

	startTime := time.Now()

	for _, recipient := range recipients {
		wg.Add(1)
		go func(rec string) {
			defer wg.Done()

			start := time.Now()
			err := sendToRecipient(rec, filePath)
			duration := time.Since(start)

			result := TransferResult{
				Recipient: rec,
				Success:   err == nil,
				Duration:  duration,
			}
			if err != nil {
				result.Error = err.Error()
			}
			results <- result
		}(recipient)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	successCount := 0
	failCount := 0

	for result := range results {
		shortAddr := result.Recipient
		if len(shortAddr) > 20 {
			shortAddr = shortAddr[:20] + "..."
		}

		if result.Success {
			fmt.Printf("[SUCCESS] %s in %v\n", shortAddr, result.Duration.Round(time.Millisecond))
			successCount++
		} else {
			fmt.Printf("[FAILED] %s: %v\n", shortAddr, result.Error)
			failCount++
		}
	}

	totalDuration := time.Since(startTime)
	fmt.Printf("\n[SUMMARY] %d successful, %d failed in %v\n",
		successCount, failCount, totalDuration.Round(time.Millisecond))
}

func sendToRecipient(recipient string, filePath string) error {
	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:"+nymClientPort, nil)
	if err != nil {
		return fmt.Errorf("connecting to nym-client: %v", err)
	}
	defer conn.Close()

	req, _ := json.Marshal(map[string]string{"type": "selfAddress"})
	conn.WriteMessage(websocket.TextMessage, req)

	var resp map[string]interface{}
	conn.ReadJSON(&resp)

	if err := sendFile(conn, recipient, filePath); err != nil {
		return fmt.Errorf("sending file: %v", err)
	}

	return nil
}

func sendFile(conn *websocket.Conn, recipient string, filePath string) error {
	fileInfo, err := getFileInfo(filePath)
	if err != nil {
		return err
	}

	fileInfo.Type = "file_info"
	if err := sendRawJSON(conn, recipient, fileInfo); err != nil {
		return fmt.Errorf("sending file info: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	if err := sendFileChunks(conn, recipient, filePath, fileInfo); err != nil {
		return fmt.Errorf("sending chunks: %v", err)
	}

	return nil
}

func sendFileChunks(conn *websocket.Conn, recipient, filePath string, fileInfo FileInfo) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	buffer := make([]byte, chunkSize)
	chunkIndex := 0

	for {
		n, err := file.Read(buffer)
		if err != nil && err != io.EOF {
			return err
		}

		if n == 0 {
			break
		}

		chunk := FileChunk{
			Type:   "file_chunk",
			Index:  chunkIndex,
			Total:  fileInfo.Chunks,
			Data:   make([]byte, n),
			IsLast: err == io.EOF,
		}
		copy(chunk.Data, buffer[:n])

		if err := sendRawJSON(conn, recipient, chunk); err != nil {
			return err
		}

		chunkIndex++
	}

	return nil
}

func sendRawJSON(conn *websocket.Conn, recipient string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	message := map[string]interface{}{
		"type":          "send",
		"recipient":     recipient,
		"message":       string(jsonData),
		"withReplySurb": false,
	}

	msgBytes, err := json.Marshal(message)
	if err != nil {
		return err
	}

	conn.SetWriteDeadline(time.Now().Add(connectionTimeout))
	err = conn.WriteMessage(websocket.TextMessage, msgBytes)
	conn.SetWriteDeadline(time.Time{})

	return err
}

func getFileInfo(filePath string) (FileInfo, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return FileInfo{}, err
	}

	chunks := int((info.Size() + chunkSize - 1) / chunkSize)

	fileInfo := FileInfo{
		Name:   filepath.Base(filePath),
		Size:   info.Size(),
		Chunks: chunks,
	}

	return fileInfo, nil
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

func getConfigPath(configFile string) string {
	if configFile != "" {
		return configFile
	}
	configDir := getConfigDir()
	return filepath.Join(configDir, configFileName)
}

func getConfigDir() string {
	if configDir := os.Getenv("XDG_CONFIG_HOME"); configDir != "" {
		return filepath.Join(configDir, "NymMail")
	}
	if homeDir, err := os.UserHomeDir(); err == nil {
		switch runtime.GOOS {
		case "windows":
			return filepath.Join(homeDir, "AppData", "Roaming", "NymMail")
		default:
			return filepath.Join(homeDir, ".config", "NymMail")
		}
	}
	return "."
}

func createDummyConfig(configFile string) error {
	configPath := getConfigPath(configFile)
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %v", err)
	}

	dummyConfig := Config{
		Aliases: map[string]string{
			"ch1ffr3punk@mix.nym": "83D4Uc5R2dL2eomXdn8hXz99e3yBFXG57XprphFTcx8s.HGV4Tn5mJMvU9gx6p6dBdBtLQbxdJXS1QQvUxUDDy2t@7SnUJy4rWH9hXCitpgwx7XoK5PGRBNjaiz7BWeaqRfXx",
			"alice@nym.example": "83D4Uc5R2dL2eomXdn8hXz99e3yBFXG57XprphFTcx8s.HGV4Tn5mJMvU9gx6p6dBdBtLQbxdJXS1QQvUxUDDy2t@7SnUJy4rWH9hXCitpgwx7XoK5PGRBNjaiz7BWeaqRfXx",
			"bob@nym.example": "83D4Uc5R2dL2eomXdn8hXz99e3yBFXG57XprphFTcx8s.HGV4Tn5mJMvU9gx6p6dBdBtLQbxdJXS1QQvUxUDDy2t@7SnUJy4rWH9hXCitpgwx7XoK5PGRBNjaiz7BWeaqRfXx",
		},
	}

	data, err := json.MarshalIndent(dummyConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("writing config: %v", err)
	}
	return nil
}

func loadConfig(configFile string) (*Config, error) {
	configPath := getConfigPath(configFile)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found: %s", configPath)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file: %v", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("invalid config file: %v", err)
	}
	return &config, nil
}

func resolveRecipient(recipient string, aliases map[string]string) (string, error) {
	if addr, exists := aliases[recipient]; exists {
		return addr, nil
	}
	if len(recipient) >= 50 {
		return recipient, nil
	}
	return "", fmt.Errorf("unknown recipient '%s'", recipient)
}
