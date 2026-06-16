package cloudflare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DNSRecord represents a Cloudflare DNS record
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// CFResponse represents the Cloudflare API envelope response
type CFResponse struct {
	Result  []DNSRecord `json:"result"`
	Success bool        `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// DNSClient is a simple client for Cloudflare DNS Operations
type DNSClient struct {
	APIToken string
	ZoneID   string
	client   *http.Client
}

// NewDNSClient initializes a new Cloudflare DNS client
func NewDNSClient(apiToken, zoneID string) *DNSClient {
	return &DNSClient{
		APIToken: apiToken,
		ZoneID:   zoneID,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// CreateOrUpdateRecord checks for an existing record with the name, then updates or creates it
func (c *DNSClient) CreateOrUpdateRecord(name, content string, proxied bool) error {
	if c.APIToken == "" {
		return fmt.Errorf("cloudflare API Token is not configured")
	}

	if c.ZoneID == "" {
		zones, err := ListZones(c.APIToken)
		if err != nil {
			return fmt.Errorf("failed to list zones for auto-discovery: %w", err)
		}
		for _, z := range zones {
			if strings.HasSuffix(name, z.Name) {
				c.ZoneID = z.ID
				break
			}
		}
		if c.ZoneID == "" {
			return fmt.Errorf("could not find matching zone ID for domain %s", name)
		}
	}

	// 1. Determine record type (A vs CNAME)
	recordType := "CNAME"
	if ip := net.ParseIP(content); ip != nil {
		recordType = "A"
	}

	// 2. Search for existing record with the exact name
	searchURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s", c.ZoneID, name)
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create search request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute search request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read search response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("search DNS records failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var searchResp CFResponse
	if err := json.Unmarshal(bodyBytes, &searchResp); err != nil {
		return fmt.Errorf("failed to parse search response JSON: %w", err)
	}

	// 3. Create or Update
	recordPayload := DNSRecord{
		Type:    recordType,
		Name:    name,
		Content: content,
		TTL:     1, // 1 for automatic/proxied
		Proxied: proxied,
	}

	payloadBytes, err := json.Marshal(recordPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	var writeURL string
	var method string
	var existingRecordID string

	if len(searchResp.Result) > 0 {
		// Found existing record, we update it
		existingRecordID = searchResp.Result[0].ID
		writeURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", c.ZoneID, existingRecordID)
		method = "PUT"
	} else {
		// No record found, we create a new one
		writeURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", c.ZoneID)
		method = "POST"
	}

	writeReq, err := http.NewRequest(method, writeURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create write request: %w", err)
	}
	writeReq.Header.Set("Authorization", "Bearer "+c.APIToken)
	writeReq.Header.Set("Content-Type", "application/json")

	writeResp, err := c.client.Do(writeReq)
	if err != nil {
		return fmt.Errorf("failed to execute write request: %w", err)
	}
	defer writeResp.Body.Close()

	writeBodyBytes, err := io.ReadAll(writeResp.Body)
	if err != nil {
		return fmt.Errorf("failed to read write response body: %w", err)
	}

	if writeResp.StatusCode != http.StatusOK && writeResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("write DNS record failed with status %d: %s", writeResp.StatusCode, string(writeBodyBytes))
	}

	return nil
}

// CFZone represents a Cloudflare Zone
type CFZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CFZonesResponse represents the response for listing zones
type CFZonesResponse struct {
	Result  []CFZone `json:"result"`
	Success bool     `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// ListZones fetches all zones available for the token
func ListZones(apiToken string) ([]CFZone, error) {
	req, err := http.NewRequest("GET", "https://api.cloudflare.com/client/v4/zones?status=active", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list zones failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var zonesResp CFZonesResponse
	if err := json.Unmarshal(bodyBytes, &zonesResp); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	if !zonesResp.Success {
		return nil, fmt.Errorf("API reported failure: %v", zonesResp.Errors)
	}

	return zonesResp.Result, nil
}
