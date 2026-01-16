// luistervink.go provides HTTP handlers for Luistervink-related functionality
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tphakala/birdnet-go/internal/conf"
	"github.com/tphakala/birdnet-go/internal/luistervink"
)

// Luistervink test stage constants
const (
	stageLuistervinkAPIConnectivity = "API Connectivity"
	stageLuistervinkAuthentication  = "Authentication"
	stageLuistervinkDetectionPost   = "Detection Post"
)

// TestLuistervink handles requests to test Luistervink connectivity and functionality
// API: GET/POST /api/v2/integrations/luistervink/test
func (h *Handlers) TestLuistervink(c echo.Context) error {
	// Define a struct for the test configuration
	type TestConfig struct {
		Enabled   bool    `json:"enabled"`
		Token     string  `json:"token"`
		Threshold float64 `json:"threshold"`
		Debug     bool    `json:"debug"`
	}

	var testConfig TestConfig
	var settings *conf.Settings

	// If this is a POST request, use the provided test configuration
	if c.Request().Method == "POST" {
		if err := c.Bind(&testConfig); err != nil {
			return h.NewHandlerError(err, "Invalid test configuration", http.StatusBadRequest)
		}

		// Create temporary settings for the test
		settings = &conf.Settings{
			BirdNET: conf.BirdNETConfig{
				Latitude:  45.0, // Default test values for location
				Longitude: -75.0,
			},
			Realtime: conf.RealtimeSettings{
				Luistervink: conf.LuistervinkSettings{
					Enabled:   testConfig.Enabled,
					Token:     testConfig.Token,
					Threshold: testConfig.Threshold,
					Debug:     testConfig.Debug,
				},
			},
		}
	} else {
		// For GET requests, use the current settings
		settings = h.Settings
	}

	// Check if Luistervink is enabled
	if !settings.Realtime.Luistervink.Enabled {
		return h.NewHandlerError(
			nil,
			"Luistervink is not enabled in settings",
			http.StatusBadRequest,
		)
	}

	// Check if the API token is provided
	if settings.Realtime.Luistervink.Token == "" {
		return h.NewHandlerError(
			nil,
			"Luistervink API token is not configured",
			http.StatusBadRequest,
		)
	}

	// Create a temporary Luistervink client for testing
	lvClient, err := luistervink.New(settings)
	if err != nil {
		return h.NewHandlerError(err, "Failed to create Luistervink client", http.StatusInternalServerError)
	}

	// Create context with timeout for the test
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Set up streaming response
	c.Response().Header().Set("Content-Type", "application/x-ndjson")
	c.Response().WriteHeader(http.StatusOK)

	// Create a channel to receive test results
	resultChan := make(chan luistervink.TestResult)

	// Start test in a goroutine
	go func() {
		defer close(resultChan)
		lvClient.TestConnection(ctx, resultChan)
	}()

	// Stream results to client
	enc := json.NewEncoder(c.Response())
	for result := range resultChan {
		// Process the result
		h.processLuistervinkTestResult(&result)

		// Send the processed result
		if err := enc.Encode(result); err != nil {
			// If we can't write to the response, client probably disconnected
			return nil
		}
		c.Response().Flush()
	}

	// Clean up
	lvClient.Close()

	return nil
}

// processLuistervinkTestResult processes a Luistervink test result and adds useful information
func (h *Handlers) processLuistervinkTestResult(result *luistervink.TestResult) {
	if !result.Success {
		// Generate a user-friendly troubleshooting hint
		hint := generateLuistervinkTroubleshootingHint(result)

		// Format the error message for the UI
		switch {
		case strings.Contains(result.Error, "404") || strings.Contains(result.Error, "not found"):
			url := extractURLFromError(result.Error)
			errorMsg := result.Error
			if url != "" {
				errorMsg = fmt.Sprintf("URL not found (404): %s", url)
			}
			result.Message = fmt.Sprintf("%s %s %s",
				result.Message,
				errorMsg,
				hint)
		case hint != "":
			result.Message = fmt.Sprintf("%s %s %s",
				result.Message,
				result.Error,
				hint)
		}
		result.Error = ""
	} else {
		// Mark progress messages
		result.IsProgress = strings.Contains(strings.ToLower(result.Message), "testing") ||
			strings.Contains(strings.ToLower(result.Message), "posting")

		// Add formatting for successful completion stages
		if !result.IsProgress && result.Stage == stageLuistervinkDetectionPost {
			result.Message = fmt.Sprintf("%s You are ready to continue using the Luistervink integration.",
				result.Message)
		}
	}
}

// generateLuistervinkTroubleshootingHint provides context-specific troubleshooting suggestions
func generateLuistervinkTroubleshootingHint(result *luistervink.TestResult) string {
	if result.Success {
		return ""
	}

	switch result.Stage {
	case stageLuistervinkAPIConnectivity:
		switch {
		case strings.Contains(result.Error, "404"):
			return "The Luistervink API endpoint returned a 404 error. Please check for any announcements from Luistervink about API changes, or try again later."
		case strings.Contains(result.Error, "timeout") || strings.Contains(result.Error, "i/o timeout"):
			return "Check your internet connection and ensure that api.luistervink.nl is accessible from your network."
		case strings.Contains(result.Error, "no such host") || strings.Contains(result.Error, "DNS"):
			return "Could not resolve the Luistervink API hostname. Please check your DNS configuration and internet connectivity."
		default:
			return "Unable to connect to the Luistervink API. Verify your internet connection and network settings."
		}

	case stageLuistervinkAuthentication:
		switch {
		case strings.Contains(result.Error, "404"):
			return "The Luistervink authentication endpoint could not be found. Please verify that your API token is correct."
		case strings.Contains(result.Error, "invalid token") || strings.Contains(result.Error, "401") || strings.Contains(result.Error, "403"):
			return "Authentication failed. Please verify that your Luistervink API token is correct and active."
		default:
			return "Failed to authenticate with Luistervink. Check your API token and ensure it is properly registered."
		}

	case stageLuistervinkDetectionPost:
		return "Failed to post the test detection. This could be due to network issues, authentication problems, or the Luistervink service might be experiencing issues."

	default:
		return "Something went wrong with the Luistervink test. Check your connection settings and try again."
	}
}
