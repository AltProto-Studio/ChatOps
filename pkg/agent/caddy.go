package agent

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// CaddyManager handles reverse proxy routing configurations
type CaddyManager struct {
	adminAPI      string
	caddyfilePath string
}

// NewCaddyManager initializes CaddyManager with default configurations
func NewCaddyManager() *CaddyManager {
	return &CaddyManager{
		adminAPI:      "http://localhost:2019",
		caddyfilePath: "Caddyfile",
	}
}

// UpdateRoute updates reverse proxy settings for a domain to direct to hostPort
func (c *CaddyManager) UpdateRoute(domain string, hostPort int, useSSL bool) error {
	log.Printf("[Caddy] Updating routing: %s -> localhost:%d (SSL: %v)", domain, hostPort, useSSL)

	targetDomain := domain
	if !useSSL && !strings.HasPrefix(targetDomain, "http://") {
		targetDomain = "http://" + targetDomain
	}

	cleanDomain := strings.TrimPrefix(domain, "http://")
	cleanDomain = strings.TrimPrefix(cleanDomain, "https://")

	// Try method 1: REST API (Memory configuration)
	err := c.updateViaAPI(cleanDomain, hostPort, useSSL)
	if err == nil {
		log.Println("[Caddy] Successfully updated routing via REST API.")
		return nil
	}
	log.Printf("[Caddy] REST API update failed (%v). Falling back to Caddyfile modification...", err)

	// Try method 2: Caddyfile (File configuration + Shell reload)
	return c.updateViaCaddyfile(targetDomain, hostPort)
}

func (c *CaddyManager) updateViaAPI(domain string, hostPort int, useSSL bool) error {
	listenPorts := `[":80", ":443"]`
	if !useSSL {
		listenPorts = `[":80"]`
	}

	caddyJSONConfig := fmt.Sprintf(`{
		"apps": {
			"http": {
				"servers": {
					"srv0": {
						"listen": %s,
						"routes": [
							{
								"match": [{"host": ["%s"]}],
								"handle": [
									{
										"handler": "reverse_proxy",
										"upstreams": [{"dial": "localhost:%d"}]
									}
								]
							}
						]
					}
				}
			}
		}
	}`, listenPorts, domain, hostPort)

	client := http.Client{Timeout: 1 * time.Second}
	req, err := http.NewRequest("POST", c.adminAPI+"/load", bytes.NewBuffer([]byte(caddyJSONConfig)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("caddy load returned status: %d", resp.StatusCode)
	}

	return nil
}

func (c *CaddyManager) updateViaCaddyfile(domain string, hostPort int) error {
	var content string

	// 1. Read existing Caddyfile or create new
	data, err := os.ReadFile(c.caddyfilePath)
	if err == nil {
		content = string(data)
	} else if os.IsNotExist(err) {
		log.Println("[Caddy] Caddyfile not found, creating new Caddyfile...")
		content = ""
	} else {
		return fmt.Errorf("failed to read Caddyfile: %w", err)
	}

	// 2. Modify routing definition
	// Regex matches: domain { \n reverse_proxy localhost:port \n }
	pattern := fmt.Sprintf(`(?s)%s\s*\{\s*reverse_proxy\s+localhost:\d+\s*\n\s*\}`, regexp.QuoteMeta(domain))
	re := regexp.MustCompile(pattern)

	newEntry := fmt.Sprintf("%s {\n\treverse_proxy localhost:%d\n}", domain, hostPort)

	if re.MatchString(content) {
		// Replace existing block
		content = re.ReplaceAllString(content, newEntry)
		log.Printf("[Caddy] Updated existing domain '%s' in Caddyfile", domain)
	} else {
		// Append new block
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += newEntry + "\n"
		log.Printf("[Caddy] Appended new domain '%s' to Caddyfile", domain)
	}

	// 3. Write Caddyfile
	if err := os.WriteFile(c.caddyfilePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write Caddyfile: %w", err)
	}

	// 4. Trigger reload
	_, err = exec.LookPath("caddy")
	if err == nil {
		cmd := exec.Command("caddy", "reload", "--config", c.caddyfilePath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("caddy reload failed: %w (output: %s)", err, string(out))
		}
		log.Println("[Caddy] Executed 'caddy reload' successfully.")
	} else {
		log.Println("[Caddy] WARNING: 'caddy' CLI executable not found on PATH. Skipping reload command execution.")
	}

	return nil
}
