package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
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
	"github.com/gagliardetto/solana-go/rpc"
)

// setupLogging configures logging to both console and file
func setupLogging(logFile string) (*os.File, error) {
	// Set log format with timestamp
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	if logFile == "" {
		// Console only
		log.SetOutput(os.Stdout)
		return nil, nil
	}

	// Open log file (append mode, create if not exists)
	f, err := os.OpenFile(logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	// Write to both console and file
	multiWriter := io.MultiWriter(os.Stdout, f)
	log.SetOutput(multiWriter)

	log.Printf("Logging to console and file: %s", logFile)
	return f, nil
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
	return &ValidatorAgent{
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
func (va *ValidatorAgent) becomeActive(reason string) error {
	va.mu.Lock()
	if va.isActive {
		va.mu.Unlock()
		log.Printf("Already active, ignoring become_active request")
		return nil
	}
	va.mu.Unlock()

	log.Printf("=== BECOMING ACTIVE === Reason: %s", reason)

	// Step 1: Restore tower file from etcd
	log.Printf("Step 1: Restoring tower file from etcd...")
	if err := va.restoreTower(); err != nil {
		return fmt.Errorf("failed to restore tower: %w", err)
	}

	// Step 2: Change identity
	log.Printf("Step 2: Changing validator identity...")
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

	// Step 2: Remove identity
	log.Printf("Step 2: Removing validator identity...")
	output, err := va.executeCommand(va.config.IdentityRemoveCommand, false)
	if err != nil {
		return fmt.Errorf("failed to remove identity: %w", err)
	}
	log.Printf("Identity remove output: %s", strings.TrimSpace(output))

	va.mu.Lock()
	va.isActive = false
	va.mu.Unlock()

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

	// Update last manager ping time
	va.mu.Lock()
	va.lastManagerPing = time.Now()
	va.hasSeenManager = true // Mark that we've seen manager at least once
	va.missedPings = 0
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

	// Backup tower file on each status check (only if active)
	// This happens every time manager pings us (every 2 seconds)
	if response.IsActive && response.ProcessRunning {
		if err := va.backupTower(); err != nil {
			log.Printf("Tower backup warning: %v", err)
		}
	}

	log.Printf("Status request: ProcessRunning=%v, Slot=%d, Active=%v, Healthy=%v",
		response.ProcessRunning, response.ValidatorSlot, response.IsActive, response.IsHealthy)

	va.sendJSON(w, response)
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

	log.Printf("Failover command received: Action=%s, Reason=%s", cmd.Action, cmd.Reason)

	response := api.FailoverResponse{
		DryRun:    va.config.DryRun,
		Timestamp: time.Now().Unix(),
	}

	var err error
	switch cmd.Action {
	case "become_active":
		err = va.becomeActive(cmd.Reason)
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

// handleHealth is a simple health check endpoint
func (va *ValidatorAgent) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
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
		va.Stop()
		os.Exit(0)
	}()
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

			// We've seen manager at least once, now monitor for timeout
			// Check if manager has been silent too long
			if time.Since(lastPing) > managerTimeout {
				log.Printf("WARNING: No manager heartbeat for %v (timeout: %v)",
					time.Since(lastPing), managerTimeout)

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
	http.HandleFunc("/health", va.handleHealth)
	http.HandleFunc("/shutdown", va.handleShutdown)

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
	logFileHandle, err := setupLogging(logFilePath)
	if logFileHandle != nil {
		defer logFileHandle.Close()
	}
	if err != nil {
		log.Fatalf("Failed to setup logging: %v", err)
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
