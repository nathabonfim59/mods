package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	CopilotChatAuthURL   = "https://api.github.com/copilot_internal/v2/token"
	CopilotEditorVersion = "vscode/1.95.3"
	CopilotUserAgent     = "curl/7.81.0" // Necessay to bypass the user-agent check
)

type CopilotAccessToken struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	Endpoints struct {
		API           string `json:"api"` // Can change in Github Enterprise instances
		OriginTracker string `json:"origin-tracker"`
		Proxy         string `json:"proxy"`
		Telemetry     string `json:"telemetry"`
	} `json:"endpoints"`
	ErrorDetails *struct {
		URL            string `json:"url,omitempty"`
		Message        string `json:"message,omitempty"`
		Title          string `json:"title,omitempty"`
		NotificationID string `json:"notification_id,omitempty"`
	} `json:"error_details,omitempty"`
}

type CopilotHTTPClient struct {
	client      *http.Client
	AccessToken *CopilotAccessToken
}

func NewCopilotHTTPClient() *CopilotHTTPClient {
	return &CopilotHTTPClient{
		client: &http.Client{},
	}
}

func (c *CopilotHTTPClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Editor-Version", CopilotEditorVersion)
	req.Header.Set("User-Agent", CopilotUserAgent)

	var isTokenExpired bool = c.AccessToken != nil && c.AccessToken.ExpiresAt < time.Now().Unix()

	if c.AccessToken == nil || isTokenExpired {
		accessToken, err := getCopilotAccessToken(c.client)
		if err != nil {
			return nil, fmt.Errorf("failed to get access token: %w", err)
		}
		c.AccessToken = &accessToken
	}

	if c.AccessToken != nil {
		req.Header.Set("Authorization", "Bearer "+c.AccessToken.Token)
	}

	return c.client.Do(req)
}

func getCopilotRefreshToken() (string, error) {
	configPath := filepath.Join(os.Getenv("HOME"), ".config/github-copilot")
	if runtime.GOOS == "windows" {
		configPath = filepath.Join(os.Getenv("LOCALAPPDATA"), "github-copilot")
	}

	// Check both possible config file locations
	configFiles := []string{
		filepath.Join(configPath, "hosts.json"),
		filepath.Join(configPath, "apps.json"),
	}

	// Try to get token from config files
	for _, path := range configFiles {
		token, err := extractCopilotTokenFromFile(path)
		if err == nil && token != "" {
			return token, nil
		}
	}

	return "", fmt.Errorf("no token found in %s", strings.Join(configFiles, ", "))
}

func extractCopilotTokenFromFile(path string) (string, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	var config map[string]json.RawMessage
	if err := json.Unmarshal(bytes, &config); err != nil {
		return "", err
	}

	for key, value := range config {
		if key == "github.com" || strings.HasPrefix(key, "github.com:") {
			var tokenData map[string]string
			if err := json.Unmarshal(value, &tokenData); err != nil {
				continue
			}
			if token, exists := tokenData["oauth_token"]; exists {
				return token, nil
			}
		}
	}

	return "", fmt.Errorf("no token found in %s", path)
}

func getCopilotAccessToken(client *http.Client) (CopilotAccessToken, error) {
	cache, err := NewExpiringCache[CopilotAccessToken]()
	if err == nil {
		var token CopilotAccessToken
		err = cache.Read("copilot", func(r io.Reader) error {
			return json.NewDecoder(r).Decode(&token)
		})
		if err == nil && token.ExpiresAt > time.Now().Unix() {
			return token, nil
		}
	}

	refreshToken, err := getCopilotRefreshToken()
	if err != nil {
		return CopilotAccessToken{}, fmt.Errorf("failed to get refresh token: %w", err)
	}

	tokenReq, err := http.NewRequest(http.MethodGet, CopilotChatAuthURL, nil)
	if err != nil {
		return CopilotAccessToken{}, fmt.Errorf("failed to create token request: %w", err)
	}

	tokenReq.Header.Set("Authorization", "token "+refreshToken)
	tokenReq.Header.Set("Accept", "application/json")
	tokenReq.Header.Set("Editor-Version", CopilotEditorVersion)
	tokenReq.Header.Set("User-Agent", CopilotUserAgent)

	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return CopilotAccessToken{}, fmt.Errorf("failed to get access token: %w", err)
	}
	defer tokenResp.Body.Close()

	var tokenResponse CopilotAccessToken
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenResponse); err != nil {
		return CopilotAccessToken{}, fmt.Errorf("failed to decode token response: %w", err)
	}

	if tokenResponse.ErrorDetails != nil {
		return CopilotAccessToken{}, fmt.Errorf("token error: %s", tokenResponse.ErrorDetails.Message)
	}

	if cache != nil {
		if err := cache.Write("copilot", tokenResponse.ExpiresAt, func(w io.Writer) error {
			return json.NewEncoder(w).Encode(tokenResponse)
		}); err != nil {
			return CopilotAccessToken{}, fmt.Errorf("failed to cache token: %w", err)
		}
	}

	return tokenResponse, nil
}
