package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	// 1. Login, Unlock, and get Session Token
	sessionToken, err := loginAndGetSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Bitwarden login failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Bitwarden login successful.")

	// Set the session token as an environment variable for all child processes
	os.Setenv("BW_SESSION", sessionToken)

	// 2. Start the actual 'bw serve' process in the background
	bwServePort := os.Getenv("BW_SERVE_PORT")
	if bwServePort == "" {
		bwServePort = "8088"
	}
	go startBwServe(bwServePort)

	// 3. Start the proxy server on the main port
	bwProxyPort := os.Getenv("BW_PROXY_PORT")
	if bwProxyPort == "" {
		bwProxyPort = "8087"
	}
	go startProxyServer(bwProxyPort, bwServePort)

	// 4. Start the periodic sync
	if os.Getenv("BW_DISABLE_SYNC") != "true" {
		go startPeriodicSync()
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
		cmdConfig := exec.Command("bw", "config", "server", host)
		configResult, err := cmdConfig.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("bw config server failed: %s - %v", string(configResult), err)
		}
	}

	// Login using API Key
	cmdLogin := exec.Command("bw", "login", "--apikey")
	loginOutput, err := cmdLogin.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bw login failed: %s - %v", string(loginOutput), err)
	} else {
		fmt.Println("Logged in successfully")
	}

	fmt.Println("Unlocking vault...")
	// Unlock the vault and get the session key
	cmdUnlock := exec.Command("bw", "unlock", "--passwordenv", "BW_PASSWORD", "--raw")
	unlockOutput, err := cmdUnlock.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bw unlock failed: %s - %v", string(unlockOutput), err)
	}

	return strings.TrimSpace(string(unlockOutput)), nil
}

// startBwServe starts the 'bw serve' process.
func startBwServe(port string) {
	fmt.Printf("Starting 'bw serve' on internal port %s\n", port)
	cmd := exec.Command("bw", "serve", "--hostname", "0.0.0.0", "--port", port)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: 'bw serve' process failed: %v\n", err)
		os.Exit(1)
	}
}

// startProxyServer starts the proxy and health check server.
func startProxyServer(proxyPort, targetPort string) {
	targetURL, err := url.Parse(fmt.Sprintf("http://localhost:%s", targetPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Invalid target URL: %v\n", err)
		os.Exit(1)
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})

	// Sync endpoint
	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fmt.Println("Executing 'bw sync'...")
		cmd := exec.Command("bw", "sync")
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
		fmt.Fprint(w, "Sync successful")
	})

	// Proxy all other requests to the 'bw serve' process
	mux.HandleFunc("/", proxy.ServeHTTP)

	fmt.Printf("Starting proxy server on port %s\n", proxyPort)
	if err := http.ListenAndServe(":"+proxyPort, mux); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Proxy server failed: %v\n", err)
		os.Exit(1)
	}
}

// startPeriodicSync starts a loop that periodically calls the /sync endpoint.
// The interval is configurable via the BW_SYNC_INTERVAL environment variable (e.g., "2m", "1h").
func startPeriodicSync() {
	syncIntervalStr := os.Getenv("BW_SYNC_INTERVAL")
	if syncIntervalStr == "" {
		syncIntervalStr = "2m" // Default to 2 minutes
	}

	syncInterval, err := time.ParseDuration(syncIntervalStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: Invalid format for BW_SYNC_INTERVAL '%s', using default of 2 minutes: %v", syncIntervalStr, err)
		syncInterval = 2 * time.Minute
	}

	fmt.Printf("Starting periodic sync every %s\n", syncInterval)
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for range ticker.C {
		fmt.Println("Periodic sync triggered...")
		resp, err := http.Post("http://localhost:8087/sync", "application/json", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Periodic sync failed: %v", err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Periodic sync failed with status code: %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
}
