package agent

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

type GitHubReleaseAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type GitHubRelease struct {
	Assets []GitHubReleaseAsset `json:"assets"`
}

// DownloadAndReplaceAgentBinary downloads the agent binary from URL (or GitHub API if token is present)
func DownloadAndReplaceAgentBinary(downloadURL string, githubToken string, tagName string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %w", err)
	}

	dir := filepath.Dir(execPath)
	tempFilePath := filepath.Join(dir, "gopass-agent.new.tmp")

	// 1. Download
	out, err := os.OpenFile(tempFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer func() {
		out.Close()
		_ = os.Remove(tempFilePath)
	}()

	client := &http.Client{Timeout: 120 * time.Second}
	var req *http.Request

	assetName := "gopass-agent" // default fallback
	// If token and tag are present, fetch asset ID first to support private repos
	if githubToken != "" && tagName != "" {
		log.Printf("[Agent Updater] Private repo update: fetching release info for tag %s...", tagName)
		apiURL := fmt.Sprintf("https://api.github.com/repos/AltProto-Studio/ChatOps/releases/tags/%s", tagName)
		
		infoReq, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create release info request: %w", err)
		}
		infoReq.Header.Set("Authorization", "Bearer "+githubToken)
		infoReq.Header.Set("User-Agent", "ChatOps-Agent-Updater")

		infoResp, err := client.Do(infoReq)
		if err != nil {
			return fmt.Errorf("failed to fetch release info: %w", err)
		}
		defer infoResp.Body.Close()

		if infoResp.StatusCode != http.StatusOK {
			return fmt.Errorf("github api returned status %d while fetching release info", infoResp.StatusCode)
		}

		var release GitHubRelease
		if err := json.NewDecoder(infoResp.Body).Decode(&release); err != nil {
			return fmt.Errorf("failed to decode release info: %w", err)
		}

		// Find matching asset
		goOS := runtime.GOOS
		goArch := runtime.GOARCH
		expectedSuffix := fmt.Sprintf("%s-%s", goOS, goArch)
		if goOS == "windows" {
			expectedSuffix = fmt.Sprintf("%s-%s.exe", goOS, goArch)
		}
		expectedName := fmt.Sprintf("gopass-agent-%s", expectedSuffix)

		var matchedAsset *GitHubReleaseAsset
		for _, asset := range release.Assets {
			if asset.Name == expectedName {
				matchedAsset = &asset
				break
			}
		}

		// Fallback fuzzy match
		if matchedAsset == nil {
			for _, asset := range release.Assets {
				if strings.Contains(asset.Name, "agent") && strings.Contains(asset.Name, goOS) && strings.Contains(asset.Name, goArch) {
					matchedAsset = &asset
					break
				}
			}
		}

		if matchedAsset == nil {
			return fmt.Errorf("failed to find matching asset in release for %s/%s", goOS, goArch)
		}

		assetName = matchedAsset.Name
		log.Printf("[Agent Updater] Matched private asset ID %d (%s). Downloading...", matchedAsset.ID, matchedAsset.Name)
		assetURL := fmt.Sprintf("https://api.github.com/repos/AltProto-Studio/ChatOps/releases/assets/%d", matchedAsset.ID)
		
		req, err = http.NewRequest("GET", assetURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create asset download request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+githubToken)
		req.Header.Set("Accept", "application/octet-stream")
	} else {
		// Public download
		req, err = http.NewRequest("GET", downloadURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		if tagName != "" {
			assetName = fmt.Sprintf("gopass-agent-%s-%s", runtime.GOOS, runtime.GOARCH)
			if runtime.GOOS == "windows" {
				assetName += ".zip"
			} else {
				assetName += ".tar.gz"
			}
		}
	}
	req.Header.Set("User-Agent", "ChatOps-Agent-Updater")

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
	extractedPath := filepath.Join(dir, "gopass-agent.extracted.tmp")
	err = extractExecutable(tempFilePath, extractedPath, assetName)
	if err != nil {
		return fmt.Errorf("failed to extract executable from archive: %w", err)
	}
	defer os.Remove(extractedPath)

	if runtime.GOOS != "windows" {
		_ = os.Chmod(extractedPath, 0755)
	}

	// 3. Health check (Test the new binary)
	testCmd := exec.Command(extractedPath, "--test")
	if err := testCmd.Run(); err != nil {
		return fmt.Errorf("new binary failed health check: %w", err)
	}

	// 4. Rename swap
	oldPath := execPath + ".old"
	if runtime.GOOS == "windows" {
		oldPath = execPath + ".old.exe"
	}

	_ = os.Remove(oldPath)

	if err := os.Rename(execPath, oldPath); err != nil {
		return fmt.Errorf("failed to rename running binary to old: %w", err)
	}

	if err := os.Rename(extractedPath, execPath); err != nil {
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

// RestartAgent spawns the new Agent process and exits the current one
func RestartAgent() error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	log.Println("[Agent Updater] Restarting Agent process...")

	cmd := exec.Command(execPath, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("failed to start new process: %w", err)
	}

	log.Println("[Agent Updater] New Agent spawned. Exiting.")
	os.Exit(0)
	return nil
}
