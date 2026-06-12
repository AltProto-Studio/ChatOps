package agent

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// DownloadAndReplaceAgentBinary downloads the agent binary from URL and replaces the running executable
func DownloadAndReplaceAgentBinary(downloadURL string) error {
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
	resp, err := client.Get(downloadURL)
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

	// 2. Rename swap
	oldPath := execPath + ".old"
	if runtime.GOOS == "windows" {
		oldPath = execPath + ".old.exe"
	}

	_ = os.Remove(oldPath)

	if err := os.Rename(execPath, oldPath); err != nil {
		return fmt.Errorf("failed to rename running binary to old: %w", err)
	}

	if err := os.Rename(tempFilePath, execPath); err != nil {
		_ = os.Rename(oldPath, execPath)
		return fmt.Errorf("failed to replace running binary: %w", err)
	}

	if runtime.GOOS != "windows" {
		_ = os.Chmod(execPath, 0755)
	}

	return nil
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
