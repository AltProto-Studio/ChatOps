package master

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"golang.org/x/crypto/ssh"
)

// CompileAgentForLinux dynamically cross-compiles gopass-agent for Linux amd64.
// Returns the compiled binary bytes or an error.
func CompileAgentForLinux() ([]byte, error) {
	tempFile := "gopass-agent-linux-temp-" + fmt.Sprintf("%d", time.Now().UnixNano())
	log.Printf("[Deploy] Compiling gopass-agent for Linux amd64 to %s...", tempFile)

	// Go build command
	cmd := exec.Command("go", "build", "-o", tempFile, "./cmd/gopass-agent")
	
	// Set Go environment variables for cross-compilation
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	
	// Run build
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("compilation failed: %w (stderr: %s)", err, stderr.String())
	}
	defer os.Remove(tempFile) // Ensure temp file is cleaned up

	// Read binary bytes
	data, err := os.ReadFile(tempFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read compiled binary: %w", err)
	}

	log.Printf("[Deploy] Successfully compiled Linux binary (size: %d bytes)", len(data))
	return data, nil
}

// DeployAgentToRemote SSH-connects to the remote server, uploads the agent binary, and starts it.
func DeployAgentToRemote(ip string, port int, username string, password string, privateKey string, agentBinary []byte, masterAddr string, token string, alias string, tlsEnabled bool, tlsSkipVerify bool) error {
	var auth []ssh.AuthMethod

	if privateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(privateKey))
		if err != nil {
			return fmt.Errorf("failed to parse private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	} else if password != "" {
		auth = append(auth, ssh.Password(password))
	} else {
		return fmt.Errorf("neither password nor private key provided")
	}

	addr := fmt.Sprintf("%s:%d", ip, port)
	// SECURITY WARNING: InsecureIgnoreHostKey makes the connection susceptible to MITM attacks.
	// It is kept for UX convenience of "one-click deploy" to new nodes.
	log.Printf("[SECURITY WARNING] SSH HostKey verification is disabled. Connection to %s is susceptible to MITM.", addr)
	config := &ssh.ClientConfig{
		User:            username,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // Automatically accept host keys in bot setup
		Timeout:         15 * time.Second,
	}

	log.Printf("[Deploy] Connecting to remote host %s via SSH...", addr)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()

	// 1. Upload the binary over SSH session stdin
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session for upload: %w", err)
	}
	defer session.Close()

	remotePath := "~/gopass-agent"
	log.Printf("[Deploy] Uploading agent binary to %s...", remotePath)
	
	session.Stdin = bytes.NewReader(agentBinary)
	// Write file using cat redirection
	err = session.Run(fmt.Sprintf("cat > %s", remotePath))
	if err != nil {
		return fmt.Errorf("failed to upload binary: %w", err)
	}

	// 2. Chmod +x the binary
	chmodSession, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session for chmod: %w", err)
	}
	defer chmodSession.Close()

	log.Printf("[Deploy] Setting executable permissions on %s...", remotePath)
	err = chmodSession.Run(fmt.Sprintf("chmod +x %s", remotePath))
	if err != nil {
		return fmt.Errorf("failed to set execution permission: %w", err)
	}

	// 3. Start the agent in background
	runSession, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session for run: %w", err)
	}
	defer runSession.Close()

	tlsFlags := ""
	if tlsEnabled {
		tlsFlags = " -tls-enabled=true"
		if tlsSkipVerify {
			tlsFlags += " -tls-skip-verify=true"
		}
	}

	// Stdin redirected from /dev/null prevents the SSH session from blocking/hanging
	cmdStr := fmt.Sprintf("nohup %s -master %s -token %s -alias %s%s < /dev/null > ~/gopass-agent.log 2>&1 &", remotePath, masterAddr, token, alias, tlsFlags)
	log.Printf("[Deploy] Executing startup command: %s", cmdStr)
	
	err = runSession.Run(cmdStr)
	if err != nil {
		return fmt.Errorf("failed to execute agent startup command: %w", err)
	}

	log.Printf("[Deploy] Agent deployment initiated on %s", ip)
	return nil
}
