// testing.go provides Luistervink connection and functionality testing capabilities
package luistervink

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go/internal/errors"
	"github.com/tphakala/birdnet-go/internal/logger"
)

// Rate limiting constants
const (
	// Minimum time between test submissions (1 minute)
	minTestInterval = 1 * time.Minute
)

// Test configuration defaults
const (
	// defaultFailureProbability is the default probability for simulated failures
	defaultFailureProbability = 0.5

	// defaultMinDelayMs is the default minimum delay in milliseconds for simulated delays
	defaultMinDelayMs = 500

	// defaultMaxDelayMs is the default maximum delay in milliseconds for simulated delays
	defaultMaxDelayMs = 3000
)

// Audio constants for test data generation
const (
	// testAudioSampleRate is the sample rate for test audio data (48kHz)
	testAudioSampleRate = 48000

	// testAudioBytesPerSample is the number of bytes per audio sample (16-bit = 2 bytes)
	testAudioBytesPerSample = 2

	// testAudioDurationFraction is the fraction of a second for test audio (0.5 seconds)
	testAudioDurationFraction = 2 // sampleRate / 2 = 0.5 seconds
)

// generateTestPCMData creates a small test PCM data buffer (500ms of silence)
// This is used in multiple test functions to avoid code duplication
func generateTestPCMData() []byte {
	numSamples := testAudioSampleRate / testAudioDurationFraction // 0.5 seconds
	return make([]byte, numSamples*testAudioBytesPerSample)
}

// newSecureHTTPClient creates an HTTP client with secure TLS configuration
// This helper reduces code duplication for creating HTTP clients with TLS settings
func newSecureHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: false,
			},
		},
	}
}

// HTTP status code constants for testing
const (
	// httpStatusClientError is the threshold for client error status codes
	httpStatusClientError = 400

	// httpStatusUnauthorized is the 401 status code
	httpStatusUnauthorized = 401

	// httpStatusForbidden is the 403 status code
	httpStatusForbidden = 403

	// httpStatusNotFound is the 404 status code
	httpStatusNotFound = 404
)

// DNS Fallback resolvers
var fallbackDNSResolvers = []string{
	"1.1.1.1:53", // Cloudflare
	"8.8.8.8:53", // Google
	"9.9.9.9:53", // Quad9
}

// resultContext is used to pass result IDs through context
type resultContext struct {
	ID string
}

// Define a custom type for the context key to avoid collisions
type contextKey int

// Define the key constant
const resultIDKey contextKey = iota

// Global rate limiter state
var (
	lastTestTime  time.Time
	rateLimiterMu sync.Mutex
)

// maskURLForLogging masks sensitive LuistervinkID tokens in URLs for safe logging
// This is a package-level function for use in testing code
func maskURLForLogging(urlStr, luistervinkID string) string {
	if luistervinkID == "" {
		return urlStr
	}
	return strings.ReplaceAll(urlStr, luistervinkID, "***")
}

// checkRateLimit returns error if tests are being run too frequently
func checkRateLimit() error {
	rateLimiterMu.Lock()
	defer rateLimiterMu.Unlock()

	// If this is the first test or enough time has passed, allow it
	if lastTestTime.IsZero() || time.Since(lastTestTime) >= minTestInterval {
		// Update the last test time and allow this test
		lastTestTime = time.Now()
		return nil
	}

	// Calculate time until next allowed test
	nextAllowedTime := lastTestTime.Add(minTestInterval)
	expiryTime := nextAllowedTime.Unix()

	return fmt.Errorf("rate limit exceeded: please wait before testing again|%d", expiryTime)
}

// resolveDNSWithFallback attempts to resolve a hostname using fallback DNS servers if the OS resolver fails
// It uses shorter timeouts per DNS server to avoid long waits when multiple servers are unreachable
// The provided context allows callers to control overall timeout and cancellation
//
//nolint:gocognit // Complexity justified: DNS fallback requires multiple resolver attempts with per-attempt timeout management
func resolveDNSWithFallback(ctx context.Context, hostname string) ([]net.IP, error) {
	log := GetLogger()

	// First try the standard resolver with context-based timeout
	// Create a child context with systemDNSTimeout, but respect parent cancellation
	dnsCtx, cancel := context.WithTimeout(ctx, systemDNSTimeout)
	defer cancel()

	// Use net.DefaultResolver.LookupIP which supports context cancellation
	ips, err := net.DefaultResolver.LookupIP(dnsCtx, "ip", hostname)
	if err == nil && len(ips) > 0 {
		return ips, nil
	}

	// Log the error with appropriate context
	// Go 1.23+: Better timeout detection via errors.Is(err, context.DeadlineExceeded)
	if err != nil {
		if isDNSTimeout(err) {
			log.Warn("Standard DNS resolution timed out (likely multiple unreachable DNS servers)",
				logger.String("hostname", hostname),
				logger.Duration("timeout", systemDNSTimeout))
		} else {
			log.Warn("Standard DNS resolution failed",
				logger.String("hostname", hostname),
				logger.Error(err))
		}
	}

	log.Info("Attempting to resolve using fallback DNS servers", logger.String("hostname", hostname))

	// Try each fallback resolver with shorter, independent timeouts
	var lastErr error
	for _, resolver := range fallbackDNSResolvers {
		r := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: dnsResolverTimeout}
				// Honor the requested network (udp/tcp; including v4/v6 variants)
				return d.DialContext(ctx, network, resolver)
			},
		}

		// Cap per-attempt timeout to remaining context budget to prevent overshooting stage deadline
		attemptTimeout := dnsLookupTimeout
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				// Context already expired
				if lastErr != nil {
					return nil, fmt.Errorf("failed to resolve %s: context deadline exceeded after trying %d fallback servers: %w", hostname, len(fallbackDNSResolvers), lastErr)
				}
				return nil, ctx.Err()
			}
			if remaining < attemptTimeout {
				attemptTimeout = remaining
			}
		}

		// Create a child context with timeout, preserving parent cancellation
		childCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		fallbackIPs, err := r.LookupIPAddr(childCtx, hostname)
		cancel()

		if err != nil {
			lastErr = err
		}

		if err == nil && len(fallbackIPs) > 0 {
			// Convert IPAddr to IP
			result := make([]net.IP, len(fallbackIPs))
			for i, addr := range fallbackIPs {
				result[i] = addr.IP
			}
			log.Info("Successfully resolved using fallback DNS",
				logger.String("hostname", hostname),
				logger.String("resolver", resolver),
				logger.Any("ips", result))
			return result, nil
		}

		// Log the failure reason
		if err == nil {
			log.Debug("Fallback DNS returned no records",
				logger.String("resolver", resolver),
				logger.String("hostname", hostname))
		} else {
			log.Debug("Fallback DNS resolution failed",
				logger.String("resolver", resolver),
				logger.String("hostname", hostname),
				logger.Error(err))
		}
	}

	// Return with root cause if available for better diagnostics
	if lastErr != nil {
		return nil, fmt.Errorf("failed to resolve %s with all DNS resolvers (system + %d fallback servers): %w", hostname, len(fallbackDNSResolvers), lastErr)
	}
	return nil, fmt.Errorf("failed to resolve %s with all DNS resolvers (system + %d fallback servers)", hostname, len(fallbackDNSResolvers))
}

// TestConfig encapsulates test configuration for artificial delays and failures
type TestConfig struct {
	// Set to true to enable artificial delays and random failures
	Enabled bool
	// Internal flag to enable random failures (for testing UI behavior)
	RandomFailureMode bool
	// Probability of failure for each stage (0.0 - 1.0)
	FailureProbability float64
	// Min and max artificial delay in milliseconds
	MinDelay int
	MaxDelay int
	// Thread-safe random number generator
	rng *rand.Rand
	mu  sync.Mutex
}

// Default test configuration instance
var testConfig = &TestConfig{
	Enabled:            false,
	RandomFailureMode:  false,
	FailureProbability: defaultFailureProbability,
	MinDelay:           defaultMinDelayMs,
	MaxDelay:           defaultMaxDelayMs,
	rng:                rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()))), //nolint:gosec // G404: weak randomness acceptable for test utilities, not security-critical
}

// simulateDelay adds an artificial delay
func simulateDelay() {
	if !testConfig.Enabled {
		return
	}
	testConfig.mu.Lock()
	delay := testConfig.rng.IntN(testConfig.MaxDelay-testConfig.MinDelay) + testConfig.MinDelay
	testConfig.mu.Unlock()
	time.Sleep(time.Duration(delay) * time.Millisecond)
}

// simulateFailure returns true if the test should fail
func simulateFailure() bool {
	if !testConfig.Enabled || !testConfig.RandomFailureMode {
		return false
	}
	testConfig.mu.Lock()
	defer testConfig.mu.Unlock()
	return testConfig.rng.Float64() < testConfig.FailureProbability
}

// TestResult represents the result of a Luistervink test
type TestResult struct {
	Success         bool   `json:"success"`
	Stage           string `json:"stage"`
	Message         string `json:"message"`
	Error           string `json:"error,omitempty"`
	IsProgress      bool   `json:"isProgress,omitempty"`
	State           string `json:"state,omitempty"`           // Current state: running, completed, failed, timeout
	Timestamp       string `json:"timestamp,omitempty"`       // ISO8601 timestamp of the result
	ResultID        string `json:"resultId,omitempty"`        // Optional ID for test results like soundscapeID
	RateLimitExpiry int64  `json:"rateLimitExpiry,omitempty"` // Unix timestamp for when rate limit expires
}

// TestStage represents a stage in the Luistervink test process
type TestStage int

const (
	APIConnectivity TestStage = iota
	Authentication
	SoundscapeUpload
	DetectionPost
)

// String returns the string representation of a test stage
func (s TestStage) String() string {
	switch s {
	case APIConnectivity:
		return "API Connectivity"
	case Authentication:
		return "Authentication"
	case SoundscapeUpload:
		return "Soundscape Upload"
	case DetectionPost:
		return "Detection Post"
	default:
		return "Unknown Stage"
	}
}

// Timeout constants for various test stages
// These timeouts account for potential DNS resolution delays in Docker/containerized environments
// where multiple DNS servers may be configured and the first server(s) might be unreachable,
// causing sequential timeout attempts (typically 5 seconds per DNS server).
const (
	apiTimeout    = 15 * time.Second // Increased to handle multiple DNS server timeouts
	authTimeout   = 15 * time.Second // Increased to handle multiple DNS server timeouts
	uploadTimeout = 30 * time.Second // Increased for encoding + DNS resolution
	postTimeout   = 15 * time.Second // Increased to handle multiple DNS server timeouts

	// DNS-specific timeouts
	// Linux default DNS timeout is 5s per server. With multiple DNS servers configured,
	// total time can be 5s × N servers. We allow time for 2 server attempts (10s).
	systemDNSTimeout = 10 * time.Second // Maximum wait for system DNS (allows 2 × 5s server attempts)

	// Per-server timeouts for fallback DNS resolution
	// Set to 5s to match Linux default and allow each DNS server a full timeout attempt
	dnsResolverTimeout = 5 * time.Second // Per-server connection timeout: matches Linux DNS default
	dnsLookupTimeout   = 5 * time.Second // Per-lookup timeout: allows one full DNS server attempt
)

// networkTest represents a generic network test function
type luistervinkTest func(context.Context) error

// runTest executes a Luistervink test with proper timeout and cleanup
func runTest(ctx context.Context, stage TestStage, test luistervinkTest) TestResult {
	// Add simulated delay if enabled
	simulateDelay()

	// Check for simulated failure
	if simulateFailure() {
		return TestResult{
			Success: false,
			Stage:   stage.String(),
			Error:   fmt.Sprintf("simulated %s failure", stage),
			Message: fmt.Sprintf("Failed to perform %s", stage),
		}
	}

	// Create buffered channel for test result
	resultChan := make(chan error, 1)

	// Create a context with a value to pass back the result ID
	ctx = context.WithValue(ctx, resultIDKey, &resultContext{})

	// Run the test in a goroutine
	go func() {
		resultChan <- test(ctx)
	}()

	// Wait for either test completion or context cancellation
	select {
	case <-ctx.Done():
		// Provide more helpful timeout error message
		timeoutMsg := fmt.Sprintf("%s operation timed out. If using Docker, this may be caused by DNS resolution delays from unreachable DNS servers in /etc/resolv.conf", stage)
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

	// Get any result ID from the context
	var resultID string
	if rc, ok := ctx.Value(resultIDKey).(*resultContext); ok && rc != nil {
		resultID = rc.ID
	}

	// Create appropriate success message based on the stage
	var message string
	switch stage {
	case SoundscapeUpload:
		message = fmt.Sprintf("Successfully uploaded test soundscape (0.5 second silent audio) to Luistervink. This recording should appear on your Luistervink station at %s.", time.Now().Format("Jan 2, 2006 at 15:04:05"))
	case DetectionPost:
		message = "Successfully posted test detection to Luistervink: Whooper Swan (Cygnus cygnus) with unlikely confidence."
	default:
		message = fmt.Sprintf("Successfully completed %s", stage)
	}

	return TestResult{
		Success:  true,
		Stage:    stage.String(),
		Message:  message,
		ResultID: resultID,
	}
}

// testAPIConnectivity tests basic connectivity to the Luistervink API
//
//nolint:gocognit // Complexity justified: connectivity testing requires multiple fallback attempts and error handling paths
func (b *LvClient) testAPIConnectivity(ctx context.Context) TestResult {
	log := GetLogger()

	apiCtx, apiCancel := context.WithTimeout(ctx, apiTimeout)
	defer apiCancel()

	return runTest(apiCtx, APIConnectivity, func(ctx context.Context) error {
		// Define the API endpoint URL
		apiEndpoint := "https://api.luistervink.nl/api/docs/"

		// Parse URL to extract the hostname
		parsedURL, err := url.Parse(apiEndpoint)
		if err != nil {
			return fmt.Errorf("invalid API endpoint URL: %w", err)
		}
		hostname := parsedURL.Hostname()

		// First attempt: Use standard HTTP client
		log.Info("Testing connectivity to Luistervink API", logger.String("endpoint", apiEndpoint))
		err = tryAPIConnection(ctx, apiEndpoint)

		// If first attempt fails with DNS error, try fallback DNS resolution
		if err != nil {
			if isDNSError(err) {
				log.Warn("DNS resolution failed, attempting fallback", logger.Error(err))

				// Attempt DNS resolution with fallback resolvers
				ips, resolveErr := resolveDNSWithFallback(ctx, hostname)
				if resolveErr != nil {
					return fmt.Errorf("failed to connect to Luistervink API: %w - could not resolve the Luistervink API hostname", err)
				}

				// If fallback DNS succeeded, it means the system DNS is incorrectly configured
				// We don't connect directly with IP as that would cause HTTPS certificate validation issues
				log.Info("Fallback DNS successfully resolved while system DNS failed",
					logger.String("hostname", hostname))
				log.Warn("This indicates your system DNS is incorrectly configured")

				// Log the resolved IPs for debugging
				ipStrings := make([]string, len(ips))
				for i, ip := range ips {
					ipStrings[i] = ip.String()
				}
				log.Debug("Resolved IPs using fallback DNS", logger.String("ips", strings.Join(ipStrings, ", ")))

				// Try connecting again with the original FQDN - this may work if the DNS
				// resolution failure was transient or if the fallback resolution affected DNS cache
				log.Info("Retrying connection with original hostname after fallback DNS resolution")
				retryErr := tryAPIConnection(ctx, apiEndpoint)
				if retryErr == nil {
					log.Info("Successfully connected to Luistervink API after fallback DNS resolution")
					return nil
				}

				// Both attempts failed
				return fmt.Errorf("failed to connect to Luistervink API: %w - System DNS failed but fallback DNS resolved the hostname. This indicates your system DNS resolver is misconfigured or has unreachable DNS servers. If using Docker, check /etc/resolv.conf for unreachable nameservers. Consider removing unreachable DNS servers or increasing timeout values", err)
			}

			// Not a DNS error, return the original error
			return err
		}

		return nil
	})
}

// Helper function to check if an error is DNS-related
// Go 1.23+ improvement: DNSError now wraps timeout and cancellation errors,
// so we can use errors.Is for more reliable detection
func isDNSError(err error) bool {
	if err == nil {
		return false
	}

	// Check if it's a DNSError type
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	// Check for URL errors with lookup operation
	var urlErr *url.Error
	if errors.As(err, &urlErr) && strings.HasPrefix(urlErr.Op, "lookup") {
		return true
	}

	// Minimal fallback to string matching only for clear DNS-specific patterns
	// Avoid broad matches like "network" or "dial tcp" that can match non-DNS errors
	s := err.Error()
	return strings.Contains(s, "no such host") || strings.Contains(s, "lookup ")
}

// isDNSTimeout checks if a DNS error was caused by a timeout
// Go 1.23+ feature: DNSError now wraps context.DeadlineExceeded
func isDNSTimeout(err error) bool {
	if err == nil {
		return false
	}

	// Go 1.23+: DNSError wraps timeout errors
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Also check for net.Error timeout
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	return false
}

// tryAPIConnection attempts to connect to the API endpoint
func tryAPIConnection(ctx context.Context, apiEndpoint string, hostHeader ...string) error {
	log := GetLogger()

	req, err := http.NewRequestWithContext(ctx, "HEAD", apiEndpoint, http.NoBody)
	if err != nil {
		return err
	}

	// Set User-Agent
	req.Header.Set("User-Agent", "BirdNET-Go")

	// If host header is provided (for IP direct connections), set it
	if len(hostHeader) > 0 && hostHeader[0] != "" {
		req.Host = hostHeader[0]
	}

	// Create a temporary HTTP client with a shorter timeout for this test
	client := newSecureHTTPClient(apiTimeout)

	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return fmt.Errorf("API connectivity test timed out: %w", err)
		}
		// Check if this is a DNS error
		if isDNSError(err) {
			return fmt.Errorf("failed to connect to Luistervink API: %w - could not resolve the Luistervink API hostname", err)
		}
		return fmt.Errorf("failed to connect to Luistervink API: %w", err)
	}
	defer closeResponseBody(resp)

	if resp.StatusCode >= httpStatusClientError {
		// Special handling for 404 Not Found errors
		if resp.StatusCode == httpStatusNotFound {
			log.Warn("Luistervink API endpoint not found", logger.Int("status_code", resp.StatusCode))
			return fmt.Errorf("API endpoint not found (404)")
		}
		log.Warn("Luistervink API returned error status", logger.Int("status_code", resp.StatusCode))
		return fmt.Errorf("API returned error status: %d", resp.StatusCode)
	}

	// Successfully connected to the API
	log.Info("Successfully connected to Luistervink API", logger.Int("status_code", resp.StatusCode))
	return nil
}

// testAuthentication tests authentication with the Luistervink API by posting to the test endpoint
// The test endpoint validates the device token and accepts test detection data
func (b *LvClient) testAuthentication(ctx context.Context) TestResult {
	log := GetLogger()

	authCtx, authCancel := context.WithTimeout(ctx, authTimeout)
	defer authCancel()

	return runTest(authCtx, Authentication, func(ctx context.Context) error {
		// Use the test detection endpoint with device token
		testURL := fmt.Sprintf("https://api.luistervink.nl/api/detections/test/?token=%s", url.QueryEscape(b.LuistervinkID))
		maskedURL := maskURLForLogging(testURL, b.LuistervinkID)

		log.Info("Testing authentication with Luistervink", logger.String("endpoint", maskedURL))

		// Create minimal valid test detection payload
		testPayload := struct {
			Timestamp      string  `json:"timestamp"`
			ScientificName string  `json:"scientificName"`
			Latitude       float64 `json:"lat"`
			Longitude      float64 `json:"lon"`
			Confidence     float64 `json:"confidence"`
			SoundscapeID   string  `json:"soundscapeId"`
		}{
			Timestamp:      time.Now().Format("2006-01-02T15:04:05.000-0700"),
			ScientificName: "Parus major",
			Latitude:       b.Latitude,
			Longitude:      b.Longitude,
			Confidence:     0.1, // Very low confidence to indicate test
			SoundscapeID:   "auth-test",
		}

		payloadBytes, err := json.Marshal(testPayload)
		if err != nil {
			return fmt.Errorf("failed to marshal test payload: %w", err)
		}

		// Create HTTP request
		req, err := http.NewRequestWithContext(ctx, "POST", testURL, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return fmt.Errorf("failed to create authentication request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "BirdNET-Go")

		// Execute request
		client := newSecureHTTPClient(authTimeout)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to authenticate with Luistervink at %s: %w", maskedURL, err)
		}
		defer closeResponseBody(resp)

		// Check response status
		switch resp.StatusCode {
		case http.StatusOK:
			log.Info("Successfully authenticated with Luistervink", logger.Int("status_code", resp.StatusCode))
			return nil
		case httpStatusUnauthorized, httpStatusForbidden:
			log.Error("Luistervink authentication failed: invalid device token")
			return fmt.Errorf("authentication failed: invalid device token")
		case httpStatusNotFound:
			log.Error("Luistervink test endpoint not found", logger.Int("status_code", resp.StatusCode))
			return fmt.Errorf("test endpoint not found (404) - please verify API availability")
		default:
			if resp.StatusCode >= httpStatusClientError {
				log.Error("Luistervink authentication failed",
					logger.Int("status_code", resp.StatusCode))
				return fmt.Errorf("authentication failed: server returned status code %d", resp.StatusCode)
			}
			log.Info("Successfully authenticated with Luistervink", logger.Int("status_code", resp.StatusCode))
			return nil
		}
	})
}

// TestConnection performs a multi-stage test of the Luistervink connection and functionality
//
//nolint:gocognit // Complexity justified: orchestrates multiple test stages with state management and result coordination
func (b *LvClient) TestConnection(ctx context.Context, resultChan chan<- TestResult) {
	log := GetLogger()

	// Helper function to send a result
	sendResult := func(result TestResult) {
		// Mark progress messages
		result.IsProgress = strings.Contains(strings.ToLower(result.Message), "running") ||
			strings.Contains(strings.ToLower(result.Message), "testing") ||
			strings.Contains(strings.ToLower(result.Message), "establishing") ||
			strings.Contains(strings.ToLower(result.Message), "initializing")

		// Set state based on result
		switch {
		case result.State != "":
			// Keep existing state if explicitly set
		case result.Error != "":
			result.State = "failed"
			result.Success = false
			result.IsProgress = false
		case result.IsProgress:
			result.State = "running"
		case result.Success:
			result.State = "completed"
		case strings.Contains(strings.ToLower(result.Error), "timeout") ||
			strings.Contains(strings.ToLower(result.Error), "deadline exceeded"):
			result.State = "timeout"
		default:
			result.State = "failed"
		}

		// Add timestamp
		result.Timestamp = time.Now().Format(time.RFC3339)

		// Log the result
		if result.Success {
			logMsg := result.Message
			if !result.Success && result.Error != "" {
				logMsg = fmt.Sprintf("%s: %s", result.Message, result.Error)
			}
			log.Info("Test stage completed",
				logger.String("stage", result.Stage),
				logger.String("message", logMsg))
		} else {
			log.Warn("Test stage failed",
				logger.String("stage", result.Stage),
				logger.String("message", result.Message),
				logger.String("error", result.Error))
		}

		// Send result to channel
		select {
		case <-ctx.Done():
			return
		case resultChan <- result:
		}
	}

	// Check context before starting
	if err := ctx.Err(); err != nil {
		sendResult(TestResult{
			Success: false,
			Stage:   "Test Setup",
			Message: "Test cancelled",
			Error:   err.Error(),
			State:   "timeout",
		})
		return
	}

	// Start with the explicit "Starting Test" stage
	sendResult(TestResult{
		Success:    true,
		Stage:      "Starting Test",
		Message:    "Initializing Luistervink Connection Test...",
		State:      "running",
		IsProgress: true,
	})

	// Check rate limiting
	if err := checkRateLimit(); err != nil {
		sendResult(TestResult{
			Success: false,
			Stage:   "Starting Test",
			Message: "Rate limit check failed",
			Error:   err.Error(),
			State:   "failed",
		})
		return
	}

	// Helper function to run a test stage
	runStage := func(stage TestStage, test func() TestResult) bool {
		// First, mark the "Starting Test" stage as completed if we're on the first real test
		if stage == APIConnectivity {
			sendResult(TestResult{
				Success:    true,
				Stage:      "Starting Test",
				Message:    "Initialization complete, starting tests",
				State:      "completed",
				IsProgress: false,
			})
		}

		// Send progress message for current stage
		sendResult(TestResult{
			Success:    true,
			Stage:      stage.String(),
			Message:    fmt.Sprintf("Running %s test...", stage.String()),
			State:      "running",
			IsProgress: true,
		})

		// Execute the test
		result := test()
		sendResult(result)
		return result.Success
	}

	// Stage 1: API Connectivity
	if !runStage(APIConnectivity, func() TestResult {
		return b.testAPIConnectivity(ctx)
	}) {
		return
	}

	// Stage 2: Authentication
	if !runStage(Authentication, func() TestResult {
		return b.testAuthentication(ctx)
	}) {
		return
	}
}

// UploadTestSoundscape uploads a test soundscape for testing purposes
func (b *LvClient) UploadTestSoundscape(ctx context.Context) TestResult {
	simulateDelay()

	if simulateFailure() {
		return TestResult{
			Success: false,
			Stage:   SoundscapeUpload.String(),
			Error:   "simulated soundscape upload failure",
			Message: "Failed to upload test soundscape",
		}
	}

	// Generate a small test PCM data (500ms of silence)
	testPCMData := generateTestPCMData()

	// Generate current timestamp in the required format
	timestamp := time.Now().Format("2006-01-02T15:04:05.000-0700")

	// Create a channel for the upload result
	resultChan := make(chan struct {
		id  string
		err error
	}, 1)

	go func() {
		id, err := b.UploadSoundscape(timestamp, testPCMData)
		resultChan <- struct {
			id  string
			err error
		}{id, err}
	}()

	// Wait for either the context to be done or the upload to complete
	select {
	case <-ctx.Done():
		return TestResult{
			Success: false,
			Stage:   SoundscapeUpload.String(),
			Error:   "Soundscape upload timeout",
			Message: "Soundscape upload timed out",
		}
	case result := <-resultChan:
		if result.err != nil {
			return TestResult{
				Success: false,
				Stage:   SoundscapeUpload.String(),
				Error:   result.err.Error(),
				Message: "Failed to upload test soundscape",
			}
		}
		return TestResult{
			Success: true,
			Stage:   SoundscapeUpload.String(),
			Message: fmt.Sprintf("Successfully uploaded test soundscape (0.5 second silent audio). <span class=\"text-info\">This recording should appear on your Luistervink station at %s with ID: %s</span>", time.Now().Format("Jan 2, 2006 at 15:04:05"), result.id),
		}
	}
}
