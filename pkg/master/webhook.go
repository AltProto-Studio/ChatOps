package master

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"gopass/pkg/db"
)

// WebhookPayload represents the expected payload from external CI (e.g. Jenkins, GitLab)
type WebhookPayload struct {
	ProjectName string `json:"project_name"`
	ImageTag    string `json:"image_tag"`
	Status      string `json:"status"`
	Message     string `json:"message"`
}

// StartWebhookServer starts an HTTP server to listen for CI/CD callbacks
func StartWebhookServer(port string, mgr *db.Manager) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/webhook/ci-finished", func(w http.ResponseWriter, r *http.Request) {
		handleCIFinished(w, r, mgr)
	})

	serverAddr := port
	if serverAddr == "" {
		serverAddr = ":8080" // Default port if not provided
	}

	log.Printf("[Webhook Server] Starting CI Webhook receiver on %s/api/webhook/ci-finished", serverAddr)
	
	err := http.ListenAndServe(serverAddr, mux)
	if err != nil {
		return fmt.Errorf("webhook server failed: %w", err)
	}
	return nil
}

// handleCIFinished processes the callback when external CI completes building a Docker image
func handleCIFinished(w http.ResponseWriter, r *http.Request, mgr *db.Manager) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Security: Require Bearer token authentication matching the communication token
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		log.Printf("[SECURITY WARNING] Webhook received request without Bearer token from %s", r.RemoteAddr)
		http.Error(w, "Unauthorized: Missing or invalid token", http.StatusUnauthorized)
		return
	}
	
	providedToken := strings.TrimPrefix(authHeader, "Bearer ")
	expectedToken, err := mgr.GetCommunicationToken()
	if err != nil || providedToken != expectedToken {
		log.Printf("[SECURITY WARNING] Webhook rejected due to invalid token from %s", r.RemoteAddr)
		http.Error(w, "Unauthorized: Invalid token", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload WebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	log.Printf("[Webhook Receiver] Received CI callback for project '%s'. Status: %s, Image: %s", 
		payload.ProjectName, payload.Status, payload.ImageTag)

	if payload.Status == "success" {
		// In a full implementation, we would look up the pending deployment task
		// and trigger the gopass-agent to pull the image and deploy it.
		// For now, we just acknowledge receipt.
		log.Printf("[Webhook Receiver] Triggering Agent deployment logic for %s:%s...", payload.ProjectName, payload.ImageTag)
	} else {
		log.Printf("[Webhook Receiver] CI reported failure: %s", payload.Message)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Webhook received and processed successfully\n"))
}
