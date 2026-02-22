package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// getEnv retrieves the value of the environment variable named by the key.
// If the variable is present and not empty, the value is returned.
// Otherwise, the fallback value is returned.
// os.LookupEnv is preferred over os.Getenv to distinguish between unset and empty,
// allows for robust handling where empty might not be desired.
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

var (
	execCommand         = exec.Command
	bwServeWaitRetries  = 30
	bwServeWaitInterval = 1 * time.Second
)

func main() {
	// 1. Login, Unlock, and get Session Token
	sessionToken, err := loginAndGetSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Bitwarden login failed: %v\n", err)
		os.Exit(1)
	}

	// Set the session token as an environment variable for all child processes
	if err := os.Setenv("BW_SESSION", sessionToken); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Failed to set BW_SESSION environment variable: %v\n", err)
		os.Exit(1)
	}

	// 2. Start the actual 'bw serve' process in the background
	bwServePort := getEnv("BW_SERVE_PORT", "8088")
	go startBwServe(bwServePort, sessionToken)

	// Wait for the API to be unlocked before routing traffic
	if err := waitForBwServe(bwServePort); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Bitwarden serve API failed to initialize: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Bitwarden serve API is ready and unlocked. Authentication successful.")

	// 3. Start the proxy server on the main port
	bwProxyPort := getEnv("BW_PROXY_PORT", "8087")
	go startProxyServer(bwProxyPort, bwServePort)

	// 4. Start the periodic sync
	if getEnv("BW_DISABLE_SYNC", "false") != "true" {
		bwProxyHost := getEnv("BW_PROXY_HOST", "localhost")
		go startPeriodicSync(bwProxyHost, bwProxyPort)
	} else {
		fmt.Println("Automatic sync is disabled.")
	}

	// Keep the main goroutine alive
	select {}
}

// loginAndGetSession handles the full Bitwarden authentication and returns the session token.
func loginAndGetSession() (string, error) {
	fmt.Println("Executing Bitwarden login...")
	host := os.Getenv("BW_HOST")
	clientID := os.Getenv("BW_CLIENTID")
	clientSecret := os.Getenv("BW_CLIENTSECRET")
	password := os.Getenv("BW_PASSWORD")

	if clientID == "" || clientSecret == "" || password == "" {
		return "", fmt.Errorf("missing one or more required environment variables (BW_CLIENTID, BW_CLIENTSECRET, BW_PASSWORD)")
	}

	// if custom host is specified, configure bw-cli to use it
	if host != "" {
		fmt.Println("Configuring bw-cli to use the supplied host", host)
		cmdConfig := execCommand("bw", "config", "server", host)
		configResult, err := cmdConfig.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("bw config server failed: %s - %v", string(configResult), err)
		}
	}

	// Login using API Key
	cmdLogin := execCommand("bw", "login", "--apikey")
	loginOutput, err := cmdLogin.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bw login failed: %s - %v", string(loginOutput), err)
	} else {
		fmt.Println("Logged in successfully")
	}

	fmt.Println("Unlocking vault...")
	// Unlock the vault and get the session key
	cmdUnlock := execCommand("bw", "unlock", "--passwordenv", "BW_PASSWORD", "--raw")
	unlockOutput, err := cmdUnlock.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bw unlock failed: %s - %v", string(unlockOutput), err)
	}

	return strings.TrimSpace(string(unlockOutput)), nil
}

// startBwServe starts the 'bw serve' process.
func startBwServe(port, sessionToken string) {
	fmt.Printf("Starting 'bw serve' on internal port %s\n", port)
	cmd := execCommand("bw", "serve", "--hostname", "0.0.0.0", "--port", port, "--session", sessionToken)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: 'bw serve' process failed: %v\n", err)
		os.Exit(1)
	}
}

// waitForBwServe blocks until 'bw serve' returns an unlocked status, or errors out.
func waitForBwServe(port string) error {
	statusURL := fmt.Sprintf("http://127.0.0.1:%s/status", port)
	client := &http.Client{Timeout: 2 * time.Second}

	fmt.Println("Waiting for 'bw serve' to become ready and unlocked...")

	for i := 0; i < bwServeWaitRetries; i++ {
		resp, err := client.Get(statusURL)
		if err == nil {
			body, ioErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && ioErr == nil {
				var v map[string]interface{}
				if err := json.Unmarshal(body, &v); err == nil {
					if isUnlocked(v) {
						return nil
					}
				}
			}
		}
		time.Sleep(bwServeWaitInterval)
	}
	return fmt.Errorf("timeout waiting for bw serve to become unlocked")
}

func isUnlocked(v map[string]interface{}) bool {
	if data, ok := v["data"].(map[string]interface{}); ok {
		if template, ok := data["template"].(map[string]interface{}); ok {
			if status, ok := template["status"].(string); ok && status == "unlocked" {
				return true
			}
		}
		if status, ok := data["status"].(string); ok && status == "unlocked" {
			return true
		}
	}
	if status, ok := v["status"].(string); ok && status == "unlocked" {
		return true
	}
	return false
}

// startProxyServer starts the proxy and health check server.
func startProxyServer(proxyPort, targetPort string) {
	targetURL, err := url.Parse(fmt.Sprintf("http://localhost:%s", targetPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Invalid target URL: %v\n", err)
		os.Exit(1)
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	mux := setupRouter(proxy)

	fmt.Printf("Starting proxy server on port %s\n", proxyPort)
	if err := http.ListenAndServe(":"+proxyPort, mux); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Proxy server failed: %v\n", err)
		os.Exit(1)
	}
}

// setupRouter configures the proxy and handlers
func setupRouter(proxy *httputil.ReverseProxy) *http.ServeMux {
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "OK")
	})

	// Sync endpoint
	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fmt.Println("Executing 'bw sync'...")
		cmd := execCommand("bw", "sync")
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Sync failed: %s\n", out.String())
			http.Error(w, fmt.Sprintf("Sync failed: %s", out.String()), http.StatusInternalServerError)
			return
		}
		fmt.Println("Sync successful.")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "Sync successful")
	})

	// Proxy all other requests to the 'bw serve' process
	mux.HandleFunc("/", proxy.ServeHTTP)

	return mux
}

func startPeriodicSync(host, port string) {
	syncIntervalStr := getEnv("BW_SYNC_INTERVAL", "2m")

	syncInterval, err := time.ParseDuration(syncIntervalStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: Invalid format for BW_SYNC_INTERVAL '%s', using default of 2 minutes: %v", syncIntervalStr, err)
		syncInterval = 2 * time.Minute
	}

	syncURL := fmt.Sprintf("http://%s:%s/sync", host, port)
	fmt.Printf("Starting periodic sync every %s targeting %s\n", syncInterval, syncURL)
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for range ticker.C {
		fmt.Println("Periodic sync triggered...")
		resp, err := http.Post(syncURL, "application/json", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Periodic sync failed: %v", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Periodic sync failed with status code: %d and could not read body: %v\n", resp.StatusCode, err)
			} else {
				fmt.Fprintf(os.Stderr, "Periodic sync failed with status code: %d, body: %s\n", resp.StatusCode, string(body))
			}
		}
		_ = resp.Body.Close()
	}
}
