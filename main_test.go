package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"testing"
)

// mockExecCommand mocks exec.Command for testing
func mockExecCommand(command string, args ...string) *exec.Cmd {
	cs := []string{"-test.run=TestHelperProcess", "--", command}
	cs = append(cs, args...)
	cmd := exec.Command(os.Args[0], cs...)
	cmd.Env = []string{"GO_WANT_HELPER_PROCESS=1"}
	return cmd
}

// TestHelperProcess isn't a real test. It's used to mock exec.Command.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "No command\n")
		os.Exit(2)
	}

	cmd, args := args[0], args[1:]
	switch cmd {
	case "bw":
		if len(args) > 0 && args[0] == "sync" {
			// Simulate sync success
			fmt.Println("Sync successful")
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func TestHealthcheck(t *testing.T) {
	url, _ := url.Parse("http://localhost:8080")
	proxy := httputil.NewSingleHostReverseProxy(url)
	router := setupRouter(proxy)

	req, _ := http.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	expected := "OK"
	if rr.Body.String() != expected {
		t.Errorf("handler returned unexpected body: got %v want %v",
			rr.Body.String(), expected)
	}
}

func TestSyncEndpoint(t *testing.T) {
	// Swap execCommand with our mock
	execCommand = mockExecCommand
	defer func() { execCommand = exec.Command }()

	url, _ := url.Parse("http://localhost:8080")
	proxy := httputil.NewSingleHostReverseProxy(url)
	router := setupRouter(proxy)

	req, _ := http.NewRequest("POST", "/sync", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	expected := "Sync successful"
	if rr.Body.String() != expected {
		t.Errorf("handler returned unexpected body: got %v want %v",
			rr.Body.String(), expected)
	}
}

func TestSyncEndpointMethodNotAllowed(t *testing.T) {
	url, _ := url.Parse("http://localhost:8080")
	proxy := httputil.NewSingleHostReverseProxy(url)
	router := setupRouter(proxy)

	req, _ := http.NewRequest("GET", "/sync", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusMethodNotAllowed {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusMethodNotAllowed)
	}
}

func TestWaitForBwServe_Unlocked(t *testing.T) {
	t.Setenv("BW_SERVE_WAIT_RETRIES", "5")
	t.Setenv("BW_SERVE_WAIT_INTERVAL", "10ms")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data": {"template": {"status": "unlocked"}}}`))
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	port := u.Port()

	if err := waitForBwServe(port); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestWaitForBwServe_Timeout(t *testing.T) {
	t.Setenv("BW_SERVE_WAIT_RETRIES", "2")
	t.Setenv("BW_SERVE_WAIT_INTERVAL", "10ms")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data": {"template": {"status": "locked"}}}`))
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	port := u.Port()

	if err := waitForBwServe(port); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestIsUnlocked(t *testing.T) {
	tests := []struct {
		jsonStr string
		want    bool
	}{
		{`{"data": {"template": {"status": "unlocked"}}}`, true},
		{`{"data": {"status": "unlocked"}}`, true},
		{`{"status": "unlocked"}`, true},
		{`{"data": {"template": {"status": "locked"}}}`, false},
		{`{}`, false},
		{`{"data": 123}`, false},
		{`{"data": {"template": []}}`, false},
		{`{"status": {}}`, false},
	}

	for _, tt := range tests {
		var v BwStatusResponse
		err := json.Unmarshal([]byte(tt.jsonStr), &v)
		got := err == nil && v.isUnlocked()
		if got != tt.want {
			t.Errorf("isUnlocked(%s) = %v, want %v", tt.jsonStr, got, tt.want)
		}
	}
}
