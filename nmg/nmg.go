// Server: Nym Mixnet to SMTP Gateway
// Receives messages from Nym Mixnet via WebSocket and forwards them via local SMTP.
// All comments are in English.

package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/smtp"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	smtpServer      = "localhost:25"          // Local SMTP server address
	maxEmailSize    = 64 * 1024               // Maximum email size in bytes (64 KB)
	configFile      = "blacklist.json"        // Configuration file path
	fixedFrom       = "Nym Mail <noreply@oc2mx.net>" // Fixed From header for anonymity
	messageIDDomain = "oc2mx.net"             // Domain for generated Message-ID headers
	contactAddress  = "info@oc2mx.net"        // Contact address for Comment header
	nymClientPort   = "1977"                  // WebSocket port for nym-client connection
)

// Config holds the application configuration loaded from JSON file.
type Config struct {
	BlockedEmails []string `json:"blocked_emails"` // List of blocked recipient addresses
}

// FileInfo describes metadata for an incoming file transfer.
type FileInfo struct {
	Type   string `json:"type"`   // Always "file_info"
	Name   string `json:"name"`   // Original filename
	Size   int64  `json:"size"`   // Total file size in bytes
	Chunks int    `json:"chunks"` // Total number of chunks
}

// FileChunk represents a single chunk of a split file transfer.
type FileChunk struct {
	Type   string `json:"type"`   // Always "file_chunk"
	Index  int    `json:"index"`  // Chunk index (0-based)
	Total  int    `json:"total"`  // Total chunks expected
	Data   string `json:"data"`   // Base64-encoded chunk data
	IsLast bool   `json:"is_last"` // True if this is the final chunk
}

// FileReceiver tracks state for reassembling multi-chunk file transfers.
type FileReceiver struct {
	CurrentFileName string
	CurrentFileSize int64
	TotalChunks     int
	NextExpected    int
	ChunksWritten   int
	ReceivedChunks  map[int]FileChunk
	MessageData     []byte
}

func main() {
	// Load configuration from file (non-fatal if missing)
	config, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Connect to nym-client via WebSocket
	fmt.Printf("Connecting to nym-client on port %s\n", nymClientPort)
	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:"+nymClientPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot connect to nym-client: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Request and display our own Nym address for reference
	req, _ := json.Marshal(map[string]string{"type": "selfAddress"})
	conn.WriteMessage(websocket.TextMessage, req)

	var resp map[string]interface{}
	conn.ReadJSON(&resp)

	if selfAddr, ok := resp["address"].(string); ok {
		shortAddr := selfAddr
		if len(shortAddr) > 20 {
			shortAddr = shortAddr[:20] + "..."
		}
		fmt.Printf("[INFO] Nym address: %s\n", shortAddr)
	}

	fmt.Println("[INFO] Waiting for incoming messages from Nym Mixnet...")
	fmt.Println("[INFO] Press Ctrl+C to exit")
	fmt.Println()

	// Handle graceful shutdown on SIGINT/SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println()
		fmt.Println("[INFO] Shutting down...")
		os.Exit(0)
	}()

	// Initialize file reassembly state
	receiver := &FileReceiver{
		ReceivedChunks: make(map[int]FileChunk),
	}

	// Main message receive loop
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Connection error: %v\n", err)
			break
		}

		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Failed to parse JSON: %v\n", err)
			continue
		}

		// Process only "received" type messages from nym-client
		if msgType, ok := data["type"].(string); ok && msgType == "received" {
			messageContent, ok := data["message"].(string)
			if !ok || len(messageContent) == 0 {
				continue
			}

			if err := processMessage(messageContent, receiver, config); err != nil {
				fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			}
		}
	}
}

// processMessage attempts to parse the message as FileInfo or FileChunk and dispatches accordingly.
func processMessage(message string, receiver *FileReceiver, config *Config) error {
	// Try parsing as file_info
	var fileInfo FileInfo
	if err := json.Unmarshal([]byte(message), &fileInfo); err == nil && fileInfo.Type == "file_info" {
		return handleFileInfo(fileInfo, receiver)
	}

	// Try parsing as file_chunk
	var chunk FileChunk
	if err := json.Unmarshal([]byte(message), &chunk); err == nil && chunk.Type == "file_chunk" {
		return handleFileChunk(chunk, receiver, config)
	}

	return fmt.Errorf("unknown message type")
}

// handleFileInfo initializes the FileReceiver state for a new file transfer.
func handleFileInfo(info FileInfo, receiver *FileReceiver) error {
	receiver.CurrentFileName = info.Name
	receiver.CurrentFileSize = info.Size
	receiver.TotalChunks = info.Chunks
	receiver.NextExpected = 0
	receiver.ChunksWritten = 0
	receiver.ReceivedChunks = make(map[int]FileChunk)
	receiver.MessageData = make([]byte, 0, info.Size)
	return nil
}

// handleFileChunk processes an incoming file chunk, reassembles if complete, and triggers email sending.
func handleFileChunk(chunk FileChunk, receiver *FileReceiver, config *Config) error {
	if receiver.TotalChunks == 0 {
		return fmt.Errorf("received chunk before file_info")
	}

	// Decode base64 chunk data
	data, err := base64.StdEncoding.DecodeString(chunk.Data)
	if err != nil {
		return fmt.Errorf("failed to decode chunk: %v", err)
	}

	// Store decoded data as string for reassembly
	chunk.Data = string(data)
	receiver.ReceivedChunks[chunk.Index] = chunk

	// Write chunks in order as they become available
	for {
		nextChunk, exists := receiver.ReceivedChunks[receiver.NextExpected]
		if !exists {
			break
		}

		receiver.MessageData = append(receiver.MessageData, []byte(nextChunk.Data)...)
		receiver.ChunksWritten++
		delete(receiver.ReceivedChunks, receiver.NextExpected)
		receiver.NextExpected++

		// If all chunks received or IsLast flag set, process and send email
		if nextChunk.IsLast || receiver.ChunksWritten == receiver.TotalChunks {
			messageStr := string(receiver.MessageData)
			return processAndSendEmail(messageStr, config)
		}
	}

	return nil
}

// loadConfig reads the blacklist configuration from JSON file.
// Returns empty config if file does not exist.
func loadConfig() (*Config, error) {
	config := &Config{BlockedEmails: []string{}}
	file, err := os.Open(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		return nil, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(config); err != nil {
		return nil, err
	}
	return config, nil
}

// generateMessageID creates a unique Message-ID header per RFC 5322.
func generateMessageID() string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	randomBytes := make([]byte, 21)
	_, _ = rand.Read(randomBytes)

	var randomPart strings.Builder
	randomPart.Grow(21)
	for _, b := range randomBytes {
		randomPart.WriteByte(chars[b%byte(len(chars))])
	}

	return fmt.Sprintf("<%s@%s>", randomPart.String(), messageIDDomain)
}

// formatUTCDate returns current UTC time in RFC 5322 email date format.
func formatUTCDate() string {
	return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 -0700")
}

// findUTF8CharBoundary finds a safe split point at or before maxPos
// that doesn't cut a multi-byte UTF-8 character in half.
func findUTF8CharBoundary(data []byte, maxPos int) int {
	if maxPos >= len(data) {
		return len(data)
	}
	if maxPos <= 0 {
		return 0
	}

	b := data[maxPos-1]

	// ASCII (0xxxxxxx) or leading byte (11xxxxxx): safe to split after
	if (b&0x80) == 0 || (b&0xC0) == 0xC0 {
		return maxPos
	}

	// Continuation byte (10xxxxxx): walk back to find character start
	pos := maxPos - 1
	for pos > 0 && (data[pos]&0xC0) == 0x80 {
		pos--
	}

	return pos
}

// foldASCIIHeaderLine applies RFC 5322 folding to a plain ASCII header value.
// prefixLen is the length of the header prefix (e.g., "Subject: ") for first-line calculation.
func foldASCIIHeaderLine(value string, maxLineLen, prefixLen int) string {
	totalFirstLine := prefixLen + len(value)
	if totalFirstLine <= maxLineLen {
		return value
	}

	var result strings.Builder
	pos := 0
	available := maxLineLen - prefixLen

	for pos < len(value) {
		end := pos + available
		if end >= len(value) {
			result.WriteString(value[pos:])
			break
		}

		// Prefer folding at word boundary (space)
		if spaceIdx := strings.LastIndex(value[pos:end], " "); spaceIdx != -1 {
			end = pos + spaceIdx + 1 // Include the space in this line
		}

		result.WriteString(value[pos:end])
		result.WriteString("\r\n ")
		pos = end
		available = maxLineLen - 2 // Continuation lines: reserve 2 chars for "\r\n "
	}

	return result.String()
}

// encodeMIMESubject encodes a subject line using RFC 2047 MIME encoded-word syntax
// with RFC 5322 header folding for long subjects.
// Returns a properly folded Subject header value (without the "Subject: " prefix).
func encodeMIMESubject(subject string) string {
	if subject == "" {
		return ""
	}

	// Check if encoding is needed (contains non-ASCII characters)
	needsEncoding := false
	for i := 0; i < len(subject); i++ {
		if subject[i] > 127 {
			needsEncoding = true
			break
		}
	}

	// Pure ASCII: no MIME encoding needed, just apply RFC 5322 folding if required
	if !needsEncoding {
		return foldASCIIHeaderLine(subject, 78, 9) // 9 = len("Subject: ")
	}

	// UTF-8 subject: encode in chunks to enable proper folding
	// Each chunk becomes a separate encoded-word, allowing safe line breaks
	utf8Bytes := []byte(subject)

	const (
		maxLineLen             = 78                            // RFC 5322 recommended max
		prefixContLen          = 3                             // len("\r\n ")
		encodedWordOverhead    = 12                            // len("=?UTF-8?B??=")
		maxEncodedPerChunk     = maxLineLen - prefixContLen - encodedWordOverhead // ~63
		maxBase64Len           = (maxEncodedPerChunk / 4) * 4  // Must be multiple of 4 for base64
		maxUTF8BytesPerChunk   = maxBase64Len * 3 / 4          // ~45 UTF-8 bytes per chunk
		prefixFirstLen         = 9                             // len("Subject: ")
		maxEncodedPerChunkFirst = maxLineLen - prefixFirstLen - encodedWordOverhead // ~57
		maxBase64LenFirst      = (maxEncodedPerChunkFirst / 4) * 4
		maxUTF8BytesPerChunkFirst = maxBase64LenFirst * 3 / 4  // ~42 UTF-8 bytes for first chunk
	)

	var result strings.Builder
	pos := 0
	first := true

	for pos < len(utf8Bytes) {
		// Calculate safe chunk boundary
		maxBytes := maxUTF8BytesPerChunk
		if first {
			maxBytes = maxUTF8BytesPerChunkFirst
		}

		end := pos + maxBytes
		if end > len(utf8Bytes) {
			end = len(utf8Bytes)
		} else {
			// Ensure we don't split a multi-byte UTF-8 character
			end = findUTF8CharBoundary(utf8Bytes, end)
			// Edge case: if boundary logic fails, take at least one character
			if end == pos {
				end = pos + 1
				for end < len(utf8Bytes) && (utf8Bytes[end]&0xC0) == 0x80 {
					end++
				}
			}
		}

		chunk := string(utf8Bytes[pos:end])
		encoded := mime.BEncoding.Encode("UTF-8", chunk)

		// Add folding whitespace before continuation encoded-words
		if !first {
			result.WriteString("\r\n ")
		}
		first = false
		result.WriteString(encoded)

		pos = end
	}

	return result.String()
}

// decodeSingleMIMEWord decodes a single RFC 2047 encoded-word (e.g. =?UTF-8?B?...?=).
func decodeSingleMIMEWord(encoded string) string {
	if !strings.HasPrefix(encoded, "=?") || !strings.HasSuffix(encoded, "?=") {
		return encoded
	}

	inner := encoded[2 : len(encoded)-2]
	parts := strings.SplitN(inner, "?", 3)
	if len(parts) != 3 {
		return encoded
	}

	charset := strings.ToUpper(parts[0])
	encoding := strings.ToUpper(parts[1])
	data := parts[2]

	if charset != "UTF-8" {
		return encoded
	}

	switch encoding {
	case "Q":
		// Quoted-printable decoding for MIME encoded-words
		var decoded strings.Builder
		for i := 0; i < len(data); i++ {
			if data[i] == '_' {
				decoded.WriteByte(' ')
			} else if data[i] == '=' && i+2 < len(data) {
				var b byte
				fmt.Sscanf(data[i+1:i+3], "%02x", &b)
				decoded.WriteByte(b)
				i += 2
			} else {
				decoded.WriteByte(data[i])
			}
		}
		return decoded.String()
	case "B":
		// Base64 decoding
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return encoded
		}
		return string(decoded)
	default:
		return encoded
	}
}

// decodeMIMEEncodedSubject decodes all MIME encoded-words in a subject header.
func decodeMIMEEncodedSubject(subject string) string {
	result := subject
	for {
		start := strings.Index(result, "=?")
		if start == -1 {
			break
		}
		end := strings.Index(result[start:], "?=")
		if end == -1 {
			break
		}
		end += start + 2

		encoded := result[start:end]
		decoded := decodeSingleMIMEWord(encoded)
		result = result[:start] + decoded + result[end:]
	}
	return result
}

// normalizeLineEndings converts all line endings to CRLF (\r\n) as required by SMTP.
func normalizeLineEndings(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	return data
}

// modifyHeaders rewrites email headers for anonymity while preserving essential structure.
// Adds Comment header, regenerates Message-ID and Date, and encodes Subject via MIME if needed.
func modifyHeaders(original []byte) []byte {
	var buffer bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(original))

	hasMimeVersion := false
	hasContentType := false
	hasContentTransferEncoding := false
	hasSubject := false
	hasReferences := false
	hasInReplyTo := false
	hasTo := false

	var subjectHeader strings.Builder
	var referencesHeader strings.Builder
	var inReplyToHeader strings.Builder
	var toHeader strings.Builder
	var otherHeaders strings.Builder

	var toContent, subjectContent, referencesContent, inReplyToContent string

	// First pass: collect and process headers
	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			break
		}

		lowerLine := strings.ToLower(line)

		if strings.HasPrefix(lowerLine, "to:") {
			hasTo = true
			toHeader.WriteString(line)
			continue
		}

		if strings.HasPrefix(lowerLine, "subject:") {
			hasSubject = true
			subjectPart := strings.TrimSpace(line[8:])
			decodedSubject := decodeMIMEEncodedSubject(subjectPart)
			encodedSubject := encodeMIMESubject(decodedSubject)
			subjectHeader.WriteString("Subject: " + encodedSubject)
			continue
		}

		if strings.HasPrefix(lowerLine, "references:") {
			hasReferences = true
			referencesHeader.WriteString(line)
			continue
		}

		if strings.HasPrefix(lowerLine, "in-reply-to:") {
			hasInReplyTo = true
			inReplyToHeader.WriteString(line)
			continue
		}

		// Skip headers we replace for anonymity
		if strings.HasPrefix(lowerLine, "from:") ||
			strings.HasPrefix(lowerLine, "message-id:") ||
			strings.HasPrefix(lowerLine, "date:") {
			continue
		}

		otherHeaders.WriteString(line + "\r\n")

		if strings.HasPrefix(lowerLine, "mime-version:") {
			hasMimeVersion = true
		}
		if strings.HasPrefix(lowerLine, "content-type:") {
			hasContentType = true
		}
		if strings.HasPrefix(lowerLine, "content-transfer-encoding:") {
			hasContentTransferEncoding = true
		}
	}

	if hasTo {
		toContent = toHeader.String()
	}
	if hasSubject {
		subjectContent = subjectHeader.String()
	}
	if hasReferences {
		referencesContent = referencesHeader.String()
	}
	if hasInReplyTo {
		inReplyToContent = inReplyToHeader.String()
	}

	// Build headers in RFC 5322 recommended order
	buffer.WriteString("From: " + fixedFrom + "\r\n")

	if hasTo && toContent != "" {
		buffer.WriteString(toContent + "\r\n")
	}

	// Anonymity notice with proper folding (CRLF + space per RFC 5322)
	buffer.WriteString("Comment: This message did not originate from the sender address above.\r\n")
	buffer.WriteString(" It was sent anonymously via the Nym Mixnet.\r\n")
	buffer.WriteString("Contact: " + contactAddress + "\r\n")

	if hasSubject && subjectContent != "" {
		buffer.WriteString(subjectContent + "\r\n")
	}

	if hasReferences && referencesContent != "" {
		buffer.WriteString(referencesContent + "\r\n")
	}

	if hasInReplyTo && inReplyToContent != "" {
		buffer.WriteString(inReplyToContent + "\r\n")
	}

	buffer.WriteString("Message-ID: " + generateMessageID() + "\r\n")
	buffer.WriteString("Date: " + formatUTCDate() + "\r\n")

	buffer.WriteString(otherHeaders.String())

	// Add default MIME headers if missing
	if !hasMimeVersion {
		buffer.WriteString("MIME-Version: 1.0\r\n")
	}
	if !hasContentType {
		buffer.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	}
	if !hasContentTransferEncoding {
		buffer.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	}

	buffer.WriteString("\r\n")

	// Append body with normalized CRLF line endings
	for scanner.Scan() {
		buffer.WriteString(scanner.Text() + "\r\n")
	}

	return buffer.Bytes()
}

// parseEmail extracts To and Subject headers from raw email content.
// Handles folded headers and MIME-encoded subjects.
func parseEmail(emailContent string) (string, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(emailContent))
	var to, subject strings.Builder
	var lastHeaderName string

	for scanner.Scan() {
		line := scanner.Text()

		// Handle folded headers (continuation lines start with SP or HTAB)
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if lastHeaderName == "subject" {
				subject.WriteString(strings.TrimSpace(line))
				subject.WriteString(" ")
			}
			continue
		}

		lowerLine := strings.ToLower(line)

		if strings.HasPrefix(lowerLine, "to:") {
			lastHeaderName = "to"
			to.WriteString(strings.TrimSpace(line[3:]))
		} else if strings.HasPrefix(lowerLine, "subject:") {
			lastHeaderName = "subject"
			subject.WriteString(strings.TrimSpace(line[8:]))
		} else {
			lastHeaderName = ""
		}
	}

	// Extract email address from To header (handle angle brackets)
	toAddr := strings.TrimSpace(to.String())
	if idx := strings.Index(toAddr, "<"); idx != -1 {
		if idx2 := strings.Index(toAddr, ">"); idx2 != -1 {
			toAddr = strings.TrimSpace(toAddr[idx+1 : idx2])
		}
	}

	if toAddr == "" {
		return "", "", fmt.Errorf("could not find To: header")
	}

	// Decode MIME-encoded subject if present
	subjectStr := strings.TrimSpace(subject.String())
	return toAddr, decodeMIMEEncodedSubject(subjectStr), nil
}

// processAndSendEmail validates, anonymizes, and sends the email via local SMTP.
func processAndSendEmail(emailContent string, config *Config) error {
	if len(emailContent) > maxEmailSize {
		return fmt.Errorf("email size exceeds %d KB", maxEmailSize/1024)
	}

	to, _, err := parseEmail(emailContent)
	if err != nil {
		return err
	}

	// Check recipient against blacklist
	if config != nil {
		for _, blocked := range config.BlockedEmails {
			if strings.ToLower(strings.TrimSpace(blocked)) == strings.ToLower(to) {
				return fmt.Errorf("recipient blacklisted: %s", to)
			}
		}
	}

	// Normalize line endings and rewrite headers for anonymity
	normalized := normalizeLineEndings([]byte(emailContent))
	modified := modifyHeaders(normalized)

	// Extract sender address from fixedFrom for SMTP MAIL FROM
	fromEmail := fixedFrom
	if idx := strings.Index(fixedFrom, "<"); idx != -1 {
		if idx2 := strings.Index(fixedFrom, ">"); idx2 != -1 {
			fromEmail = fixedFrom[idx+1 : idx2]
		}
	}

	// Connect to local SMTP server
	smtpConn, err := smtp.Dial(smtpServer)
	if err != nil {
		return fmt.Errorf("SMTP connection: %v", err)
	}
	defer smtpConn.Close()

	// SMTP transaction: MAIL FROM, RCPT TO, DATA
	if err := smtpConn.Mail(fromEmail); err != nil {
		return fmt.Errorf("MAIL FROM: %v", err)
	}
	if err := smtpConn.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO: %v", err)
	}

	w, err := smtpConn.Data()
	if err != nil {
		return fmt.Errorf("DATA: %v", err)
	}
	defer w.Close()

	if _, err := w.Write(modified); err != nil {
		return fmt.Errorf("write: %v", err)
	}

	return nil
}
