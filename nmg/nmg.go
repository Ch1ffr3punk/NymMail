package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/smtp"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	smtpServer      = "localhost:25"
	maxEmailSize    = 64 * 1024
	configFile      = "blacklist.json"
	fixedFrom       = "Nym Mail <noreply@oc2mx.net>"
	messageIDDomain = "oc2mx.net"
	contactAddress  = "info@oc2mx.net"
	nymClientPort   = "1977"
)

type Config struct {
	BlockedEmails []string `json:"blocked_emails"`
}

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
	Data   string `json:"data"`
	IsLast bool   `json:"is_last"`
}

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
	config, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Connecting to nym-client on port %s\n", nymClientPort)

	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:"+nymClientPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot connect to nym-client: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Get self address for display
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

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println()
		fmt.Println("[INFO] Shutting down...")
		os.Exit(0)
	}()

	receiver := &FileReceiver{
		ReceivedChunks: make(map[int]FileChunk),
	}

	// Main receive loop
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

func processMessage(message string, receiver *FileReceiver, config *Config) error {
	// Try file_info
	var fileInfo FileInfo
	if err := json.Unmarshal([]byte(message), &fileInfo); err == nil && fileInfo.Type == "file_info" {
		return handleFileInfo(fileInfo, receiver)
	}

	// Try file_chunk
	var chunk FileChunk
	if err := json.Unmarshal([]byte(message), &chunk); err == nil && chunk.Type == "file_chunk" {
		return handleFileChunk(chunk, receiver, config)
	}

	return fmt.Errorf("unknown message type")
}

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

func handleFileChunk(chunk FileChunk, receiver *FileReceiver, config *Config) error {
	if receiver.TotalChunks == 0 {
		return fmt.Errorf("received chunk before file_info")
	}

	data, err := base64.StdEncoding.DecodeString(chunk.Data)
	if err != nil {
		return fmt.Errorf("failed to decode chunk: %v", err)
	}

	chunk.Data = string(data)
	receiver.ReceivedChunks[chunk.Index] = chunk

	for {
		nextChunk, exists := receiver.ReceivedChunks[receiver.NextExpected]
		if !exists {
			break
		}

		receiver.MessageData = append(receiver.MessageData, []byte(nextChunk.Data)...)
		receiver.ChunksWritten++
		delete(receiver.ReceivedChunks, receiver.NextExpected)
		receiver.NextExpected++

		if nextChunk.IsLast || receiver.ChunksWritten == receiver.TotalChunks {
			messageStr := string(receiver.MessageData)
			return processAndSendEmail(messageStr, config)
		}
	}

	return nil
}

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

// generateMessageID creates a unique Message-ID header
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

// formatUTCDate returns the current UTC time in email format
func formatUTCDate() string {
	return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 -0700")
}

// findUTF8CharBoundary finds a safe split point at or before maxPos
// that doesn't cut a multi-byte UTF-8 character in half
func findUTF8CharBoundary(data []byte, maxPos int) int {
	if maxPos >= len(data) {
		return len(data)
	}
	if maxPos <= 0 {
		return 0
	}
	
	// Check the byte just before maxPos
	b := data[maxPos-1]
	
	// ASCII (0xxxxxxx) or leading byte (11xxxxxx): safe to split after
	if (b & 0x80) == 0 || (b & 0xC0) == 0xC0 {
		return maxPos
	}
	
	// Continuation byte (10xxxxxx): walk back to find the character start
	pos := maxPos - 1
	for pos > 0 && (data[pos] & 0xC0) == 0x80 {
		pos--
	}
	
	// Return position at the start of this UTF-8 character
	return pos
}

// encodeMIMESubject encodes a subject as MIME encoded word using base64 (RFC 2047)
// with proper header folding using TAB (RFC 5322) and UTF-8-safe chunking
func encodeMIMESubject(subject string) string {
	utf8Bytes := []byte(subject)
	
	// Check if subject needs encoding (contains non-ASCII)
	needsEncoding := false
	for _, b := range utf8Bytes {
		if b > 127 {
			needsEncoding = true
			break
		}
	}
	
	if !needsEncoding {
		return subject
	}
	
	const (
		maxLineLen  = 78
		prefix      = "=?UTF-8?B?"
		suffix      = "?="
		prefixLen   = 10 // len("=?UTF-8?B?")
		suffixLen   = 2  // len("?=")
		subjectPref = 9  // len("Subject: ")
		foldCharLen = 1  // len("\t")
	)
	
	// Max base64 chars per encoded word (must be multiple of 4)
	maxBase64First := ((maxLineLen - subjectPref - prefixLen - suffixLen) / 4) * 4
	maxBase64Cont := ((maxLineLen - foldCharLen - prefixLen - suffixLen) / 4) * 4
	
	// Corresponding max UTF-8 bytes: base64_chars * 3 / 4
	maxBytesFirst := maxBase64First * 3 / 4 // 42
	maxBytesCont := maxBase64Cont * 3 / 4   // 48
	
	var result strings.Builder
	pos := 0
	first := true
	
	for pos < len(utf8Bytes) {
		maxBytes := maxBytesCont
		if first {
			maxBytes = maxBytesFirst
		}
		
		end := pos + maxBytes
		if end > len(utf8Bytes) {
			end = len(utf8Bytes)
		} else {
			// Find safe UTF-8 character boundary
			end = findUTF8CharBoundary(utf8Bytes, end)
			
			// Edge case: if we can't split (character too large), take the whole character
			if end == pos {
				end = pos + 1
				for end < len(utf8Bytes) && (utf8Bytes[end] & 0xC0) == 0x80 {
					end++
				}
			}
		}
		
		chunk := utf8Bytes[pos:end]
		encoded := base64.StdEncoding.EncodeToString(chunk)
		
		if !first {
			// RFC 5322 folding with TAB
			result.WriteString("\r\n\t")
		}
		first = false
		
		result.WriteString(prefix)
		result.WriteString(encoded)
		result.WriteString(suffix)
		
		pos = end
	}
	
	return result.String()
}

// decodeSingleMIMEWord decodes a single MIME encoded word
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
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return encoded
		}
		return string(decoded)
	default:
		return encoded
	}
}

// decodeMIMEEncodedSubject decodes MIME encoded subject
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

// normalizeLineEndings converts all line endings to CRLF
func normalizeLineEndings(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	return data
}

// modifyHeaders rewrites email headers for anonymity
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

	// First pass: collect all headers
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
		
		// Skip headers we're replacing
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
	
	// Build headers in correct order
	buffer.WriteString("From: " + fixedFrom + "\r\n")
	
	if hasTo && toContent != "" {
		buffer.WriteString(toContent + "\r\n")
	}
	
	// Comment header with proper folding (CRLF + space for continuation)
	// RFC 5322: folding = CRLF followed by at least one SP or HTAB
	buffer.WriteString("Comment: This message did not originate from the sender address above.\r\n")
	buffer.WriteString("\t It was sent anonymously via the Nym Mixnet.\r\n")
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
	
	// Add body - preserve original body lines with CRLF
	for scanner.Scan() {
		buffer.WriteString(scanner.Text() + "\r\n")
	}
	
	return buffer.Bytes()
}

// parseEmail extracts To and Subject headers, handling folded headers correctly
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

	// Extract email address from To header if present (handle angle brackets)
	toAddr := strings.TrimSpace(to.String())
	if idx := strings.Index(toAddr, "<"); idx != -1 {
		if idx2 := strings.Index(toAddr, ">"); idx2 != -1 {
			toAddr = strings.TrimSpace(toAddr[idx+1 : idx2])
		}
	}

	if toAddr == "" {
		return "", "", fmt.Errorf("could not find To: header")
	}
	
	// Decode MIME-encoded subject if necessary
	subjectStr := strings.TrimSpace(subject.String())
	return toAddr, decodeMIMEEncodedSubject(subjectStr), nil
}

func processAndSendEmail(emailContent string, config *Config) error {
	if len(emailContent) > maxEmailSize {
		return fmt.Errorf("email size exceeds %d KB", maxEmailSize/1024)
	}

	to, _, err := parseEmail(emailContent)
	if err != nil {
		return err
	}

	if config != nil {
		for _, blocked := range config.BlockedEmails {
			if strings.ToLower(strings.TrimSpace(blocked)) == strings.ToLower(to) {
				return fmt.Errorf("recipient blacklisted: %s", to)
			}
		}
	}

	normalized := normalizeLineEndings([]byte(emailContent))
	modified := modifyHeaders(normalized)

	fromEmail := fixedFrom
	if idx := strings.Index(fixedFrom, "<"); idx != -1 {
		if idx2 := strings.Index(fixedFrom, ">"); idx2 != -1 {
			fromEmail = fixedFrom[idx+1 : idx2]
		}
	}

	smtpConn, err := smtp.Dial(smtpServer)
	if err != nil {
		return fmt.Errorf("SMTP connection: %v", err)
	}
	defer smtpConn.Close()

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
