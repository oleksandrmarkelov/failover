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
	"os/signal"
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

// ValidatorState tracks the state of a validator
type ValidatorState struct {
	Name             string
	Endpoint         string
	LastResponse     *api.ValidatorStatusResponse
	LastSuccess      time.Time
	ConsecutiveFails int
	IsReachable      bool
}

// Manager manages failover between validators
type Manager struct {
	config *config.ManagerConfig

	// State
	mu         sync.RWMutex
	activeIdx  int // 0 = first validator (active), 1 = second validator (passive)
	validators [2]*ValidatorState

	// Network slot tracking (fetched from cluster RPC)
	networkSlot     uint64
	networkSlotTime time.Time

	// Solana RPC client for cluster slot checks
	clusterClient *rpc.Client

	// HTTP client
	httpClient *http.Client

	// Shutdown
	ctx    context.Context
	cancel context.CancelFunc
}

// NewManager creates a new failover manager
func NewManager(cfg *config.ManagerConfig) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		config: cfg,
		validators: [2]*ValidatorState{
			{Name: "primary", Endpoint: cfg.ActiveValidator, IsReachable: true},
			{Name: "secondary", Endpoint: cfg.PassiveValidator, IsReachable: true},
		},
		activeIdx:     0,
		clusterClient: rpc.New(cfg.ClusterRPC),
		httpClient: &http.Client{
			Timeout: cfg.RequestTimeout.Duration(),
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// fetchNetworkSlot fetches the current network slot from the cluster RPC
func (m *Manager) fetchNetworkSlot(ctx context.Context) (uint64, error) {
	slot, err := m.clusterClient.GetSlot(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return 0, fmt.Errorf("failed to get cluster slot: %w", err)
	}
	return slot, nil
}

// updateNetworkSlot updates the cached network slot
func (m *Manager) updateNetworkSlot() {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()

	slot, err := m.fetchNetworkSlot(ctx)
	if err != nil {
		log.Printf("Warning: Failed to fetch network slot from cluster RPC: %v", err)
		return
	}

	m.mu.Lock()
	m.networkSlot = slot
	m.networkSlotTime = time.Now()
	m.mu.Unlock()

	log.Printf("Network slot updated: %d", slot)
}

// getNetworkSlot returns the cached network slot
func (m *Manager) getNetworkSlot() (uint64, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.networkSlot, m.networkSlotTime
}

// networkSlotLoop periodically fetches the network slot from cluster RPC
func (m *Manager) networkSlotLoop() {
	interval := m.config.SlotCheckInterval.Duration()
	if interval == 0 {
		interval = 30 * time.Second
	}

	// Initial fetch
	m.updateNetworkSlot()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.updateNetworkSlot()
		}
	}
}

// checkValidator sends a status request to a validator
func (m *Manager) checkValidator(ctx context.Context, state *ValidatorState) (*api.ValidatorStatusResponse, error) {
	reqBody := api.ValidatorStatusRequest{
		Timestamp: time.Now().Unix(),
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/status", strings.TrimSuffix(state.Endpoint, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("validator returned status %d: %s", resp.StatusCode, string(body))
	}

	var statusResp api.ValidatorStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &statusResp, nil
}

// sendFailoverCommand sends a failover command to a validator
func (m *Manager) sendFailoverCommand(ctx context.Context, state *ValidatorState, action, reason string) (*api.FailoverResponse, error) {
	cmd := api.FailoverCommand{
		Action:    action,
		Reason:    reason,
		Timestamp: time.Now().Unix(),
	}

	jsonBody, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal command: %w", err)
	}

	url := fmt.Sprintf("%s/failover", strings.TrimSuffix(state.Endpoint, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send command: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("validator returned status %d: %s", resp.StatusCode, string(body))
	}

	var failoverResp api.FailoverResponse
	if err := json.NewDecoder(resp.Body).Decode(&failoverResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &failoverResp, nil
}

// calculateSlotDiff calculates the slot difference between network and validator
func (m *Manager) calculateSlotDiff(validatorSlot uint64) int64 {
	networkSlot, _ := m.getNetworkSlot()
	if networkSlot == 0 {
		return 0 // No network slot data yet
	}
	return int64(networkSlot) - int64(validatorSlot)
}

// isValidatorHealthy determines if a validator is healthy
func (m *Manager) isValidatorHealthy(status *api.ValidatorStatusResponse) bool {
	if status == nil {
		return false
	}

	// Must have process running
	if !status.ProcessRunning {
		return false
	}

	// Must not be recently restarted (crash loop detection)
	// ProcessHealthy = running AND uptime > 2 minutes
	if !status.ProcessHealthy {
		return false
	}

	// Must have healthy RPC
	if !status.IsHealthy {
		return false
	}

	// Calculate slot diff using network slot from cluster RPC
	// (not from validator, as disconnected validator would have stale data)
	slotDiff := m.calculateSlotDiff(status.ValidatorSlot)

	// Must not be too far behind
	if slotDiff > m.config.SlotDiffThreshold {
		return false
	}

	return true
}

// getUnhealthyReason returns why validator is unhealthy
func (m *Manager) getUnhealthyReason(state *ValidatorState, status *api.ValidatorStatusResponse) string {
	if status == nil {
		return "unreachable"
	}

	reasons := []string{}
	if !status.ProcessRunning {
		reasons = append(reasons, "process not running")
	}
	if status.ProcessRunning && !status.ProcessHealthy {
		reasons = append(reasons, fmt.Sprintf("process recently restarted (uptime: %ds, need 120s)", status.ProcessUptime))
	}
	if !status.IsHealthy {
		reasons = append(reasons, "RPC unhealthy")
	}

	// Use slot diff calculated by manager (from cluster RPC), not validator's view
	slotDiff := m.calculateSlotDiff(status.ValidatorSlot)
	if slotDiff > m.config.SlotDiffThreshold {
		reasons = append(reasons, fmt.Sprintf("behind by %d slots (threshold: %d)", slotDiff, m.config.SlotDiffThreshold))
	}
	if status.Error != "" {
		reasons = append(reasons, status.Error)
	}

	if len(reasons) == 0 {
		return "unknown"
	}
	return strings.Join(reasons, ", ")
}

// performFailover switches from active to passive
func (m *Manager) performFailover(reason string) error {
	m.mu.Lock()
	oldActiveIdx := m.activeIdx
	newActiveIdx := 1 - oldActiveIdx // Toggle between 0 and 1
	m.mu.Unlock()

	oldActive := m.validators[oldActiveIdx]
	newActive := m.validators[newActiveIdx]

	log.Printf("========================================")
	log.Printf("=== FAILOVER INITIATED ===")
	log.Printf("Reason: %s", reason)
	log.Printf("From: [%s] %s", oldActive.Name, oldActive.Endpoint)
	log.Printf("To:   [%s] %s", newActive.Name, newActive.Endpoint)
	log.Printf("========================================")

	if m.config.DryRun {
		log.Printf("[DRY-RUN] Would perform failover, but dry-run is enabled")
		return nil
	}

	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	// Step 1: Tell old active to become passive (if reachable)
	if oldActive.IsReachable {
		log.Printf("Step 1: Sending become_passive to [%s]...", oldActive.Name)
		resp, err := m.sendFailoverCommand(ctx, oldActive, "become_passive", reason)
		if err != nil {
			log.Printf("Warning: Failed to send become_passive to [%s]: %v", oldActive.Name, err)
		} else if !resp.Success {
			log.Printf("Warning: [%s] failed to become passive: %s", oldActive.Name, resp.Message)
		} else {
			log.Printf("[%s] is now passive", oldActive.Name)
		}
	} else {
		log.Printf("Step 1: [%s] is unreachable, skipping become_passive", oldActive.Name)
	}

	// Step 2: Tell new active to become active
	log.Printf("Step 2: Sending become_active to [%s]...", newActive.Name)
	resp, err := m.sendFailoverCommand(ctx, newActive, "become_active", reason)
	if err != nil {
		return fmt.Errorf("failed to send become_active to [%s]: %w", newActive.Name, err)
	}
	if !resp.Success {
		return fmt.Errorf("[%s] failed to become active: %s", newActive.Name, resp.Message)
	}

	log.Printf("[%s] is now active", newActive.Name)

	// Update state
	m.mu.Lock()
	m.activeIdx = newActiveIdx
	m.mu.Unlock()

	log.Printf("========================================")
	log.Printf("=== FAILOVER COMPLETE ===")
	log.Printf("New active: [%s] %s", newActive.Name, newActive.Endpoint)
	log.Printf("========================================")

	return nil
}

// checkAndFailover performs health check and failover if needed
func (m *Manager) checkAndFailover() {
	ctx, cancel := context.WithTimeout(m.ctx, m.config.RequestTimeout.Duration())
	defer cancel()

	m.mu.RLock()
	activeIdx := m.activeIdx
	m.mu.RUnlock()

	activeState := m.validators[activeIdx]
	passiveState := m.validators[1-activeIdx]

	// Check active validator
	log.Printf("Checking [%s] (%s)...", activeState.Name, activeState.Endpoint)
	activeStatus, err := m.checkValidator(ctx, activeState)

	if err != nil {
		activeState.ConsecutiveFails++
		activeState.IsReachable = false
		log.Printf("[%s] UNREACHABLE (fail #%d): %v",
			activeState.Name, activeState.ConsecutiveFails, err)
	} else {
		activeState.ConsecutiveFails = 0
		activeState.IsReachable = true
		activeState.LastSuccess = time.Now()
		activeState.LastResponse = activeStatus
		m.printValidatorStatus(activeState, activeStatus)
	}

	// Check passive validator (less frequently, just for status)
	passiveStatus, passiveErr := m.checkValidator(ctx, passiveState)
	if passiveErr != nil {
		passiveState.IsReachable = false
		log.Printf("[%s] unreachable: %v", passiveState.Name, passiveErr)
	} else {
		passiveState.IsReachable = true
		passiveState.LastSuccess = time.Now()
		passiveState.LastResponse = passiveStatus
		log.Printf("[%s] reachable, process=%v, healthy=%v",
			passiveState.Name, passiveStatus.ProcessRunning, passiveStatus.IsHealthy)
	}

	// Determine if failover is needed
	needsFailover := false
	failoverReason := ""

	if activeState.ConsecutiveFails >= m.config.MissesBeforeFailover {
		needsFailover = true
		failoverReason = fmt.Sprintf("active unreachable for %d consecutive checks",
			activeState.ConsecutiveFails)
	} else if activeStatus != nil && !m.isValidatorHealthy(activeStatus) {
		needsFailover = true
		failoverReason = m.getUnhealthyReason(activeState, activeStatus)
	}

	if needsFailover {
		log.Printf("Failover needed: %s", failoverReason)

		// Check if passive is available
		if !passiveState.IsReachable {
			log.Printf("CRITICAL: Passive validator is also unreachable. Cannot failover!")
			return
		}

		if passiveStatus != nil && !passiveStatus.ProcessRunning {
			log.Printf("WARNING: Passive validator process not running. Failover may fail!")
		}

		if err := m.performFailover(failoverReason); err != nil {
			log.Printf("CRITICAL: Failover failed: %v", err)
		}
	} else {
		log.Printf("[%s] is healthy", activeState.Name)
	}

	log.Println()
}

// printValidatorStatus prints validator status
func (m *Manager) printValidatorStatus(state *ValidatorState, status *api.ValidatorStatusResponse) {
	// Get network slot from manager's cluster RPC (not validator's view)
	networkSlot, networkSlotTime := m.getNetworkSlot()
	slotDiff := m.calculateSlotDiff(status.ValidatorSlot)

	log.Printf("[%s] Status:", state.Name)
	log.Printf("  Process Running: %v (uptime: %ds, healthy: %v)",
		status.ProcessRunning, status.ProcessUptime, status.ProcessHealthy)
	log.Printf("  Validator Slot:  %d", status.ValidatorSlot)
	log.Printf("  Network Slot:    %d (from cluster RPC, age: %v)", networkSlot, time.Since(networkSlotTime).Round(time.Second))
	log.Printf("  Slot Difference: %d (threshold: %d)", slotDiff, m.config.SlotDiffThreshold)
	log.Printf("  RPC Healthy:     %v", status.IsHealthy)
	log.Printf("  Is Active:       %v", status.IsActive)
	if status.Error != "" {
		log.Printf("  Error: %s", status.Error)
	}
}

// Monitor starts the monitoring loop
func (m *Manager) Monitor() error {
	interval := m.config.HeartbeatInterval.Duration()
	if interval == 0 {
		interval = 2 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("========================================")
	log.Printf("=== Failover Manager Starting ===")
	log.Printf("Active validator:  %s", m.validators[0].Endpoint)
	log.Printf("Passive validator: %s", m.validators[1].Endpoint)
	log.Printf("Heartbeat interval: %v", interval)
	log.Printf("Misses before failover: %d", m.config.MissesBeforeFailover)
	log.Printf("Slot diff threshold: %d", m.config.SlotDiffThreshold)
	log.Printf("Dry-run mode: %v", m.config.DryRun)
	log.Printf("========================================")
	log.Println()

	// Initial check
	m.checkAndFailover()

	for {
		select {
		case <-m.ctx.Done():
			log.Println("Manager shutting down...")
			return m.ctx.Err()
		case <-ticker.C:
			m.checkAndFailover()
		}
	}
}

// Stop stops the manager
func (m *Manager) Stop() {
	m.cancel()
}

// GetActiveValidator returns the currently active validator endpoint
func (m *Manager) GetActiveValidator() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.validators[m.activeIdx].Endpoint
}

func main() {
	// Command line flags
	configFile := flag.String("config", "", "Path to config file")
	activeValidator := flag.String("active", "", "Active validator endpoint (e.g., http://192.168.1.10:8080)")
	passiveValidator := flag.String("passive", "", "Passive validator endpoint (e.g., http://192.168.1.11:8080)")
	heartbeatInterval := flag.Duration("interval", 5*time.Second, "Heartbeat interval")
	missesBeforeFailover := flag.Int("misses", 5, "Consecutive misses before failover")
	slotThreshold := flag.Int64("slot-threshold", 100, "Maximum allowed slot difference")
	requestTimeout := flag.Duration("timeout", 5*time.Second, "Request timeout")
	dryRun := flag.Bool("dry-run", true, "Dry-run mode (don't trigger failover)")
	logFile := flag.String("log-file", "", "Path to log file (logs to both console and file)")
	generateConfig := flag.Bool("generate-config", false, "Generate example config file")

	flag.Parse()

	// Generate example config if requested
	if *generateConfig {
		cfg := config.DefaultManagerConfig()
		if err := config.SaveConfig("manager-config.json", cfg); err != nil {
			log.Fatalf("Failed to generate config: %v", err)
		}
		log.Printf("Generated example config: manager-config.json")
		return
	}

	var cfg *config.ManagerConfig
	var err error

	// Load config from file or use flags
	if *configFile != "" {
		cfg, err = config.LoadManagerConfig(*configFile)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
	} else {
		if *activeValidator == "" || *passiveValidator == "" {
			log.Fatal("Both --active and --passive validator endpoints are required")
		}

		cfg = &config.ManagerConfig{
			ActiveValidator:      *activeValidator,
			PassiveValidator:     *passiveValidator,
			HeartbeatInterval:    config.Duration(*heartbeatInterval),
			MissesBeforeFailover: *missesBeforeFailover,
			SlotDiffThreshold:    *slotThreshold,
			RequestTimeout:       config.Duration(*requestTimeout),
			DryRun:               *dryRun,
		}
	}

	// Environment variable overrides
	if env := os.Getenv("ACTIVE_VALIDATOR"); env != "" {
		cfg.ActiveValidator = env
	}
	if env := os.Getenv("PASSIVE_VALIDATOR"); env != "" {
		cfg.PassiveValidator = env
	}
	if os.Getenv("DRY_RUN") == "false" {
		cfg.DryRun = false
	}

	// Setup logging (console + file if specified)
	logFilePath := cfg.LogFile
	if *logFile != "" {
		logFilePath = *logFile
	}
	if env := os.Getenv("MANAGER_LOG_FILE"); env != "" {
		logFilePath = env
	}
	logFileHandle, err := setupLogging(logFilePath)
	if logFileHandle != nil {
		defer logFileHandle.Close()
	}
	if err != nil {
		log.Fatalf("Failed to setup logging: %v", err)
	}

	manager := NewManager(cfg)

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		manager.Stop()
	}()

	if err := manager.Monitor(); err != nil && err != context.Canceled {
		log.Fatalf("Manager error: %v", err)
	}
}
