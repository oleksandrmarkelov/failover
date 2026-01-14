package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/failover/pkg/api"
	"github.com/failover/pkg/config"
	"github.com/failover/pkg/logging"
	"github.com/gagliardetto/solana-go/rpc"
)

// getPublicIP detects the public IP of this server
func getPublicIP() (string, error) {
	// Try to get public IP by connecting to a public server
	// This doesn't actually send any data, just determines the outbound IP
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", fmt.Errorf("failed to detect public IP: %w", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String(), nil
}

// ValidatorAgent handles health check requests and manages failover
type ValidatorAgent struct {
	config *config.ValidatorConfig

	// State
	mu              sync.RWMutex
	isActive        bool
	lastManagerPing time.Time
	lastTowerBackup time.Time
	missedPings     int
	startTime       time.Time // When agent started
	hasSeenManager  bool      // Whether we've received at least one manager ping

	// Outbound connectivity tracking (Echo/Ack mechanism)
	// Used to detect asymmetric network failures where we receive pings but can't respond
	lastSentTimestamp  int64 // Timestamp we included in our last response
	lastAckedTimestamp int64 // Last timestamp the manager confirmed receiving

	// HTTP client for peer communication
	httpClient *http.Client

	// Shutdown
	ctx    context.Context
	cancel context.CancelFunc
}

// NewValidatorAgent creates a new validator agent
func NewValidatorAgent(cfg *config.ValidatorConfig) *ValidatorAgent {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()

	agent := &ValidatorAgent{
		config:          cfg,
		isActive:        cfg.IsActiveOnStart,
		lastManagerPing: time.Time{}, // Zero time - indicates never received ping
		startTime:       now,
		hasSeenManager:  false,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		ctx:    ctx,
		cancel: cancel,
	}

	// Auto-detect local IP if not configured
	if cfg.LocalIP == "" {
		detectedIP, err := getPublicIP()
		if err != nil {
			log.Printf("Warning: Failed to auto-detect public IP: %v", err)
		} else {
			cfg.LocalIP = detectedIP
			log.Printf("Auto-detected public IP: %s", detectedIP)
		}
	}

	// Try to auto-detect active state from gossip if configured
	if cfg.GossipCheckCommand != "" && cfg.LocalIP != "" {
		log.Printf("Checking active state from gossip...")
		isActive, err := agent.checkActiveFromGossip()
		if err != nil {
			log.Printf("Warning: Failed to check gossip for active state: %v", err)
			log.Printf("Falling back to is_active_on_start=%v", cfg.IsActiveOnStart)
		} else {
			agent.isActive = isActive
			log.Printf("Auto-detected active state from gossip: is_active=%v", isActive)
		}
	}

	// If starting as passive, ensure identity symlink and identity are set to passive
	if !agent.isActive {
		agent.ensurePassiveIdentity()
	}

	return agent
}

// ensurePassiveIdentity updates symlink and identity to passive state on startup
func (va *ValidatorAgent) ensurePassiveIdentity() {
	log.Printf("Starting as passive - ensuring passive identity configuration...")

	// Update identity symlink to passive
	if va.config.PassiveIdentitySymlinkCommand != "" {
		log.Printf("Updating identity symlink to passive identity...")
		output, err := va.executeCommand(va.config.PassiveIdentitySymlinkCommand, false)
		if err != nil {
			log.Printf("WARNING: Failed to update identity symlink: %v", err)
		} else {
			log.Printf("Identity symlink updated: %s", strings.TrimSpace(output))
		}
	}

	// Switch to passive identity (with retry since RPC may not be available immediately)
	if va.config.IdentityRemoveCommand != "" {
		log.Printf("Switching to passive identity...")
		const maxRetries = 2
		const retryDelay = 5 * time.Second
		var output string
		var err error

		for attempt := 1; attempt <= maxRetries; attempt++ {
			output, err = va.executeCommand(va.config.IdentityRemoveCommand, false)
			if err == nil {
				log.Printf("Identity switched to passive: %s", strings.TrimSpace(output))
				break
			}
			if attempt < maxRetries {
				log.Printf("WARNING: Passive identity switch attempt %d/%d failed: %v. Retrying in %v...", attempt, maxRetries, err, retryDelay)
				time.Sleep(retryDelay)
			}
		}

		// If identity switch failed after all retries, restart the validator
		// This ensures the validator starts fresh with the passive identity symlink
		if err != nil {
			log.Printf("WARNING: Failed to switch to passive identity after %d attempts: %v", maxRetries, err)
			if va.config.ValidatorRestartCommand != "" {
				log.Printf("Restarting validator service to apply passive identity...")
				restartOutput, restartErr := va.executeCommand(va.config.ValidatorRestartCommand, false)
				if restartErr != nil {
					log.Printf("ERROR: Failed to restart validator service: %v", restartErr)
				} else {
					log.Printf("Validator service restarted: %s", strings.TrimSpace(restartOutput))
				}
			} else {
				log.Printf("WARNING: validator_restart_command not configured, cannot restart validator")
			}
		}
	}

	log.Printf("Passive identity configuration complete")
}

// isProcessRunning checks if agave-validator process is running (Ubuntu/Linux)
func (va *ValidatorAgent) isProcessRunning() bool {
	cmd := exec.Command("pgrep", "-x", va.config.ProcessName)
	err := cmd.Run()
	return err == nil
}

// getProcessUptime returns how long the process has been running in seconds
// Returns 0 if process is not running or uptime cannot be determined
func (va *ValidatorAgent) getProcessUptime() int64 {
	// Get PID of the process
	cmd := exec.Command("pgrep", "-x", va.config.ProcessName)
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	pid := strings.TrimSpace(string(output))
	if pid == "" {
		return 0
	}

	// Get elapsed time in seconds using ps
	// ps -o etimes= -p <PID> returns elapsed time in seconds
	cmd = exec.Command("ps", "-o", "etimes=", "-p", pid)
	output, err = cmd.Output()
	if err != nil {
		return 0
	}

	uptimeStr := strings.TrimSpace(string(output))
	uptime, err := strconv.ParseInt(uptimeStr, 10, 64)
	if err != nil {
		return 0
	}

	return uptime
}

// isProcessHealthy checks if process is running AND has been running long enough
// A freshly restarted process (crash loop) is not considered healthy
func (va *ValidatorAgent) isProcessHealthy() (running bool, uptime int64, healthy bool) {
	running = va.isProcessRunning()
	if !running {
		return false, 0, false
	}

	uptime = va.getProcessUptime()

	// Process must be running for at least 2 minutes to be considered healthy
	// This catches crash loops where systemd keeps restarting the process
	minUptime := int64(120) // 2 minutes in seconds
	healthy = uptime >= minUptime

	return running, uptime, healthy
}

// getGossipCheckCommand returns the gossip check command with validator_identity substituted
func (va *ValidatorAgent) getGossipCheckCommand() string {
	cmd := va.config.GossipCheckCommand
	if va.config.ValidatorIdentity != "" {
		cmd = strings.ReplaceAll(cmd, "{validator_identity}", va.config.ValidatorIdentity)
	}
	return cmd
}

// getTowerFilePath returns the tower file path with validator_identity substituted
func (va *ValidatorAgent) getTowerFilePath() string {
	path := va.config.TowerFilePath
	if va.config.ValidatorIdentity != "" {
		path = strings.ReplaceAll(path, "{validator_identity}", va.config.ValidatorIdentity)
	}
	return path
}

// checkActiveFromGossip checks if this server is the active validator by comparing
// the IP in gossip output with the local IP
// Returns: isActive, error (nil error means check was successful)
func (va *ValidatorAgent) checkActiveFromGossip() (bool, error) {
	if va.config.GossipCheckCommand == "" || va.config.LocalIP == "" {
		return false, fmt.Errorf("gossip check not configured (need gossip_check_command and local_ip)")
	}

	gossipCmd := va.getGossipCheckCommand()
	cmd := exec.Command("bash", "-c", gossipCmd)
	output, err := cmd.Output()
	if err != nil {
		// grep returns exit code 1 if no match found
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// No match in gossip - validator identity not found
			log.Printf("Validator identity not found in gossip")
			return false, nil
		}
		return false, fmt.Errorf("gossip check command failed: %w", err)
	}

	// Parse gossip output: first column is IP
	// Example: "80.251.153.166  | DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY | ..."
	line := strings.TrimSpace(string(output))
	if line == "" {
		return false, nil
	}

	// Split by | and get first field (IP)
	parts := strings.Split(line, "|")
	if len(parts) < 1 {
		return false, fmt.Errorf("unexpected gossip output format: %s", line)
	}

	gossipIP := strings.TrimSpace(parts[0])
	isActive := gossipIP == va.config.LocalIP

	log.Printf("Gossip check: gossip_ip=%s, local_ip=%s, is_active=%v", gossipIP, va.config.LocalIP, isActive)

	return isActive, nil
}

// getSlot gets current slot from local validator RPC
// Note: We only return the validator's own slot. The manager is responsible
// for checking the network slot from a trusted cluster RPC, because if this
// validator is disconnected from the network, its view of the network slot
// would be stale and unreliable.
func (va *ValidatorAgent) getSlot(ctx context.Context) (uint64, error) {
	client := rpc.New(va.config.LocalRPC)
	slot, err := client.GetSlot(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return 0, fmt.Errorf("failed to get slot: %w", err)
	}
	return slot, nil
}

// checkHealth checks the validator's RPC health
func (va *ValidatorAgent) checkHealth(ctx context.Context) bool {
	client := rpc.New(va.config.LocalRPC)
	health, err := client.GetHealth(ctx)
	if err != nil {
		return false
	}
	return health == "ok"
}

// executeCommand runs a shell command (Ubuntu/Linux)
func (va *ValidatorAgent) executeCommand(command string, dryRun bool) (string, error) {
	if command == "" {
		return "", fmt.Errorf("command is empty")
	}

	if dryRun || va.config.DryRun {
		log.Printf("[DRY-RUN] Would execute: %s", command)
		return "[DRY-RUN] Command not executed", nil
	}

	log.Printf("Executing: %s", command)

	cmd := exec.Command("bash", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("command failed: %w, output: %s", err, string(output))
	}

	return string(output), nil
}

// backupTower backs up tower file to etcd
func (va *ValidatorAgent) backupTower() error {
	va.mu.RLock()
	isActive := va.isActive
	va.mu.RUnlock()

	if !isActive {
		return nil // Only active validator backs up tower
	}

	output, err := va.executeCommand(va.config.TowerBackupCommand, false)
	if err != nil {
		return fmt.Errorf("tower backup failed: %w", err)
	}

	va.mu.Lock()
	va.lastTowerBackup = time.Now()
	va.mu.Unlock()

	log.Printf("Tower backup successful: %s", strings.TrimSpace(output))
	return nil
}

// restoreTower restores tower file from etcd
func (va *ValidatorAgent) restoreTower() error {
	output, err := va.executeCommand(va.config.TowerRestoreCommand, false)
	if err != nil {
		return fmt.Errorf("tower restore failed: %w", err)
	}

	log.Printf("Tower restore successful: %s", strings.TrimSpace(output))
	return nil
}

// becomeActive switches this validator to active mode
// If skipIdentity is true, only tower restore is performed (secure identity mode)
func (va *ValidatorAgent) becomeActive(reason string, skipIdentity bool) error {
	va.mu.Lock()
	if va.isActive {
		va.mu.Unlock()
		log.Printf("Already active, ignoring become_active request")
		return nil
	}
	va.mu.Unlock()

	if skipIdentity {
		log.Printf("=== BECOMING ACTIVE (tower only, secure mode) === Reason: %s", reason)
	} else {
		log.Printf("=== BECOMING ACTIVE === Reason: %s", reason)
	}

	// Step 1: Restore tower file from etcd
	log.Printf("Step 1: Restoring tower file from etcd...")
	if err := va.restoreTower(); err != nil {
		return fmt.Errorf("failed to restore tower: %w", err)
	}

	// In secure identity mode, skip identity symlink and identity change
	// Manager will set identity via SSH
	if skipIdentity {
		va.mu.Lock()
		va.isActive = true
		va.mu.Unlock()

		log.Printf("=== TOWER RESTORED (identity will be set by manager via SSH) ===")
		return nil
	}

	// Step 2: Update identity symlink to point to active identity
	if va.config.ActiveIdentitySymlinkCommand != "" {
		log.Printf("Step 2: Updating identity symlink to active identity...")
		symlinkOutput, symlinkErr := va.executeCommand(va.config.ActiveIdentitySymlinkCommand, false)
		if symlinkErr != nil {
			log.Printf("WARNING: Failed to update identity symlink: %v", symlinkErr)
		} else {
			log.Printf("Identity symlink updated: %s", strings.TrimSpace(symlinkOutput))
		}
	} else {
		log.Printf("Step 2: Skipping identity symlink update (active_identity_symlink_command not configured)")
	}

	// Step 3: Change identity
	log.Printf("Step 3: Changing validator identity...")
	output, err := va.executeCommand(va.config.IdentityChangeCommand, false)
	if err != nil {
		return fmt.Errorf("failed to change identity: %w", err)
	}
	log.Printf("Identity change output: %s", strings.TrimSpace(output))

	va.mu.Lock()
	va.isActive = true
	va.mu.Unlock()

	log.Printf("=== NOW ACTIVE ===")
	return nil
}

// becomePassive switches this validator to passive mode
func (va *ValidatorAgent) becomePassive(reason string) error {
	va.mu.Lock()
	if !va.isActive {
		va.mu.Unlock()
		log.Printf("Already passive, ignoring become_passive request")
		return nil
	}
	va.mu.Unlock()

	log.Printf("=== BECOMING PASSIVE === Reason: %s", reason)

	// Step 1: Backup tower file before going passive
	log.Printf("Step 1: Backing up tower file...")
	if err := va.backupTower(); err != nil {
		log.Printf("Warning: tower backup failed: %v", err)
	}

	// Step 2: Update identity symlink to point to unstaked identity
	if va.config.PassiveIdentitySymlinkCommand != "" {
		log.Printf("Step 2: Updating identity symlink to passive identity...")
		symlinkOutput, symlinkErr := va.executeCommand(va.config.PassiveIdentitySymlinkCommand, false)
		if symlinkErr != nil {
			log.Printf("WARNING: Failed to update identity symlink: %v", symlinkErr)
		} else {
			log.Printf("Identity symlink updated: %s", strings.TrimSpace(symlinkOutput))
		}
	} else {
		log.Printf("Step 2: Skipping identity symlink update (passive_identity_symlink_command not configured)")
	}

	// Step 3: Remove identity (with retry since RPC may not be available immediately)
	log.Printf("Step 3: Removing validator identity...")
	const maxRetries = 2
	const retryDelay = 3 * time.Second
	var output string
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		output, err = va.executeCommand(va.config.IdentityRemoveCommand, false)
		if err == nil {
			break
		}
		if attempt < maxRetries {
			log.Printf("WARNING: Identity removal attempt %d/%d failed: %v. Retrying in %v...", attempt, maxRetries, err, retryDelay)
			time.Sleep(retryDelay)
		}
	}

	// If identity removal failed after all retries, restart the validator
	// Restarting with passive identity symlink achieves the same result
	if err != nil {
		log.Printf("WARNING: Failed to remove identity after %d attempts: %v", maxRetries, err)
		if va.config.ValidatorRestartCommand != "" {
			log.Printf("Restarting validator service to apply passive identity...")
			restartOutput, restartErr := va.executeCommand(va.config.ValidatorRestartCommand, false)
			if restartErr != nil {
				log.Printf("ERROR: Failed to restart validator service: %v", restartErr)
			} else {
				log.Printf("Validator service restarted: %s", strings.TrimSpace(restartOutput))
				// Restart with passive symlink is equivalent to successful identity removal
				err = nil
			}
		} else {
			log.Printf("WARNING: validator_restart_command not configured, cannot restart validator")
		}
	} else {
		log.Printf("Identity remove output: %s", strings.TrimSpace(output))
	}

	// Mark as passive regardless of command result
	// This ensures we stop writing tower files even if the command fails
	va.mu.Lock()
	va.isActive = false
	va.mu.Unlock()

	// Step 4: Remove tower file to prevent stale tower usage
	towerFilePath := va.getTowerFilePath()
	if towerFilePath != "" {
		log.Printf("Step 4: Removing tower file: %s", towerFilePath)
		if va.config.DryRun {
			log.Printf("[DRY-RUN] Would remove tower file: %s", towerFilePath)
		} else {
			if removeErr := os.Remove(towerFilePath); removeErr != nil {
				if os.IsNotExist(removeErr) {
					log.Printf("Tower file does not exist (already removed): %s", towerFilePath)
				} else {
					log.Printf("WARNING: Failed to remove tower file: %v", removeErr)
				}
			} else {
				log.Printf("Tower file removed successfully")
			}
		}
	} else {
		log.Printf("Step 4: Skipping tower file removal (tower_file_path not configured)")
	}

	if err != nil {
		log.Printf("=== NOW PASSIVE (with identity removal error) ===")
		return fmt.Errorf("failed to remove identity: %w", err)
	}

	log.Printf("=== NOW PASSIVE ===")
	return nil
}

// checkPeerStatus checks if peer validator is alive and its status
func (va *ValidatorAgent) checkPeerStatus(ctx context.Context) (*api.PeerStatusResponse, error) {
	if va.config.PeerEndpoint == "" {
		return nil, fmt.Errorf("peer endpoint not configured")
	}

	reqBody := api.PeerStatusRequest{
		FromValidator: va.config.ListenAddr,
		Timestamp:     time.Now().Unix(),
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/peer-status", strings.TrimSuffix(va.config.PeerEndpoint, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := va.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("peer returned status %d: %s", resp.StatusCode, string(body))
	}

	var peerResp api.PeerStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&peerResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &peerResp, nil
}

// handleStatus handles the /status endpoint (from manager)
func (va *ValidatorAgent) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the request to get LastReceivedTimestamp (Echo/Ack mechanism)
	var req api.ValidatorStatusRequest
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// If we can't decode, continue with empty request (backwards compatibility)
			log.Printf("Warning: Failed to decode status request: %v", err)
		}
	}

	// Update last manager ping time and process Echo/Ack
	va.mu.Lock()
	va.lastManagerPing = time.Now()
	va.hasSeenManager = true // Mark that we've seen manager at least once
	va.missedPings = 0

	// Update lastAckedTimestamp based on what manager echoed back
	// This is used by managerWatchLoop to detect outbound connectivity failures
	if req.LastReceivedTimestamp > 0 {
		va.lastAckedTimestamp = req.LastReceivedTimestamp
	}
	va.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	response := api.ValidatorStatusResponse{
		Timestamp: time.Now().Unix(),
	}

	// Check if process is running and healthy (not recently restarted)
	running, uptime, healthy := va.isProcessHealthy()
	response.ProcessRunning = running
	response.ProcessUptime = uptime
	response.ProcessHealthy = healthy

	if !healthy && running {
		log.Printf("WARNING: Process running but recently restarted (uptime: %ds). May be in crash loop.", uptime)
	}

	// Get validator's current slot
	// Note: SlotDiff and NetworkSlot will be calculated by the manager
	// using a trusted cluster RPC, since this validator's view of the
	// network slot would be stale if it's disconnected.
	if response.ProcessRunning {
		slot, err := va.getSlot(ctx)
		if err != nil {
			response.Error = err.Error()
		} else {
			response.ValidatorSlot = slot
		}

		// Check RPC health
		response.IsHealthy = va.checkHealth(ctx)
	}

	// Get current state
	va.mu.RLock()
	response.IsActive = va.isActive
	response.LastTowerBackup = va.lastTowerBackup.Unix()
	va.mu.RUnlock()

	if response.Error != "" {
		log.Printf("Status request: ProcessRunning=%v, Slot=%d, Active=%v, Healthy=%v, Error=%s",
			response.ProcessRunning, response.ValidatorSlot, response.IsActive, response.IsHealthy, response.Error)
	} else {
		log.Printf("Status request: ProcessRunning=%v, Slot=%d, Active=%v, Healthy=%v",
			response.ProcessRunning, response.ValidatorSlot, response.IsActive, response.IsHealthy)
	}

	// Track the timestamp we're sending (for Echo/Ack mechanism)
	va.mu.Lock()
	va.lastSentTimestamp = response.Timestamp
	va.mu.Unlock()

	// Send response immediately, don't block on tower backup
	va.sendJSON(w, response)

	// Backup tower file after sending response (only if active)
	// Run in goroutine so slow etcd doesn't block next request
	if response.IsActive && response.ProcessRunning {
		go func() {
			if err := va.backupTower(); err != nil {
				log.Printf("Tower backup warning: %v", err)
			}
		}()
	}
}

// handlePeerStatus handles the /peer-status endpoint (from other validator)
func (va *ValidatorAgent) handlePeerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	va.mu.RLock()
	response := api.PeerStatusResponse{
		IsAlive:        true,
		IsActive:       va.isActive,
		ProcessRunning: va.isProcessRunning(),
		Timestamp:      time.Now().Unix(),
	}
	va.mu.RUnlock()

	log.Printf("Peer status request: IsActive=%v, ProcessRunning=%v", response.IsActive, response.ProcessRunning)

	va.sendJSON(w, response)
}

// handleFailover handles the /failover endpoint (from manager)
func (va *ValidatorAgent) handleFailover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cmd api.FailoverCommand
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	log.Printf("Failover command received: Action=%s, Reason=%s, SkipIdentity=%v", cmd.Action, cmd.Reason, cmd.SkipIdentity)

	response := api.FailoverResponse{
		DryRun:    va.config.DryRun,
		Timestamp: time.Now().Unix(),
	}

	var err error
	switch cmd.Action {
	case "become_active":
		err = va.becomeActive(cmd.Reason, cmd.SkipIdentity)
	case "become_passive":
		err = va.becomePassive(cmd.Reason)
	default:
		err = fmt.Errorf("unknown action: %s", cmd.Action)
	}

	if err != nil {
		response.Success = false
		response.Message = err.Error()
		log.Printf("Failover failed: %v", err)
	} else {
		response.Success = true
		response.Message = fmt.Sprintf("Successfully executed: %s", cmd.Action)
		log.Printf("Failover successful: %s", cmd.Action)
	}

	va.sendJSON(w, response)
}

// handleShutdown handles the /shutdown endpoint (from manager)
func (va *ValidatorAgent) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cmd api.ShutdownCommand
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	log.Printf("Shutdown command received: Reason=%s", cmd.Reason)

	response := api.ShutdownResponse{
		Success:   true,
		Message:   "Agent shutdown initiated",
		Timestamp: time.Now().Unix(),
	}

	va.sendJSON(w, response)

	// Shutdown the agent after sending response
	go func() {
		time.Sleep(100 * time.Millisecond) // Give time for response to be sent
		log.Printf("=== AGENT SHUTTING DOWN === Reason: %s", cmd.Reason)

		// Use systemd stop command if configured, otherwise exit directly
		if va.config.AgentStopCommand != "" {
			log.Printf("Executing stop command: %s", va.config.AgentStopCommand)
			output, err := va.executeCommand(va.config.AgentStopCommand, false)
			if err != nil {
				log.Printf("Stop command failed: %v, falling back to os.Exit", err)
				va.Stop()
				os.Exit(0)
			}
			log.Printf("Stop command output: %s", output)
			// systemd will handle the process termination
		} else {
			va.Stop()
			os.Exit(0)
		}
	}()
}

// handleIdentity returns the current validator identity pubkey
func (va *ValidatorAgent) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := api.IdentityResponse{
		Timestamp: time.Now().Unix(),
	}

	if va.config.IdentityCheckCommand == "" {
		response.Error = "identity_check_command not configured"
		va.sendJSON(w, response)
		return
	}

	// Replace {local_rpc} placeholder with actual value
	cmd := strings.ReplaceAll(va.config.IdentityCheckCommand, "{local_rpc}", va.config.LocalRPC)

	output, err := va.executeCommand(cmd, false)
	if err != nil {
		response.Error = fmt.Sprintf("failed to get identity: %v", err)
		va.sendJSON(w, response)
		return
	}

	response.IdentityPubkey = strings.TrimSpace(output)
	va.sendJSON(w, response)
}

// sendJSON sends a JSON response
func (va *ValidatorAgent) sendJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// managerWatchLoop monitors manager heartbeat and handles manager failure
func (va *ValidatorAgent) managerWatchLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	managerTimeout := va.config.ManagerTimeout.Duration()
	if managerTimeout == 0 {
		managerTimeout = 30 * time.Second
	}

	// Startup grace period: wait for manager to start before monitoring
	startupGracePeriod := 60 * time.Second

	for {
		select {
		case <-va.ctx.Done():
			return
		case <-ticker.C:
			va.mu.RLock()
			isActive := va.isActive
			lastPing := va.lastManagerPing
			hasSeenManager := va.hasSeenManager
			startTime := va.startTime
			lastSentTimestamp := va.lastSentTimestamp
			lastAckedTimestamp := va.lastAckedTimestamp
			va.mu.RUnlock()

			// Only active validator needs to worry about manager timeout
			if !isActive {
				continue
			}

			// Startup grace period: if we haven't seen manager yet and we're still in grace period, wait
			if !hasSeenManager {
				timeSinceStart := time.Since(startTime)
				if timeSinceStart < startupGracePeriod {
					log.Printf("Waiting for manager to start (grace period: %v remaining)",
						startupGracePeriod-timeSinceStart)
					continue
				}
				// Grace period expired, but we still haven't seen manager
				// This is OK - manager might start later, don't take action yet
				log.Printf("Startup grace period expired, but manager not seen yet. Continuing to wait...")
				continue
			}

			// Check for two types of manager communication failure:
			// 1. No pings received (manager down or inbound blocked)
			// 2. Pings received but Echo/Ack failing (outbound blocked - asymmetric failure)
			//
			// For case 2: if we've sent responses but manager keeps echoing an old timestamp,
			// it means our responses aren't reaching the manager. This is treated the same
			// as manager being unavailable, and we check peer status to decide what to do.

			noPingsReceived := time.Since(lastPing) > managerTimeout

			// Echo/Ack failure: we've sent at least one response, manager has acked at least once,
			// but the acked timestamp is older than what we sent (by more than managerTimeout)
			// This means manager has been receiving our pings but not getting responses for a while
			echoAckFailing := false
			if lastSentTimestamp > 0 && lastAckedTimestamp > 0 && lastSentTimestamp > lastAckedTimestamp {
				// Manager is echoing an old timestamp. Check if this has persisted for too long.
				// lastAckedTimestamp is a unix timestamp, so (lastSentTimestamp - lastAckedTimestamp)
				// gives us roughly how many seconds of responses haven't been acknowledged.
				ackLag := lastSentTimestamp - lastAckedTimestamp
				if ackLag > int64(managerTimeout.Seconds()) {
					echoAckFailing = true
				}
			}

			if noPingsReceived {
				log.Printf("WARNING: No manager heartbeat for %v (timeout: %v)",
					time.Since(lastPing), managerTimeout)
			} else if echoAckFailing {
				log.Printf("WARNING: Manager pings received but Echo/Ack failing (sent=%d, acked=%d, lag=%ds)",
					lastSentTimestamp, lastAckedTimestamp, lastSentTimestamp-lastAckedTimestamp)
				log.Printf("This indicates outbound connectivity failure - manager not receiving our responses")
			}

			if noPingsReceived || echoAckFailing {

				// Before doing anything, check if peer is alive
				ctx, cancel := context.WithTimeout(va.ctx, 5*time.Second)
				peerStatus, err := va.checkPeerStatus(ctx)
				cancel()

				if err != nil {
					// Can't reach manager AND can't reach peer
					// This indicates we (active) have a network problem
					// Safe action: become passive to avoid voting while isolated
					log.Printf("CRITICAL: Cannot reach manager AND cannot reach peer!")
					log.Printf("This indicates network isolation. Becoming passive for safety.")
					if err := va.becomePassive("Network isolation - cannot reach manager or peer"); err != nil {
						log.Printf("Failed to become passive: %v", err)
					}
					continue
				}

				log.Printf("Peer status: IsAlive=%v, IsActive=%v, ProcessRunning=%v",
					peerStatus.IsAlive, peerStatus.IsActive, peerStatus.ProcessRunning)

				// If peer is already active, we should become passive
				if peerStatus.IsActive {
					log.Printf("Peer is active and manager is down. Becoming passive to avoid split-brain.")
					if err := va.becomePassive("Peer is active, manager down"); err != nil {
						log.Printf("Failed to become passive: %v", err)
					}
				} else {
					log.Printf("Peer is passive. Staying active. Waiting for manager to come back.")
				}
			}
		}
	}
}

// Start starts the HTTP server and background loops
func (va *ValidatorAgent) Start() error {
	// Register HTTP handlers
	http.HandleFunc("/status", va.handleStatus)
	http.HandleFunc("/peer-status", va.handlePeerStatus)
	http.HandleFunc("/failover", va.handleFailover)
	http.HandleFunc("/shutdown", va.handleShutdown)
	http.HandleFunc("/identity", va.handleIdentity)

	// Start background loop for manager watch
	// Note: Tower backup is now done on each status request from manager
	go va.managerWatchLoop()

	log.Printf("=== Validator Agent Starting ===")
	log.Printf("Listen address: %s", va.config.ListenAddr)
	log.Printf("Local RPC: %s", va.config.LocalRPC)
	log.Printf("Process name: %s", va.config.ProcessName)
	log.Printf("Peer endpoint: %s", va.config.PeerEndpoint)
	log.Printf("Is active on start: %v", va.isActive)
	log.Printf("Dry-run mode: %v", va.config.DryRun)
	log.Printf("================================")

	return http.ListenAndServe(va.config.ListenAddr, nil)
}

// Stop stops the validator agent
func (va *ValidatorAgent) Stop() {
	va.cancel()
}

func main() {
	// Command line flags
	configFile := flag.String("config", "", "Path to config file")
	listenAddr := flag.String("listen", ":8080", "Address to listen on")
	localRPC := flag.String("rpc", "http://127.0.0.1:8899", "Local validator RPC endpoint")
	processName := flag.String("process", "agave-validator", "Process name to monitor")
	peerEndpoint := flag.String("peer", "", "Peer validator endpoint")
	isActive := flag.Bool("active", false, "Start as active validator")
	dryRun := flag.Bool("dry-run", true, "Dry-run mode (don't execute commands)")
	logFile := flag.String("log-file", "", "Path to log file (logs to both console and file)")
	generateConfig := flag.Bool("generate-config", false, "Generate example config file")

	flag.Parse()

	// Generate example config if requested
	if *generateConfig {
		cfg := config.DefaultValidatorConfig()
		if err := config.SaveConfig("validator-config.json", cfg); err != nil {
			log.Fatalf("Failed to generate config: %v", err)
		}
		log.Printf("Generated example config: validator-config.json")
		return
	}

	var cfg *config.ValidatorConfig
	var err error

	// Load config from file or use flags
	if *configFile != "" {
		cfg, err = config.LoadValidatorConfig(*configFile)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
	} else {
		cfg = &config.ValidatorConfig{
			ListenAddr:            *listenAddr,
			LocalRPC:              *localRPC,
			ProcessName:           *processName,
			PeerEndpoint:          *peerEndpoint,
			IsActiveOnStart:       *isActive,
			ManagerTimeout:        config.Duration(30 * time.Second),
			TowerBackupCommand:    "echo 'tower backup not configured'",
			TowerRestoreCommand:   "echo 'tower restore not configured'",
			IdentityChangeCommand: "echo 'identity change not configured'",
			IdentityRemoveCommand: "echo 'identity remove not configured'",
			DryRun:                *dryRun,
		}
	}

	// Environment variable overrides
	if env := os.Getenv("VALIDATOR_LISTEN"); env != "" {
		cfg.ListenAddr = env
	}
	if env := os.Getenv("VALIDATOR_RPC"); env != "" {
		cfg.LocalRPC = env
	}
	if env := os.Getenv("VALIDATOR_PEER"); env != "" {
		cfg.PeerEndpoint = env
	}
	if os.Getenv("VALIDATOR_DRY_RUN") == "false" {
		cfg.DryRun = false
	}

	// Setup logging (console + file if specified)
	logFilePath := cfg.LogFile
	if *logFile != "" {
		logFilePath = *logFile
	}
	if env := os.Getenv("VALIDATOR_LOG_FILE"); env != "" {
		logFilePath = env
	}
	logCloser, err := logging.SetupLogging(logFilePath)
	if err != nil {
		log.Fatalf("Failed to setup logging: %v", err)
	}
	if logCloser != nil {
		defer logCloser.Close()
	}

	agent := NewValidatorAgent(cfg)

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		agent.Stop()
		os.Exit(0)
	}()

	if err := agent.Start(); err != nil {
		log.Fatalf("Failed to start agent: %v", err)
	}
}
