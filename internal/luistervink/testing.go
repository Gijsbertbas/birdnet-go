// testing.go provides testing functionality for the Luistervink API client
package luistervink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"time"
)

// TestResult represents the result of a Luistervink test
type TestResult struct {
	Success    bool   `json:"success"`
	Stage      string `json:"stage"`
	Message    string `json:"message"`
	Error      string `json:"error,omitempty"`
	IsProgress bool   `json:"isProgress,omitempty"`
	State      string `json:"state,omitempty"`     // Current state: running, completed, failed, timeout
	Timestamp  string `json:"timestamp,omitempty"` // ISO8601 timestamp of the result
}

// TestStage represents a stage in the Luistervink test process
type TestStage int

const (
	APIConnectivity TestStage = iota
	Authentication
	DetectionPost
)

// String returns the string representation of a TestStage
func (s TestStage) String() string {
	switch s {
	case APIConnectivity:
		return "API Connectivity"
	case Authentication:
		return "Authentication"
	case DetectionPost:
		return "Detection Post"
	default:
		return "Unknown Stage"
	}
}

// Timeout constants for test stages
const (
	apiTimeout  = 15 * time.Second
	authTimeout = 15 * time.Second
	postTimeout = 15 * time.Second
)

// luistervinkTest represents a generic network test function
type luistervinkTest func(context.Context) error

// runTest executes a Luistervink test with proper timeout and cleanup
func runTest(ctx context.Context, stage TestStage, test luistervinkTest) TestResult {
	resultChan := make(chan error, 1)

	// Run the test in a goroutine
	go func() {
		resultChan <- test(ctx)
	}()

	// Wait for either test completion or context cancellation
	select {
	case <-ctx.Done():
		timeoutMsg := fmt.Sprintf("%s operation timed out", stage)
		return TestResult{
			Success: false,
			Stage:   stage.String(),
			Error:   "operation timeout",
			Message: timeoutMsg,
		}
	case err := <-resultChan:
		if err != nil {
			return TestResult{
				Success: false,
				Stage:   stage.String(),
				Error:   err.Error(),
				Message: fmt.Sprintf("Failed to perform %s", stage),
			}
		}
	}

	// Create appropriate success message based on the stage
	var message string
	switch stage {
	case DetectionPost:
		message = "Successfully posted test detection to Luistervink: Whooper Swan (Cygnus cygnus) with test confidence."
	default:
		message = fmt.Sprintf("Successfully completed %s", stage)
	}

	return TestResult{
		Success:   true,
		Stage:     stage.String(),
		Message:   message,
		Timestamp: time.Now().Format(time.RFC3339),
	}
}

// TestConnection performs a comprehensive test of the Luistervink API connection
func (l *LvClient) TestConnection(ctx context.Context, resultChan chan<- TestResult) {
	serviceLogger.Info("Starting Luistervink connection test")

	// Stage 1: API Connectivity
	resultChan <- TestResult{
		Success:    true,
		Stage:      APIConnectivity.String(),
		Message:    "Testing API connectivity...",
		IsProgress: true,
	}

	apiCtx, apiCancel := context.WithTimeout(ctx, apiTimeout)
	defer apiCancel()

	apiResult := runTest(apiCtx, APIConnectivity, l.testAPIConnectivity)
	resultChan <- apiResult
	if !apiResult.Success {
		return
	}

	// Stage 2: Authentication
	resultChan <- TestResult{
		Success:    true,
		Stage:      Authentication.String(),
		Message:    "Testing authentication...",
		IsProgress: true,
	}

	authCtx, authCancel := context.WithTimeout(ctx, authTimeout)
	defer authCancel()

	authResult := runTest(authCtx, Authentication, l.testAuthentication)
	resultChan <- authResult
	if !authResult.Success {
		return
	}

	// Stage 3: Detection Post
	resultChan <- TestResult{
		Success:    true,
		Stage:      DetectionPost.String(),
		Message:    "Posting test detection...",
		IsProgress: true,
	}

	postCtx, postCancel := context.WithTimeout(ctx, postTimeout)
	defer postCancel()

	postResult := runTest(postCtx, DetectionPost, l.testDetectionPost)
	resultChan <- postResult
}

// testAPIConnectivity tests basic connectivity to the Luistervink API
func (l *LvClient) testAPIConnectivity(ctx context.Context) error {
	serviceLogger.Debug("Testing Luistervink API connectivity")

	// Test connectivity to the Luistervink API base URL
	apiURL := "https://api.luistervink.nl/api/schema/"

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to Luistervink API: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			serviceLogger.Debug("Failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API returned status code %d", resp.StatusCode)
	}

	serviceLogger.Debug("API connectivity test passed")
	return nil
}

// testAuthentication tests authentication with the Luistervink API
func (l *LvClient) testAuthentication(ctx context.Context) error {
	serviceLogger.Debug("Testing Luistervink authentication")

	if l.Token == "" {
		return fmt.Errorf("no API token configured")
	}

	// Test authentication by attempting to post a minimal test detection
	// This verifies the token is valid by actually using the detections endpoint
	authURL := fmt.Sprintf("https://api.luistervink.nl/api/detections/test/?token=%s", neturl.QueryEscape(l.Token))
	maskedURL := l.maskURL(authURL)

	// Create minimal test payload matching Luistervink API schema
	testPayload := struct {
		Timestamp      string  `json:"timestamp"`
		Latitude       float64 `json:"lat"`
		Longitude      float64 `json:"lon"`
		ScientificName string  `json:"scientificName"`
		Confidence     float64 `json:"confidence"`
		SoundscapeID   string  `json:"soundscapeId"`
	}{
		Timestamp:      time.Now().Format("2006-01-02T15:04:05.000-0700"),
		Latitude:       l.Latitude,
		Longitude:      l.Longitude,
		ScientificName: "Test auth",
		Confidence:     0.0,
		SoundscapeID:   "auth-test",
	}

	payloadBytes, err := json.Marshal(testPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal auth test payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", authURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	serviceLogger.Debug("Sending authentication request", "url", maskedURL)
	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("authentication request failed: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			serviceLogger.Debug("Failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("authentication failed: invalid token")
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("authentication failed with status code %d", resp.StatusCode)
	}

	serviceLogger.Debug("Authentication test passed")
	return nil
}

// testDetectionPost tests posting a detection to the Luistervink API
func (l *LvClient) testDetectionPost(ctx context.Context) error {
	serviceLogger.Debug("Testing detection post to Luistervink")

	// Create a test detection with unlikely confidence to identify it as a test
	timestamp := time.Now().Format("2006-01-02T15:04:05.000-0700")
	testCommonName := "Whooper Swan"
	testScientificName := "Cygnus cygnus"
	testConfidence := 0.12345 // Unlikely confidence to identify as test

	err := l.PostDetection(timestamp, testCommonName, testScientificName, testConfidence)
	if err != nil {
		return fmt.Errorf("failed to post test detection: %w", err)
	}

	serviceLogger.Debug("Detection post test passed")
	return nil
}
