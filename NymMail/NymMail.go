package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/gorilla/websocket"
)

const (
	connectionTimeout = 30 * time.Second
	chunkSize         = 1024 * 32
	maxMessageSize    = 64 * 1024 // 64KB max message size
	nymClientPort     = "1977"
	configFileName    = "NymMail.json"
	maxRetries        = 3
	retryDelay        = 1 * time.Second
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

// UnifiedConfig holds all configuration for NymMail
type UnifiedConfig struct {
	// Home server configuration for fetching emails
	HomeServer string `json:"home_server"`
	Password   string `json:"password"`
	LocalDir   string `json:"local_dir"`
	
	// Aliases for sending emails
	Aliases map[string]string `json:"aliases"`
}

// ConnectionPool manages a single shared connection for multiple transfers
type ConnectionPool struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

var pool = &ConnectionPool{}

func (p *ConnectionPool) getConnection() (*websocket.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn != nil {
		err := p.conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
		if err == nil {
			return p.conn, nil
		}
		p.conn.Close()
		p.conn = nil
	}

	var conn *websocket.Conn
	var err error

	for i := 0; i < maxRetries; i++ {
		conn, _, err = websocket.DefaultDialer.Dial("ws://localhost:"+nymClientPort, nil)
		if err == nil {
			p.conn = conn
			return conn, nil
		}

		if i < maxRetries-1 {
			time.Sleep(retryDelay)
		}
	}

	return nil, fmt.Errorf("connecting to nym-client after %d attempts: %v", maxRetries, err)
}

func (p *ConnectionPool) releaseConnection() {
	// Keep connection for reuse
}

func (p *ConnectionPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
}

// NymMessageGUI represents the GUI application
type NymMessageGUI struct {
	app          fyne.App
	window       fyne.Window
	toEntry      *widget.Entry
	messageArea  *widget.Entry
	statusLabel  *widget.Label
	statusDetail *widget.Label
	config       *UnifiedConfig
	isDarkTheme  bool
	themeSwitch  *widget.Button
	infoBtn      *widget.Button
	accountBtn   *widget.Button
}

func main() {
	// Load or create unified config automatically
	config, err := loadOrCreateUnifiedConfig()
	if err != nil {
		fmt.Printf("[ERROR] Config error: %v\n", err)
		os.Exit(1)
	}

	// Start GUI
	myApp := app.New()
	window := myApp.NewWindow("NymMail")

	gui := &NymMessageGUI{
		app:         myApp,
		window:      window,
		config:      config,
		isDarkTheme: true,
	}

	// Set initial theme
	myApp.Settings().SetTheme(theme.DarkTheme())

	// Build UI
	content := gui.setupUI()
	window.SetContent(content)
	window.Resize(fyne.NewSize(640, 480))
	window.CenterOnScreen()

	window.ShowAndRun()
}

func (g *NymMessageGUI) setupUI() fyne.CanvasObject {
	// Create input fields
	toEntry := widget.NewEntry()
	toEntry.PlaceHolder = "recipient1,recipient2,..."
	toEntry.Wrapping = fyne.TextTruncate

	// Create text area with word wrapping
	textArea := widget.NewMultiLineEntry()
	textArea.Wrapping = fyne.TextWrapWord
	textArea.PlaceHolder = "Enter your message here..."

	// Store references
	g.toEntry = toEntry
	g.messageArea = textArea

	// Action buttons
	sendButton := widget.NewButton("Send", func() {
		g.sendMessage()
	})
	sendButton.Importance = widget.HighImportance

	getButton := widget.NewButton("Get", func() {
		g.getEmails()
	})
	getButton.Importance = widget.HighImportance

	clearButton := widget.NewButton("Clear", func() {
		g.toEntry.SetText("")
		g.messageArea.SetText("")
		g.updateStatus("Ready", "All content cleared")
	})
	clearButton.Importance = widget.MediumImportance

	// Button container with 3-column grid (centered)
	buttonContainer := container.New(
		layout.NewGridLayoutWithColumns(3),
		sendButton,
		getButton,
		clearButton,
	)

	// Status labels
	g.statusLabel = widget.NewLabel("Ready")
	g.statusLabel.TextStyle = fyne.TextStyle{Bold: true}
	g.statusDetail = widget.NewLabel("NymMail ready")
	g.statusDetail.Wrapping = fyne.TextWrapWord

	// Status bar container
	statusBar := container.NewVBox(
		widget.NewSeparator(),
		container.NewHBox(
			g.statusLabel,
			layout.NewSpacer(),
		),
		g.statusDetail,
	)

	// Theme switch button
	g.themeSwitch = widget.NewButton("☀️", g.toggleTheme)
	g.themeSwitch.Importance = widget.LowImportance

	// Account button (top-center)
	g.accountBtn = widget.NewButtonWithIcon("", theme.AccountIcon(), g.showAccountPopup)
	g.accountBtn.Importance = widget.LowImportance

	// Info button (top-left)
	g.infoBtn = widget.NewButtonWithIcon("", theme.InfoIcon(), g.showInfoPopup)
	g.infoBtn.Importance = widget.LowImportance

	// Top bar with Info button (left), Account button (center), and Theme button (right)
	topBar := container.NewHBox(
		g.infoBtn,
		layout.NewSpacer(),
		g.accountBtn,
		layout.NewSpacer(),
		g.themeSwitch,
	)

	// Header section with compact fields
	headerSection := container.NewVBox(
		topBar,
		widget.NewSeparator(),
		g.createCompactField("To", toEntry),
		widget.NewSeparator(),
	)

	// Bottom section: buttons and status
	bottomSection := container.NewVBox(
		buttonContainer,
		widget.NewSeparator(),
		statusBar,
	)

	// MAIN LAYOUT: Border layout for responsive sizing
	mainContent := container.NewBorder(
		headerSection, // top
		bottomSection, // bottom
		nil, nil,      // no left/right
		textArea,      // center - expands automatically
	)

	// Scroll container for overflow handling
	mainScroll := container.NewScroll(mainContent)

	// Apply adaptive padding
	padding := g.getAdaptivePadding()
	paddedContent := container.New(
		layout.NewCustomPaddedLayout(padding, padding, padding, padding),
		mainScroll,
	)

	return paddedContent
}

// createCompactField creates a field with label next to entry
func (g *NymMessageGUI) createCompactField(labelText string, entry fyne.CanvasObject) fyne.CanvasObject {
	labelWidget := widget.NewLabel(labelText + ":")
	labelWidget.TextStyle = fyne.TextStyle{Bold: true}
	labelWidget.Alignment = fyne.TextAlignTrailing

	maxLabelWidth := g.getMaxLabelWidth()
	return container.NewBorder(
		nil, nil,
		container.New(&fixedWidthLayout{width: maxLabelWidth}, labelWidget),
		nil,
		entry,
	)
}

// getMaxLabelWidth calculates the maximum width needed for all field labels
func (g *NymMessageGUI) getMaxLabelWidth() float32 {
	labels := []string{"To:"}
	var maxWidth float32 = 0
	for _, labelText := range labels {
		tmpLabel := widget.NewLabel(labelText)
		tmpLabel.TextStyle = fyne.TextStyle{Bold: true}
		width := tmpLabel.MinSize().Width
		if width > maxWidth {
			maxWidth = width
		}
	}
	return maxWidth + 5 // Add small padding
}

// getAdaptivePadding returns padding values based on app scale setting
func (g *NymMessageGUI) getAdaptivePadding() float32 {
	scale := fyne.CurrentApp().Settings().Scale()
	padding := float32(8) * scale
	if padding < 6 {
		return 6
	}
	if padding > 16 {
		return 16
	}
	return padding
}

// fixedWidthLayout for consistent label widths
type fixedWidthLayout struct {
	width float32
}

func (f *fixedWidthLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	if len(objects) > 0 {
		objects[0].Resize(fyne.NewSize(f.width, objects[0].MinSize().Height))
		objects[0].Move(fyne.NewPos(0, 0))
	}
}

func (f *fixedWidthLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	if len(objects) > 0 {
		return fyne.NewSize(f.width, objects[0].MinSize().Height)
	}
	return fyne.NewSize(f.width, 0)
}

// generateRandomFilename creates a random filename for the message
func generateRandomFilename() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes) + ".txt"
}

func (g *NymMessageGUI) sendMessage() {
    recipientsStr := strings.TrimSpace(g.toEntry.Text)
    if recipientsStr == "" {
        dialog.ShowError(fmt.Errorf("recipient cannot be empty"), g.window)
        return
    }

    message := g.messageArea.Text
    messageSize := len([]byte(message))

    if messageSize == 0 {
        dialog.ShowError(fmt.Errorf("message cannot be empty"), g.window)
        return
    }

    if messageSize > maxMessageSize {
        dialog.ShowError(fmt.Errorf("message size (%d bytes) exceeds maximum allowed size of 64 KB", messageSize), g.window)
        return
    }

    // Parse recipients but keep original aliases for display
    recipientsStr = strings.TrimSpace(g.toEntry.Text)
    parts := strings.Split(recipientsStr, ",")
    var originalRecipients []string
    var resolvedRecipients []string
    var invalidRecipients []string

    for _, part := range parts {
        recipient := strings.TrimSpace(part)
        if recipient == "" {
            continue
        }
        
        // Store original for display
        originalRecipients = append(originalRecipients, recipient)
        
        // Resolve for sending - only checks aliases in NymMail.json
        resolved, err := resolveRecipient(recipient, g.config.Aliases)
        if err != nil {
            invalidRecipients = append(invalidRecipients, recipient)
        } else {
            resolvedRecipients = append(resolvedRecipients, resolved)
        }
    }

    if len(invalidRecipients) > 0 {
        dialog.ShowError(fmt.Errorf("invalid recipient(s): %s", strings.Join(invalidRecipients, ", ")), g.window)
        return
    }

    if len(resolvedRecipients) == 0 {
        dialog.ShowError(fmt.Errorf("no valid recipients specified"), g.window)
        return
    }

    totalRecipients := len(resolvedRecipients)
    
    // Show initial status
    g.updateStatus("Sending", fmt.Sprintf("Sending to %d recipient(s)...", totalRecipients))

    // Send in goroutine to not block UI
    go func() {
        successCount := 0
        failCount := 0
        var errors []string

        for i := 0; i < totalRecipients; i++ {
            original := originalRecipients[i]
            resolved := resolvedRecipients[i]
            
            // Shorten for display if needed
            displayAddr := original
            if len(displayAddr) > 20 {
                displayAddr = displayAddr[:20] + "..."
            }
            
            // Update status before sending
            fyne.Do(func() {
                g.statusLabel.SetText("Sending")
                g.statusDetail.SetText(fmt.Sprintf("[%d/%d] Sending to %s...", i+1, totalRecipients, displayAddr))
            })
            
            // Send the message using resolved address
            err := g.sendMessageToRecipient(resolved, message)
            
            if err != nil {
                failCount++
                errors = append(errors, fmt.Sprintf("%s: %v", original, err))
                fyne.Do(func() {
                    g.statusDetail.SetText(fmt.Sprintf("[%d/%d] Failed: %s", i+1, totalRecipients, displayAddr))
                })
            } else {
                successCount++
                fyne.Do(func() {
                    g.statusDetail.SetText(fmt.Sprintf("[%d/%d] Sent: %s", i+1, totalRecipients, displayAddr))
                })
            }
            
            // Small delay to see status
            time.Sleep(500 * time.Millisecond)
        }

        // Final status update
        fyne.Do(func() {
            if failCount == 0 {
                g.statusLabel.SetText("Success")
                g.statusDetail.SetText(fmt.Sprintf("✓ All %d messages sent successfully", successCount))
            } else {
                g.statusLabel.SetText("Error")
                g.statusDetail.SetText(fmt.Sprintf("Sent: %d, Failed: %d", successCount, failCount))
                dialog.ShowError(fmt.Errorf("failed to send to %d recipient(s):\n%s", failCount, strings.Join(errors, "\n")), g.window)
            }
        })
    }()
}

func (g *NymMessageGUI) sendMessageToRecipient(recipient string, message string) error {
	// Get connection from pool
	conn, err := pool.getConnection()
	if err != nil {
		return err
	}

	// Send selfAddress request
	req, _ := json.Marshal(map[string]string{"type": "selfAddress"})
	if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
		pool.mu.Lock()
		if pool.conn == conn {
			pool.conn = nil
		}
		pool.mu.Unlock()
		return fmt.Errorf("sending selfAddress request: %v", err)
	}

	var resp map[string]interface{}
	conn.SetReadDeadline(time.Now().Add(connectionTimeout))
	if err := conn.ReadJSON(&resp); err != nil {
		pool.mu.Lock()
		if pool.conn == conn {
			pool.conn = nil
		}
		pool.mu.Unlock()
		return fmt.Errorf("reading selfAddress response: %v", err)
	}
	conn.SetReadDeadline(time.Time{})

	// Send message as file with random filename
	if err := g.sendMessageViaConnection(conn, recipient, message); err != nil {
		return fmt.Errorf("sending message: %v", err)
	}

	return nil
}

func (g *NymMessageGUI) sendMessageViaConnection(conn *websocket.Conn, recipient string, message string) error {
	// Generate random filename
	filename := generateRandomFilename()
	messageBytes := []byte(message)
	chunks := (len(messageBytes) + chunkSize - 1) / chunkSize

	// Send file info
	fileInfo := FileInfo{
		Type:   "file_info",
		Name:   filename,
		Size:   int64(len(messageBytes)),
		Chunks: chunks,
	}

	if err := sendRawJSON(conn, recipient, fileInfo); err != nil {
		return fmt.Errorf("sending file info: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Send chunks
	for i := 0; i < chunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(messageBytes) {
			end = len(messageBytes)
		}

		chunk := FileChunk{
			Type:   "file_chunk",
			Index:  i,
			Total:  chunks,
			Data:   messageBytes[start:end],
			IsLast: i == chunks-1,
		}

		if err := sendRawJSON(conn, recipient, chunk); err != nil {
			return fmt.Errorf("sending chunk %d: %v", i, err)
		}
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

func (g *NymMessageGUI) updateStatus(status string, detail string) {
	fyne.Do(func() {
		g.statusLabel.SetText(status)
		if len(detail) > 76 {
			detail = wrapText(detail, 76)
		}
		g.statusDetail.SetText(detail)
	})
}

// wrapText wraps text to a specified maximum line length
func wrapText(text string, maxLineLength int) string {
	if len(text) <= maxLineLength {
		return text
	}
	var result strings.Builder
	words := strings.Fields(text)
	currentLineLength := 0
	for i, word := range words {
		if i > 0 {
			word = " " + word
		}
		if currentLineLength+len(word) > maxLineLength && currentLineLength > 0 {
			result.WriteString("\n")
			result.WriteString(word[1:])
			currentLineLength = len(word) - 1
		} else {
			result.WriteString(word)
			currentLineLength += len(word)
		}
	}
	return result.String()
}

func (g *NymMessageGUI) toggleTheme() {
	fyne.Do(func() {
		if g.isDarkTheme {
			g.app.Settings().SetTheme(theme.LightTheme())
			g.isDarkTheme = false
			g.themeSwitch.SetText("🌙")
		} else {
			g.app.Settings().SetTheme(theme.DarkTheme())
			g.isDarkTheme = true
			g.themeSwitch.SetText("☀️")
		}
	})
}

func (g *NymMessageGUI) showInfoPopup() {
	projURL, _ := url.Parse("https://github.com/Ch1ffr3punk/NymMail")

	projectLink := widget.NewHyperlink("An Open Source project", projURL)

	okButton := widget.NewButton("OK", func() {
		overlays := g.window.Canvas().Overlays()
		if overlays.Top() != nil {
			overlays.Remove(overlays.Top())
		}
	})
	okButton.Importance = widget.HighImportance

	content := container.NewVBox(
		widget.NewLabelWithStyle("NymMail v0.1.0", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		container.NewHBox(
			layout.NewSpacer(),
			projectLink,
			layout.NewSpacer(),
		),
		widget.NewLabelWithStyle("released under the Apache 2.0 license", fyne.TextAlignCenter, fyne.TextStyle{}),
		widget.NewLabelWithStyle("© 2026 Ch1ffr3punk", fyne.TextAlignCenter, fyne.TextStyle{}),
		widget.NewLabel(""),
		container.NewHBox(
			layout.NewSpacer(),
			okButton,
			layout.NewSpacer(),
		),
	)

	dialog.ShowCustomWithoutButtons("", content, g.window)
}

// showAccountPopup displays the account management window for managing aliases
func (g *NymMessageGUI) showAccountPopup() {
	// Create a new window for account management
	accountWindow := g.app.NewWindow("Nym Address Management")
	accountWindow.Resize(fyne.NewSize(480, 320))
	accountWindow.CenterOnScreen()

	// Track selected item
	var selectedIndex int = -1
	
	// Create a list to display aliases
	var aliasList *widget.List
	var aliases []string // sorted aliases
	
	// Refresh the list from config
	refreshAliasList := func() {
		// Get sorted aliases
		aliases = make([]string, 0, len(g.config.Aliases))
		for alias := range g.config.Aliases {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		
		// Update the list
		aliasList.Length = func() int {
			return len(aliases)
		}
		
		aliasList.Refresh()
	}
	
	// Create the list
	aliasList = widget.NewList(
		func() int { return 0 },
		func() fyne.CanvasObject {
			aliasLabel := widget.NewLabel("")
			aliasLabel.TextStyle = fyne.TextStyle{Bold: true}
			addrLabel := widget.NewLabel("")
			addrLabel.Wrapping = fyne.TextTruncate
			return container.NewVBox(
				aliasLabel,
				addrLabel,
				widget.NewSeparator(),
			)
		},
		func(id widget.ListItemID, item fyne.CanvasObject) {
			if id < len(aliases) {
				alias := aliases[id]
				addr := g.config.Aliases[alias]
				container := item.(*fyne.Container)
				container.Objects[0].(*widget.Label).SetText(alias)
				container.Objects[1].(*widget.Label).SetText(addr)
				
				// Highlight selected item
				if id == selectedIndex {
					container.Objects[0].(*widget.Label).TextStyle = fyne.TextStyle{Bold: true}
					container.Objects[0].(*widget.Label).Refresh()
				} else {
					container.Objects[0].(*widget.Label).TextStyle = fyne.TextStyle{}
					container.Objects[0].(*widget.Label).Refresh()
				}
			}
		},
	)
	
	// Handle selection
	aliasList.OnSelected = func(id widget.ListItemID) {
		selectedIndex = id
		aliasList.Refresh()
	}
	
	refreshAliasList()
	
	// Input fields for adding/editing
	aliasEntry := widget.NewEntry()
	aliasEntry.PlaceHolder = "Alias (e.g., alice@mix.nym)"
	
	addressEntry := widget.NewEntry()
	addressEntry.PlaceHolder = "Nym Address"
	addressEntry.Wrapping = fyne.TextTruncate
	
	// Buttons for actions
	addButton := widget.NewButton("Add/Update", func() {
		alias := strings.TrimSpace(aliasEntry.Text)
		address := strings.TrimSpace(addressEntry.Text)
		
		if alias == "" {
			dialog.ShowError(fmt.Errorf("alias cannot be empty"), accountWindow)
			return
		}
		
		if address == "" {
			dialog.ShowError(fmt.Errorf("nym address cannot be empty"), accountWindow)
			return
		}
		
		// Add or update alias
		g.config.Aliases[alias] = address
		
		// Save config
		if err := g.saveConfig(); err != nil {
			dialog.ShowError(fmt.Errorf("failed to save config: %v", err), accountWindow)
			return
		}
		
		// Clear inputs and selection
		aliasEntry.SetText("")
		addressEntry.SetText("")
		selectedIndex = -1
		
		// Refresh list
		refreshAliasList()
		
		dialog.ShowInformation("Success", fmt.Sprintf("Alias '%s' saved successfully", alias), accountWindow)
	})
	addButton.Importance = widget.HighImportance
	
	deleteButton := widget.NewButton("Delete Selected", func() {
		if selectedIndex < 0 || selectedIndex >= len(aliases) {
			dialog.ShowError(fmt.Errorf("no alias selected"), accountWindow)
			return
		}
		
		aliasToDelete := aliases[selectedIndex]
		
		// Confirm deletion
		dialog.ShowConfirm("Confirm Delete", 
			fmt.Sprintf("Are you sure you want to delete alias '%s'?", aliasToDelete),
			func(confirmed bool) {
				if confirmed {
					delete(g.config.Aliases, aliasToDelete)
					
					// Save config
					if err := g.saveConfig(); err != nil {
						dialog.ShowError(fmt.Errorf("failed to save config: %v", err), accountWindow)
						return
					}
					
					// Clear selection
					selectedIndex = -1
					
					// Refresh list
					refreshAliasList()
					
					dialog.ShowInformation("Success", fmt.Sprintf("Alias '%s' deleted successfully", aliasToDelete), accountWindow)
				}
			}, accountWindow)
	})
	deleteButton.Importance = widget.DangerImportance
	
	// Add button to load selected alias into input fields for editing
	editButton := widget.NewButton("Edit Selected", func() {
		if selectedIndex < 0 || selectedIndex >= len(aliases) {
			dialog.ShowError(fmt.Errorf("no alias selected"), accountWindow)
			return
		}
		
		alias := aliases[selectedIndex]
		address := g.config.Aliases[alias]
		
		// Load into input fields
		aliasEntry.SetText(alias)
		addressEntry.SetText(address)
	})
	editButton.Importance = widget.MediumImportance
	
	// Close button
	closeButton := widget.NewButton("Close", func() {
		accountWindow.Close()
	})
	
	// Center the buttons in a horizontal box with spacers
	buttonContainer := container.NewHBox(
		layout.NewSpacer(),
		addButton,
		layout.NewSpacer(),
		editButton,
		layout.NewSpacer(),
		deleteButton,
		layout.NewSpacer(),
	)
	
	// Center the close button
	closeButtonContainer := container.NewHBox(
		layout.NewSpacer(),
		closeButton,
		layout.NewSpacer(),
	)
	
	// Layout the content
	inputContainer := container.NewVBox(
		// widget.NewLabelWithStyle("", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		// widget.NewSeparator(),
		aliasEntry,
		addressEntry,
		widget.NewSeparator(),
		buttonContainer,
		widget.NewSeparator(),
	)
	
	listContainer := container.NewBorder(
		widget.NewLabelWithStyle("Aliases (alphabetical order)", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		nil, nil, nil,
		container.NewVBox(
			widget.NewSeparator(),
			aliasList,
		),
	)
	
	// Main content with scroll for list
	mainContent := container.NewBorder(
		inputContainer,
		closeButtonContainer,
		nil, nil,
		container.NewScroll(listContainer),
	)
	
	accountWindow.SetContent(mainContent)
	accountWindow.Show()
}

// saveConfig saves the current configuration to disk
func (g *NymMessageGUI) saveConfig() error {
	configPath := getConfigPath()
	
	data, err := json.MarshalIndent(g.config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %v", err)
	}
	
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("writing config: %v", err)
	}
	
	return nil
}

// Unified Config management functions
func getConfigPath() string {
	// Always use current directory
	return filepath.Join(".", configFileName)
}

func createDefaultUnifiedConfig(configPath string) error {
	fmt.Printf("[INFO] Creating default config at: %s\n", configPath)
	
	defaultConfig := UnifiedConfig{
		// Home server configuration (empty placeholders)
		HomeServer: "your_server_nym_address",
		Password:   "",
		LocalDir:   "inbox",
		
		// Aliases (example entries)
		Aliases: map[string]string{
			"alice@mix.nym":   "83D4Uc5R2dL2eomXdn8hXz99e3yBFXG57XprphFTcx8s.HGV4Tn5mJMvU9gx6p6dBdBtLQbxdJXS1QQvUxUDDy2t@7SnUJy4rWH9hXCitpgwx7XoK5PGRBNjaiz7BWeaqRfXx",
			"bob@mix.nym":     "83D4Uc5R2dL2eomXdn8hXz99e3yBFXG57XprphFTcx8s.HGV4Tn5mJMvU9gx6p6dBdBtLQbxdJXS1QQvUxUDDy2t@7SnUJy4rWH9hXCitpgwx7XoK5PGRBNjaiz7BWeaqRfXx",
			"ch1ffr3punk@mix.nym": "83D4Uc5R2dL2eomXdn8hXz99e3yBFXG57XprphFTcx8s.HGV4Tn5mJMvU9gx6p6dBdBtLQbxdJXS1QQvUxUDDy2t@7SnUJy4rWH9hXCitpgwx7XoK5PGRBNjaiz7BWeaqRfXx",
		},
	}

	data, err := json.MarshalIndent(defaultConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("writing config: %v", err)
	}
	
	// Verify the file was created
	if info, err := os.Stat(configPath); err == nil {
		fmt.Printf("[INFO] Config file created successfully, size: %d bytes\n", info.Size())
	} else {
		fmt.Printf("[WARNING] Config file created but cannot verify: %v\n", err)
	}
	
	return nil
}

func loadOrCreateUnifiedConfig() (*UnifiedConfig, error) {
	configPath := getConfigPath()
	
	fmt.Printf("[DEBUG] Looking for config at: %s\n", configPath)

	// Check if config exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Printf("[INFO] Config file not found, creating default...\n")
		// Create default config
		if err := createDefaultUnifiedConfig(configPath); err != nil {
			return nil, fmt.Errorf("failed to create default config: %v", err)
		}
		fmt.Printf("[INFO] Created default config at: %s\n", configPath)
		fmt.Println("[INFO] ========================================")
		fmt.Println("[INFO] PLEASE EDIT THIS FILE:")
		fmt.Printf("[INFO] %s\n", configPath)
		fmt.Println("[INFO] ========================================")
		fmt.Println("[INFO] Update the following fields:")
		fmt.Println("[INFO]   - home_server: your_actual_home_server_address@mix.nym")
		fmt.Println("[INFO]   - password: your_home_server_password (or leave empty for prompt)")
		fmt.Println("[INFO]   - local_dir: where to save emails (default: downloads)")
		fmt.Println("[INFO]   - aliases: add your own recipient aliases")
		fmt.Println("[INFO] ========================================")
	} else if err != nil {
		return nil, fmt.Errorf("cannot check config file: %v", err)
	} else {
		fmt.Printf("[INFO] Config file found at: %s\n", configPath)
	}

	// Load config
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file: %v", err)
	}
	
	fmt.Printf("[DEBUG] Config file size: %d bytes\n", len(data))

	var config UnifiedConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("invalid config file: %v\nRaw content:\n%s", err, string(data))
	}

	// Ensure Aliases map is initialized
	if config.Aliases == nil {
		fmt.Println("[INFO] Aliases map was nil, initializing empty map")
		config.Aliases = make(map[string]string)
	}
	
	// Set default local dir if not specified
	if config.LocalDir == "" {
		fmt.Println("[INFO] LocalDir not set, using default: downloads")
		config.LocalDir = "downloads"
	}
	
	// Expand ~ in path
	if strings.HasPrefix(config.LocalDir, "~") {
		home, _ := os.UserHomeDir()
		config.LocalDir = filepath.Join(home, config.LocalDir[1:])
		fmt.Printf("[INFO] Expanded LocalDir to: %s\n", config.LocalDir)
	}
	
	// Display config status
	fmt.Println("[INFO] ========================================")
	fmt.Println("[INFO] Current Configuration:")
	fmt.Printf("[INFO] Home Server: %s\n", config.HomeServer)
	if config.Password == "" {
		fmt.Println("[INFO] Password: <empty> (will prompt when fetching)")
	} else {
		fmt.Println("[INFO] Password: ********")
	}
	fmt.Printf("[INFO] Local Dir: %s\n", config.LocalDir)
	fmt.Printf("[INFO] Aliases: %d configured\n", len(config.Aliases))
	fmt.Println("[INFO] ========================================")

	return &config, nil
}

func resolveRecipient(recipient string, aliases map[string]string) (string, error) {
    // Look up the recipient in the aliases map
    if addr, exists := aliases[recipient]; exists {
        return addr, nil
    }
    
    // Recipient not found in aliases - return error
    return "", fmt.Errorf("'%s' not found in NymMail.json", recipient)
}

// Get emails functionality
func (g *NymMessageGUI) getEmails() {
	// Check if home_server is configured
	if g.config.HomeServer == "" || g.config.HomeServer == "your_server_nym_address" {
		g.updateStatus("Error", "server not configured in NymMail.json")
		dialog.ShowError(fmt.Errorf("server not configured in NymMail.json. Please edit NymMail.json and set your server nym address"), g.window)
		return
	}
	
	// Check if password is empty, if so, ask for it
	if g.config.Password == "" {
		g.askForPasswordAndFetch()
		return
	}
	
	// Start fetching with stored password
	g.fetchEmails(g.config.Password)
}

func (g *NymMessageGUI) askForPasswordAndFetch() {
	// Create password entry dialog
	passwordEntry := widget.NewPasswordEntry()
	passwordEntry.PlaceHolder = "Password"
	
	dialog.ShowCustomConfirm("", "Fetch", "Cancel", 
		container.NewVBox(
			// widget.NewLabel("Please enter your home server password:"),
			passwordEntry,
		), 
		func(confirmed bool) {
			if confirmed {
				password := passwordEntry.Text
				if password == "" {
					dialog.ShowError(fmt.Errorf("password cannot be empty"), g.window)
					return
				}
				g.fetchEmails(password)
			}
		}, g.window)
}

func (g *NymMessageGUI) fetchEmails(password string) {
	g.updateStatus("Fetching", "Connecting to nym-client...")
	
	// Show progress in status only, no dialog
	go func() {
		// Get connection
		conn, err := pool.getConnection()
		if err != nil {
			fyne.Do(func() {
				g.updateStatus("Error", fmt.Sprintf("Failed to connect: %v", err))
				dialog.ShowError(fmt.Errorf("failed to connect to nym-client: %v", err), g.window)
			})
			return
		}
		
		// Get own address
		fyne.Do(func() {
			g.updateStatus("Fetching", "Getting own Nym address...")
		})
		
		req, _ := json.Marshal(map[string]string{"type": "selfAddress"})
		if err := conn.WriteMessage(websocket.TextMessage, req); err != nil {
			fyne.Do(func() {
				g.updateStatus("Error", fmt.Sprintf("Failed to get self address: %v", err))
				dialog.ShowError(fmt.Errorf("failed to get self address: %v", err), g.window)
			})
			return
		}
		
		var resp map[string]interface{}
		conn.SetReadDeadline(time.Now().Add(connectionTimeout))
		if err := conn.ReadJSON(&resp); err != nil {
			fyne.Do(func() {
				g.updateStatus("Error", fmt.Sprintf("Failed to read self address: %v", err))
				dialog.ShowError(fmt.Errorf("failed to read self address: %v", err), g.window)
			})
			return
		}
		conn.SetReadDeadline(time.Time{})
		
		ownAddr, ok := resp["address"].(string)
		if !ok {
			fyne.Do(func() {
				g.updateStatus("Error", "Failed to get own Nym address")
				dialog.ShowError(fmt.Errorf("failed to get own Nym address"), g.window)
			})
			return
		}
		
		// Authenticate
		fyne.Do(func() {
			g.updateStatus("Fetching", "Authenticating with home server...")
		})
		
		if err := g.authenticate(conn, g.config.HomeServer, ownAddr, password); err != nil {
			fyne.Do(func() {
				g.updateStatus("Error", fmt.Sprintf("Authentication failed: %v", err))
				dialog.ShowError(fmt.Errorf("authentication failed: %v", err), g.window)
			})
			return
		}
		
		// Get file list
		fyne.Do(func() {
			g.updateStatus("Fetching", "Getting list of emails...")
		})
		
		files, err := g.getFileList(conn, g.config.HomeServer)
		if err != nil {
			fyne.Do(func() {
				g.updateStatus("Error", fmt.Sprintf("Failed to get file list: %v", err))
				dialog.ShowError(fmt.Errorf("failed to get file list: %v", err), g.window)
			})
			return
		}
		
		if len(files) == 0 {
			fyne.Do(func() {
				g.updateStatus("Info", "No emails waiting on server")
				dialog.ShowInformation("", "No emails are currently waiting on your home server.", g.window)
			})
			return
		}
		
		// Download files
		fyne.Do(func() {
			g.updateStatus("Fetching", fmt.Sprintf("Found %d email(s). Downloading...", len(files)))
		})
		
		// Create local directory if it doesn't exist
		if err := os.MkdirAll(g.config.LocalDir, 0755); err != nil {
			fyne.Do(func() {
				g.updateStatus("Error", fmt.Sprintf("Failed to create download directory: %v", err))
				dialog.ShowError(fmt.Errorf("failed to create download directory: %v", err), g.window)
			})
			return
		}
		
		downloaded := []string{}
		for i, file := range files {
			fyne.Do(func() {
				g.updateStatus("Fetching", fmt.Sprintf("Downloading %d/%d: %s...", i+1, len(files), file.Name))
			})
			
			if err := g.downloadFile(conn, g.config.HomeServer, file, g.config.LocalDir, i+1, len(files)); err != nil {
				fyne.Do(func() {
					g.updateStatus("Error", fmt.Sprintf("Failed to download %s: %v", file.Name, err))
				})
				continue
			}
			downloaded = append(downloaded, file.Name)
		}
		
		// Delete files from server
		if len(downloaded) > 0 {
			fyne.Do(func() {
				g.updateStatus("Fetching", "Deleting files from server...")
			})
			
			if err := g.deleteFiles(conn, g.config.HomeServer, downloaded); err != nil {
				fyne.Do(func() {
					g.updateStatus("Warning", fmt.Sprintf("Downloaded %d email(s) but failed to delete from server", len(downloaded)))
					dialog.ShowInformation("", fmt.Sprintf("Downloaded %d email(s) to %s\n\nNote: Failed to delete files from server: %v", 
						len(downloaded), g.config.LocalDir, err), g.window)
				})
			} else {
				fyne.Do(func() {
					g.updateStatus("Success", fmt.Sprintf("✓ Successfully downloaded %d email(s) to %s", len(downloaded), g.config.LocalDir))
					dialog.ShowInformation("", fmt.Sprintf("Successfully downloaded %d email(s) to:\n%s", 
						len(downloaded), g.config.LocalDir), g.window)
				})
			}
		} else {
			fyne.Do(func() {
				g.updateStatus("Error", "No files were downloaded")
				dialog.ShowError(fmt.Errorf("failed to download any emails"), g.window)
			})
		}
	}()
}

// Authentication structures
type AuthRequest struct {
	Type       string `json:"type"`
	Password   string `json:"password"`
	ClientAddr string `json:"client_addr,omitempty"`
}

type AuthResponse struct {
	Type    string `json:"type"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type FileListRequest struct {
	Type string `json:"type"`
}

type FileListResponse struct {
	Type  string            `json:"type"`
	Files []FileInfoRemote `json:"files"`
}

type FileInfoRemote struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type GetFileRequest struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type DeleteRequest struct {
	Type  string   `json:"type"`
	Files []string `json:"files"`
}

type FileChunkRemote struct {
	Type   string `json:"type"`
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	Data   []byte `json:"data"`
	IsLast bool   `json:"is_last"`
}

func (g *NymMessageGUI) authenticate(conn *websocket.Conn, recipient, ownAddr, password string) error {
	hash := sha256.Sum256([]byte(password))
	passwordHash := hex.EncodeToString(hash[:])
	
	authReq := AuthRequest{
		Type:       "auth",
		Password:   passwordHash,
		ClientAddr: ownAddr,
	}
	innerJSON, err := json.Marshal(authReq)
	if err != nil {
		return fmt.Errorf("marshal auth: %w", err)
	}
	
	wrapper := map[string]string{
		"type":    "message",
		"message": string(innerJSON),
	}
	wrapperJSON, err := json.Marshal(wrapper)
	if err != nil {
		return fmt.Errorf("marshal wrapper: %w", err)
	}
	
	if err := g.sendViaNymClient(conn, recipient, string(wrapperJSON)); err != nil {
		return fmt.Errorf("send auth via nym-client: %w", err)
	}
	
	_, respMsg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	
	innerResp, err := g.unwrapNymResponse(respMsg)
	if err != nil {
		return fmt.Errorf("unwrap response: %w", err)
	}
	
	var authResp AuthResponse
	if err := json.Unmarshal([]byte(innerResp), &authResp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	
	if !authResp.Success {
		return fmt.Errorf("%s", authResp.Message)
	}
	return nil
}

func (g *NymMessageGUI) getFileList(conn *websocket.Conn, recipient string) ([]FileInfoRemote, error) {
	req := FileListRequest{Type: "list"}
	innerJSON, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	
	wrapper := map[string]string{
		"type":    "message",
		"message": string(innerJSON),
	}
	wrapperJSON, err := json.Marshal(wrapper)
	if err != nil {
		return nil, err
	}
	
	if err := g.sendViaNymClient(conn, recipient, string(wrapperJSON)); err != nil {
		return nil, fmt.Errorf("send list request: %w", err)
	}
	
	_, respMsg, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read list response: %w", err)
	}
	
	innerResp, err := g.unwrapNymResponse(respMsg)
	if err != nil {
		return nil, fmt.Errorf("unwrap response: %w", err)
	}
	
	var listResp FileListResponse
	if err := json.Unmarshal([]byte(innerResp), &listResp); err != nil {
		return nil, fmt.Errorf("parse list response: %w", err)
	}
	
	if listResp.Type != "file_list" {
		return nil, fmt.Errorf("unexpected response type: %s", listResp.Type)
	}
	return listResp.Files, nil
}

func (g *NymMessageGUI) downloadFile(conn *websocket.Conn, recipient string, file FileInfoRemote, localDir string, current, total int) error {
	req := GetFileRequest{Type: "get", Name: file.Name}
	innerJSON, err := json.Marshal(req)
	if err != nil {
		return err
	}
	
	wrapper := map[string]string{
		"type":    "message",
		"message": string(innerJSON),
	}
	wrapperJSON, err := json.Marshal(wrapper)
	if err != nil {
		return err
	}
	
	if err := g.sendViaNymClient(conn, recipient, string(wrapperJSON)); err != nil {
		return fmt.Errorf("request file: %w", err)
	}
	
	localPath := filepath.Join(localDir, file.Name)
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	
	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	
	const downloadChunkSize = 32 * 1024
	totalChunks := int((file.Size + downloadChunkSize - 1) / downloadChunkSize)
	received := 0
	
	for {
		_, chunkMsg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read chunk: %w", err)
		}
		
		innerChunk, err := g.unwrapNymResponse(chunkMsg)
		if err != nil {
			innerChunk = string(chunkMsg)
		}
		
		var chunk FileChunkRemote
		if err := json.Unmarshal([]byte(innerChunk), &chunk); err != nil {
			return fmt.Errorf("parse chunk: %w", err)
		}
		
		if chunk.Type != "file_chunk" {
			return fmt.Errorf("expected chunk, got %s", chunk.Type)
		}
		
		if _, err := f.Write(chunk.Data); err != nil {
			return fmt.Errorf("write chunk: %w", err)
		}
		
		received++
		
		// Update progress in status
		percentage := float64(received) / float64(totalChunks) * 100
		fyne.Do(func() {
			g.updateStatus("Fetching", fmt.Sprintf("Downloading %d/%d: %s (%d%%)", current, total, file.Name, int(percentage)))
		})
		
		if chunk.IsLast {
			break
		}
	}
	
	return nil
}

func (g *NymMessageGUI) deleteFiles(conn *websocket.Conn, recipient string, files []string) error {
	req := DeleteRequest{Type: "delete", Files: files}
	innerJSON, err := json.Marshal(req)
	if err != nil {
		return err
	}
	
	wrapper := map[string]string{
		"type":    "message",
		"message": string(innerJSON),
	}
	wrapperJSON, err := json.Marshal(wrapper)
	if err != nil {
		return err
	}
	
	if err := g.sendViaNymClient(conn, recipient, string(wrapperJSON)); err != nil {
		return fmt.Errorf("send delete request: %w", err)
	}
	
	_, respMsg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read delete response: %w", err)
	}
	
	innerResp, err := g.unwrapNymResponse(respMsg)
	if err != nil {
		return fmt.Errorf("unwrap response: %w", err)
	}
	
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(innerResp), &resp); err != nil {
		return fmt.Errorf("parse delete response: %w", err)
	}
	
	if resp["type"] != "delete_success" {
		return fmt.Errorf("delete failed: %v", resp["message"])
	}
	return nil
}

func (g *NymMessageGUI) sendViaNymClient(conn *websocket.Conn, recipient, payload string) error {
	sendMsg := fmt.Sprintf(`{"type":"send","recipient":%q,"message":%q}`, recipient, payload)
	return conn.WriteMessage(websocket.TextMessage, []byte(sendMsg))
}

func (g *NymMessageGUI) unwrapNymResponse(rawMsg []byte) (string, error) {
	var wrapper map[string]string
	if err := json.Unmarshal(rawMsg, &wrapper); err != nil {
		return "", fmt.Errorf("parse nym wrapper: %w", err)
	}
	if msg, ok := wrapper["message"]; ok && msg != "" {
		return msg, nil
	}
	return "", fmt.Errorf("no message field in nym response")
}
