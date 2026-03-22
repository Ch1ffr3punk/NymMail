package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
)

type Config struct {
	ServerAddr string `json:"server_addr"` // Can be onion address, Nym address, or clearnet IP/hostname
	Port       string `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	RemoteDir  string `json:"remote_dir"`
	LocalDir   string `json:"local_dir"`
	ProxyAddr  string `json:"proxy_addr"`
	ProxyType  string `json:"proxy_type"` // "socks5", "tor", or "" for no proxy
	UseProxy   bool   `json:"use_proxy"`   // explicit control over proxy usage
}

var manifest []byte

func init() {
	if os.Getenv("GO_WANT_HELP") == "1" {
		fmt.Println("Legitimate file - get")
		os.Exit(0)
	}
}

func main() {
	configFile := flag.String("c", "", "Path to config file (searches in home directory if not specified)")
	proxyOverride := flag.String("proxy", "", "Override proxy address (e.g., 127.0.0.1:9050 or 127.0.0.1:1080)")
	noProxy := flag.Bool("no-proxy", false, "Disable proxy and use direct connection")
	proxyType := flag.String("proxy-type", "auto", "Proxy type: auto, socks5, tor, or direct")
	flag.Parse()

	// Find config file path
	configPath, err := findConfigPath(*configFile)
	if err != nil {
		log.Fatalf("Failed to find config file: %v", err)
	}

	config, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Command line overrides for proxy settings
	if *noProxy {
		config.UseProxy = false
		config.ProxyAddr = ""
	} else if *proxyOverride != "" {
		config.UseProxy = true
		config.ProxyAddr = *proxyOverride
		// Auto-detect proxy type based on port if not explicitly specified
		if *proxyType != "auto" {
			config.ProxyType = *proxyType
		}
	} else if *proxyType != "auto" {
		config.ProxyType = *proxyType
		if config.ProxyType == "direct" {
			config.UseProxy = false
		}
	}

	// Auto-detect proxy type based on port
	if config.UseProxy && config.ProxyType == "auto" {
		config.ProxyType = detectProxyType(config.ProxyAddr)
		fmt.Printf("Auto-detected proxy type: %s\n", config.ProxyType)
	}

	// Establish connection (with or without proxy)
	client, err := connectWithProxy(config)
	if err != nil {
		log.Fatalf("Connection failed: %v", err)
	}
	defer client.Close()

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		log.Fatalf("SFTP client error: %v", err)
	}
	defer sftpClient.Close()

	if err := processFiles(sftpClient, config); err != nil {
		log.Fatalf("Processing error: %v", err)
	}
}

// findConfigPath locates the configuration file in cross-platform locations
func findConfigPath(userProvidedPath string) (string, error) {
	// If user provided a specific path, use it
	if userProvidedPath != "" {
		return userProvidedPath, nil
	}

	// Get home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	// Define possible config file names
	configNames := []string{"get.json", "config.json", ".get.json"}
	
	// Define search locations based on OS
	var searchPaths []string
	
	switch runtime.GOOS {
	case "android":
		// Android typically stores app data in /sdcard or /storage/emulated/0
		searchPaths = []string{
			filepath.Join(homeDir, "Documents"),
			filepath.Join(homeDir, "Download"),
			homeDir,
			"/sdcard",
			"/storage/emulated/0",
		}
	case "ios":
		// iOS sandboxed environment
		searchPaths = []string{
			homeDir,
			filepath.Join(homeDir, "Documents"),
		}
	case "windows":
		// Windows paths
		searchPaths = []string{
			homeDir,
			filepath.Join(homeDir, "Documents"),
			filepath.Join(homeDir, "AppData", "Roaming", "get"),
			filepath.Join(homeDir, ".get"),
		}
	case "darwin":
		// macOS
		searchPaths = []string{
			homeDir,
			filepath.Join(homeDir, "Documents"),
			filepath.Join(homeDir, ".get"),
			filepath.Join(homeDir, "Library", "Application Support", "get"),
		}
	default:
		// Linux and others (including Termux on Android)
		searchPaths = []string{
			homeDir,
			filepath.Join(homeDir, ".config", "get"),
			filepath.Join(homeDir, ".get"),
			filepath.Join(homeDir, "Documents"),
		}
	}

	// Search for config file in all locations
	for _, searchPath := range searchPaths {
		for _, configName := range configNames {
			configPath := filepath.Join(searchPath, configName)
			if _, err := os.Stat(configPath); err == nil {
				fmt.Printf("Found config file: %s\n", configPath)
				return configPath, nil
			}
		}
	}

	// If no config found, return default path in home directory
	defaultPath := filepath.Join(homeDir, "get.json")
	return defaultPath, fmt.Errorf("config file not found. Please create one at: %s", defaultPath)
}

// detectProxyType determines the proxy type based on the port number
func detectProxyType(proxyAddr string) string {
	_, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		return "socks5" // default
	}
	
	switch port {
	case "9050", "9051", "9150":
		return "tor"
	case "1080":
		return "socks5"
	default:
		return "socks5"
	}
}

// loadConfig reads and parses the configuration file
func loadConfig(filename string) (*Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("could not open config file: %w", err)
	}
	defer file.Close()

	var config Config
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return nil, fmt.Errorf("invalid config format: %w", err)
	}

	// Set default values
	if config.Port == "" {
		config.Port = "22"
	}
	if config.RemoteDir == "" {
		config.RemoteDir = "inbox"
	}
	if config.LocalDir == "" {
		config.LocalDir = "downloads"
	}
	
	// Expand tilde and environment variables in paths
	config.LocalDir, err = expandPath(config.LocalDir)
	if err != nil {
		return nil, fmt.Errorf("failed to expand local dir path: %w", err)
	}
	
	// Proxy defaults: if ProxyAddr is set, proxy will be used
	if config.ProxyAddr == "" {
		config.UseProxy = false
	} else {
		// If ProxyAddr is set in config but UseProxy is not explicitly false
		if !config.UseProxy && config.ProxyAddr != "" {
			// For backward compatibility: if ProxyAddr exists, assume it should be used
			config.UseProxy = true
		}
	}
	
	// Proxy type default
	if config.ProxyType == "" {
		config.ProxyType = "auto"
	}

	config.RemoteDir = strings.ReplaceAll(config.RemoteDir, `\`, "/")
	return &config, nil
}

// expandPath expands tilde to home directory and environment variables
func expandPath(path string) (string, error) {
	if path == "" {
		return path, nil
	}
	
	// Expand ~ to home directory
	if strings.HasPrefix(path, "~") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		path = filepath.Join(homeDir, path[1:])
	}
	
	// Expand environment variables
	path = os.ExpandEnv(path)
	
	return path, nil
}

// connectWithProxy establishes an SSH connection either through a proxy or directly
func connectWithProxy(config *Config) (*ssh.Client, error) {
	sshConfig := &ssh.ClientConfig{
		User: config.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(config.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Minute,
	}

	var conn net.Conn
	var err error

	if !config.UseProxy {
		// Direct connection without proxy
		fmt.Println("Using direct connection (no proxy)")
		conn, err = net.DialTimeout("tcp", net.JoinHostPort(config.ServerAddr, config.Port), 2*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("direct connection failed: %w", err)
		}
	} else {
		// Connection through proxy
		fmt.Printf("Using proxy: %s (type: %s)\n", config.ProxyAddr, config.ProxyType)
		
		var dialer proxy.Dialer
		
		switch config.ProxyType {
		case "tor":
			// Tor uses SOCKS5 as well, but with specific settings
			dialer, err = proxy.SOCKS5("tcp", config.ProxyAddr, nil, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("Tor proxy connection failed: %w", err)
			}
		case "socks5":
			// Generic SOCKS5 proxy (also works for Nym)
			dialer, err = proxy.SOCKS5("tcp", config.ProxyAddr, nil, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("SOCKS5 proxy connection failed: %w", err)
			}
		default:
			return nil, fmt.Errorf("unsupported proxy type: %s", config.ProxyType)
		}
		
		conn, err = dialer.Dial("tcp", net.JoinHostPort(config.ServerAddr, config.Port))
		if err != nil {
			return nil, fmt.Errorf("proxy connection failed: %w", err)
		}
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(config.ServerAddr, config.Port), sshConfig)
	if err != nil {
		return nil, fmt.Errorf("SSH handshake failed: %w", err)
	}

	return ssh.NewClient(sshConn, chans, reqs), nil
}

// processFiles reads files from remote directory and downloads them
func processFiles(client *sftp.Client, config *Config) error {
	if err := os.MkdirAll(config.LocalDir, 0700); err != nil {
		return fmt.Errorf("failed to create local directory: %w", err)
	}

	files, err := client.ReadDir(config.RemoteDir)
	if err != nil {
		return fmt.Errorf("failed to read remote directory %s: %w", config.RemoteDir, err)
	}

	if len(files) == 0 {
		fmt.Println("No files found in remote directory")
		return nil
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		remotePath := fmt.Sprintf("%s/%s", strings.TrimSuffix(config.RemoteDir, "/"), file.Name())
		localPath := filepath.Join(config.LocalDir, file.Name())

		if err := transferAndRemove(client, remotePath, localPath); err != nil {
			log.Printf("Failed to process %s: %v", file.Name(), err)
			continue
		}

		fmt.Printf("Successfully processed: %s\n", file.Name())
	}
	return nil
}

// transferAndRemove downloads a file and removes it from the remote server
func transferAndRemove(client *sftp.Client, remotePath, localPath string) error {
	if err := transferFile(client, remotePath, localPath); err != nil {
		return fmt.Errorf("transfer failed: %w", err)
	}

	if err := client.Remove(remotePath); err != nil {
		return fmt.Errorf("remove failed: %w", err)
	}
	return nil
}

// transferFile copies a file from remote server to local filesystem
func transferFile(client *sftp.Client, remotePath, localPath string) error {
	srcFile, err := client.Open(remotePath)
	if err != nil {
		return fmt.Errorf("open failed: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create failed: %w", err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("copy failed: %w", err)
	}
	return nil
}
