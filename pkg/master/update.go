package master

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrNoReleaseFound is returned when the repository has no releases
var ErrNoReleaseFound = errors.New("未找到发布版本 (404)")

// GitHubReleaseAsset represents a single asset inside a GitHub Release
type GitHubReleaseAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// GitHubRelease represents the structure of the latest release from GitHub API
type GitHubRelease struct {
	TagName string               `json:"tag_name"`
	HTMLURL string               `json:"html_url"`
	Body    string               `json:"body"`
	Assets  []GitHubReleaseAsset `json:"assets"`
}

// FetchLatestRelease queries the GitHub API for the latest ChatOps release
func FetchLatestRelease(githubToken string) (*GitHubRelease, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", "https://api.github.com/repos/AltProto-Studio/ChatOps/releases/latest", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "ChatOps-Updater")

	// Authentication token fallback chain
	token := githubToken
	if token == "" {
		token = os.Getenv("GOPASS_GITHUB_TOKEN")
	}
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoReleaseFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api returned status %d", resp.StatusCode)
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to decode json: %w", err)
	}

	return &release, nil
}

// parseVersion parses a semver string like "v1.2.0" into major, minor, patch integers
func parseVersion(v string) (int, int, int, error) {
	if len(v) > 0 && v[0] == 'v' {
		v = v[1:]
	}
	var major, minor, patch int
	_, err := fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch)
	if err != nil {
		return 0, 0, 0, err
	}
	return major, minor, patch, nil
}

// IsNewerVersion returns true if latest is semantically newer than current
func IsNewerVersion(current, latest string) bool {
	curMajor, curMinor, curPatch, err1 := parseVersion(current)
	latMajor, latMinor, latPatch, err2 := parseVersion(latest)
	if err1 != nil || err2 != nil {
		// Fallback to simple string comparison if parsing fails
		return latest > current
	}
	if latMajor != curMajor {
		return latMajor > curMajor
	}
	if latMinor != curMinor {
		return latMinor > curMinor
	}
	return latPatch > curPatch
}

// DownloadAndReplaceBinary downloads a binary from URL or asset ID and replaces the currently running executable
func DownloadAndReplaceBinary(asset *GitHubReleaseAsset, githubToken string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %w", err)
	}

	// Make sure we download to the same directory to avoid cross-volume rename failures
	dir := filepath.Dir(execPath)
	tempFilePath := filepath.Join(dir, "gopass.new.tmp")

	// 1. Download new binary
	out, err := os.OpenFile(tempFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer func() {
		out.Close()
		_ = os.Remove(tempFilePath) // Clean up if we didn't succeed
	}()

	client := &http.Client{Timeout: 120 * time.Second}
	var req *http.Request

	// Resolve token for request
	token := githubToken
	if token == "" {
		token = os.Getenv("GOPASS_GITHUB_TOKEN")
	}
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}

	// Use Private API download endpoint if token is present
	if token != "" && asset.ID != 0 {
		apiURL := fmt.Sprintf("https://api.github.com/repos/AltProto-Studio/ChatOps/releases/assets/%d", asset.ID)
		req, err = http.NewRequest("GET", apiURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create API request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/octet-stream")
	} else {
		req, err = http.NewRequest("GET", asset.BrowserDownloadURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
	}
	req.Header.Set("User-Agent", "ChatOps-Updater")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download new binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download server returned status %d", resp.StatusCode)
	}

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write download to disk: %w", err)
	}
	out.Close()

	// 2. Extract the executable from the archive
	extractedPath := filepath.Join(dir, "gopass.extracted.tmp")
	err = extractExecutable(tempFilePath, extractedPath, asset.Name)
	if err != nil {
		return fmt.Errorf("failed to extract executable from archive: %w", err)
	}
	defer os.Remove(extractedPath)

	// Ensure executable permissions on Unix for testing
	if runtime.GOOS != "windows" {
		_ = os.Chmod(extractedPath, 0755)
	}

	// 3. Health check (Test the new binary)
	testCmd := exec.Command(extractedPath, "--test")
	if err := testCmd.Run(); err != nil {
		return fmt.Errorf("new binary failed health check (architecture mismatch or corrupted): %w", err)
	}

	// 4. Perform the rename swap
	oldPath := execPath + ".old"
	if runtime.GOOS == "windows" {
		oldPath = execPath + ".old.exe"
	}
	
	// Remove any previous .old file
	_ = os.Remove(oldPath)

	// Rename running binary to .old
	if err := os.Rename(execPath, oldPath); err != nil {
		return fmt.Errorf("failed to rename running binary to old: %w", err)
	}

	// Rename new binary to running path
	if err := os.Rename(extractedPath, execPath); err != nil {
		// Try to restore original if possible
		_ = os.Rename(oldPath, execPath)
		return fmt.Errorf("failed to replace running binary: %w", err)
	}

	return nil
}

func extractExecutable(archivePath, targetPath, assetName string) error {
	if strings.HasSuffix(assetName, ".zip") {
		r, err := zip.OpenReader(archivePath)
		if err != nil {
			return err
		}
		defer r.Close()

		for _, f := range r.File {
			if strings.Contains(f.Name, "gopass-master") || strings.Contains(f.Name, "gopass-agent") {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				defer rc.Close()
				out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
				if err != nil {
					return err
				}
				defer out.Close()
				_, err = io.Copy(out, rc)
				return err
			}
		}
		return errors.New("executable not found in zip archive")
	}

	if strings.HasSuffix(assetName, ".tar.gz") {
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer f.Close()

		gzr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gzr.Close()

		tr := tar.NewReader(gzr)
		for {
			header, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			if strings.Contains(header.Name, "gopass-master") || strings.Contains(header.Name, "gopass-agent") {
				out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
				if err != nil {
					return err
				}
				defer out.Close()
				_, err = io.Copy(out, tr)
				return err
			}
		}
		return errors.New("executable not found in tar.gz archive")
	}

	// If it's not an archive (e.g. raw binary), just rename/copy it
	return os.Rename(archivePath, targetPath)
}

// RestartProcess stops running services, spawns the new binary, and exits the parent
func RestartProcess(dbMgr interface{ Close() error }, gRPCServer interface{ Stop() }, chatID int64) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	log.Println("[Updater] Initiating graceful restart of ChatOps process...")

	// 1. Gracefully close DB manager
	if dbMgr != nil {
		log.Println("[Updater] Closing database manager...")
		_ = dbMgr.Close()
	}

	// 2. Stop gRPC server to release TCP port
	if gRPCServer != nil {
		log.Println("[Updater] Stopping gRPC server...")
		gRPCServer.Stop()
	}

	// Give a tiny moment for OS to release resources/ports
	time.Sleep(500 * time.Millisecond)

	// 3. Spawn the new process
	args := os.Args[1:]
	if chatID > 0 {
		args = append(args, fmt.Sprintf("--update-success-chat-id=%d", chatID))
	}
	cmd := exec.Command(execPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("failed to start new process: %w", err)
	}

	log.Println("[Updater] New process spawned successfully. Exiting parent process.")
	os.Exit(0)

	return nil
}

// GetMatchingAsset returns the release asset matching the current OS and Arch
func (r *GitHubRelease) GetMatchingAsset(component string) *GitHubReleaseAsset {
	// Component is "master" or "agent"
	// OS: "windows", "linux", "darwin"
	// Arch: "amd64", "arm64"
	goOS := runtime.GOOS
	goArch := runtime.GOARCH

	expectedSuffix := fmt.Sprintf("%s-%s", goOS, goArch)
	if goOS == "windows" {
		expectedSuffix = fmt.Sprintf("%s-%s.exe", goOS, goArch)
	}

	// Assets names are expected to be e.g. "gopass-master-windows-amd64.exe" or "gopass-agent-linux-amd64"
	for _, asset := range r.Assets {
		expectedName := fmt.Sprintf("gopass-%s-%s", component, expectedSuffix)
		if asset.Name == expectedName {
			return &asset
		}
	}
	
	// Fallback to name containing both component and os/arch if exact match fails
	for _, asset := range r.Assets {
		if filepath.Base(asset.Name) != asset.Name {
			continue
		}
		if component == "master" && !contains(asset.Name, "master") {
			continue
		}
		if component == "agent" && !contains(asset.Name, "agent") {
			continue
		}
		if contains(asset.Name, goOS) && contains(asset.Name, goArch) {
			return &asset
		}
	}

	return nil
}

// Simple helper to check if a string contains another substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && stringContains(s, substr)
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
