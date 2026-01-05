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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/failover/pkg/api"
	"github.com/failover/pkg/config"
	"github.com/failover/pkg/logging"
	"github.com/gagliardetto/solana-go/rpc"
)

// sendTelegramNotification sends a message to Telegram
func sendTelegramNotification(botToken, chatID, message string) error {
	if botToken == "" || chatID == "" {
		return nil // Telegram not configured, skip silently
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)

	payload := map[string]string{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "HTML",
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// ValidatorState tracks the state of a validator
type ValidatorState struct {
	Name                string
	Endpoint            string
	LastResponse        *api.ValidatorStatusResponse
	LastSuccess         time.Time
	ConsecutiveFails    int
	IsReachable         bool
	UnreachableNotified bool // true if we already sent unreachable notification
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

	// HTTP clients
	httpClient         *http.Client // For status checks (short timeout)
	failoverHttpClient *http.Client // For failover commands (longer timeout)

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
		failoverHttpClient: &http.Client{
			Timeout: 30 * time.Second, // Longer timeout for failover commands
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// notify sends a Telegram notification if configured
func (m *Manager) notify(message string) {
	if err := sendTelegramNotification(m.config.TelegramBotToken, m.config.TelegramChatID, message); err != nil {
		log.Printf("Failed to send Telegram notification: %v", err)
	}
}

// statusReportLoop sends a status report every 4 hours (at 00:00, 04:00, 08:00, 12:00, 16:00, 20:00)
func (m *Manager) statusReportLoop() {
	for {
		// Calculate time until next 4-hour mark
		now := time.Now()
		currentHour := now.Hour()
		nextReportHour := ((currentHour / 4) + 1) * 4 // Next multiple of 4

		var nextReport time.Time
		if nextReportHour >= 24 {
			// Next report is tomorrow at 00:00
			nextReport = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		} else {
			nextReport = time.Date(now.Year(), now.Month(), now.Day(), nextReportHour, 0, 0, 0, now.Location())
		}
		timeUntilReport := nextReport.Sub(now)

		log.Printf("Next status report scheduled in %v", timeUntilReport.Round(time.Minute))

		select {
		case <-m.ctx.Done():
			return
		case <-time.After(timeUntilReport):
			m.sendStatusReport()
		}
	}
}

// sendStatusReport sends a status report to Telegram
func (m *Manager) sendStatusReport() {
	m.mu.RLock()
	activeIdx := m.activeIdx
	validators := m.validators
	networkSlot := m.networkSlot
	m.mu.RUnlock()

	activeState := validators[activeIdx]
	passiveState := validators[1-activeIdx]

	// Build status message
	activeStatus := "🟢 Online"
	if !activeState.IsReachable {
		activeStatus = "🔴 Unreachable"
	}

	passiveStatus := "🟢 Online"
	if !passiveState.IsReachable {
		passiveStatus = "🔴 Unreachable"
	}

	var activeSlot, passiveSlot string
	if activeState.LastResponse != nil {
		activeSlot = fmt.Sprintf("%d", activeState.LastResponse.ValidatorSlot)
	} else {
		activeSlot = "N/A"
	}
	if passiveState.LastResponse != nil {
		passiveSlot = fmt.Sprintf("%d", passiveState.LastResponse.ValidatorSlot)
	} else {
		passiveSlot = "N/A"
	}

	message := fmt.Sprintf(`📊 <b>STATUS REPORT</b>

🕐 %s

<b>Active Validator:</b> %s
Status: %s
Endpoint: %s
Slot: %s

<b>Passive Validator:</b> %s
Status: %s
Endpoint: %s
Slot: %s

<b>Network Slot:</b> %d
<b>Manager:</b> 🟢 Running`,
		time.Now().Format("2006-01-02 15:04:05"),
		activeState.Name, activeStatus, activeState.Endpoint, activeSlot,
		passiveState.Name, passiveStatus, passiveState.Endpoint, passiveSlot,
		networkSlot)

	m.notify(message)
	log.Printf("Status report sent")
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

	resp, err := m.failoverHttpClient.Do(req)
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

	// Send notification
	m.notify(fmt.Sprintf("🔄 <b>FAILOVER COMPLETE</b>\n\nReason: %s\nFrom: %s (%s)\nTo: %s (%s)",
		reason, oldActive.Name, oldActive.Endpoint, newActive.Name, newActive.Endpoint))

	return nil
}

// checkAndFailover performs health check and failover if needed
func (m *Manager) checkAndFailover() {
	m.mu.RLock()
	activeIdx := m.activeIdx
	m.mu.RUnlock()

	activeState := m.validators[activeIdx]
	passiveState := m.validators[1-activeIdx]

	// Check active validator (with its own timeout)
	log.Printf("Checking [%s] (%s)...", activeState.Name, activeState.Endpoint)
	activeCtx, activeCancel := context.WithTimeout(m.ctx, m.config.RequestTimeout.Duration())
	activeStatus, err := m.checkValidator(activeCtx, activeState)
	activeCancel()

	if err != nil {
		activeState.ConsecutiveFails++
		wasReachable := activeState.IsReachable
		activeState.IsReachable = false
		log.Printf("[%s] UNREACHABLE (fail #%d): %v",
			activeState.Name, activeState.ConsecutiveFails, err)
		// Send notification only once when becoming unreachable
		if wasReachable && !activeState.UnreachableNotified {
			activeState.UnreachableNotified = true
			m.notify(fmt.Sprintf("🔴 <b>SERVER UNREACHABLE</b>\n\n%s (%s)",
				activeState.Name, activeState.Endpoint))
		}
	} else {
		// Send notification if was unreachable and now reachable
		if activeState.UnreachableNotified {
			activeState.UnreachableNotified = false
			m.notify(fmt.Sprintf("🟢 <b>SERVER BACK ONLINE</b>\n\n%s (%s)",
				activeState.Name, activeState.Endpoint))
		}
		activeState.ConsecutiveFails = 0
		activeState.IsReachable = true
		activeState.LastSuccess = time.Now()
		activeState.LastResponse = activeStatus
		m.printValidatorStatus(activeState, activeStatus)
	}

	// Check passive validator (with its own timeout)
	passiveCtx, passiveCancel := context.WithTimeout(m.ctx, m.config.RequestTimeout.Duration())
	passiveStatus, passiveErr := m.checkValidator(passiveCtx, passiveState)
	passiveCancel()
	if passiveErr != nil {
		wasReachable := passiveState.IsReachable
		passiveState.IsReachable = false
		log.Printf("[%s] unreachable: %v", passiveState.Name, passiveErr)
		// Send notification only once when becoming unreachable
		if wasReachable && !passiveState.UnreachableNotified {
			passiveState.UnreachableNotified = true
			m.notify(fmt.Sprintf("🔴 <b>SERVER UNREACHABLE</b>\n\n%s (%s)",
				passiveState.Name, passiveState.Endpoint))
		}
	} else {
		// Send notification if was unreachable and now reachable
		if passiveState.UnreachableNotified {
			passiveState.UnreachableNotified = false
			m.notify(fmt.Sprintf("🟢 <b>SERVER BACK ONLINE</b>\n\n%s (%s)",
				passiveState.Name, passiveState.Endpoint))
		}
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
			m.notify(fmt.Sprintf("🚨 <b>FAILOVER BLOCKED</b>\n\n<b>Active:</b> %s (%s)\nUnhealthy: %s\n\n<b>Passive:</b> %s (%s)\nUnreachable\n\nCannot failover!",
				activeState.Name, activeState.Endpoint, failoverReason,
				passiveState.Name, passiveState.Endpoint))
			return
		}

		// Check if passive is healthy before failover
		if passiveStatus == nil || !m.isValidatorHealthy(passiveStatus) {
			passiveReason := "unknown"
			if passiveStatus != nil {
				passiveReason = m.getUnhealthyReason(passiveState, passiveStatus)
			}
			log.Printf("CRITICAL: Passive validator is not healthy (%s). Cannot failover!", passiveReason)
			m.notify(fmt.Sprintf("🚨 <b>FAILOVER BLOCKED</b>\n\n<b>Active:</b> %s (%s)\nUnhealthy: %s\n\n<b>Passive:</b> %s (%s)\nNot healthy: %s\n\nCannot failover!",
				activeState.Name, activeState.Endpoint, failoverReason,
				passiveState.Name, passiveState.Endpoint, passiveReason))
			return
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

	// Send startup notification to Telegram
	m.notify(fmt.Sprintf(`🚀 <b>FAILOVER MANAGER STARTED</b>

🕐 %s

<b>Active:</b> %s (%s)
<b>Passive:</b> %s (%s)

<b>Settings:</b>
• Heartbeat: %v
• Misses before failover: %d
• Slot diff threshold: %d
• Dry-run: %v`,
		time.Now().Format("2006-01-02 15:04:05"),
		m.validators[0].Name, m.validators[0].Endpoint,
		m.validators[1].Name, m.validators[1].Endpoint,
		interval,
		m.config.MissesBeforeFailover,
		m.config.SlotDiffThreshold,
		m.config.DryRun))

	// Start network slot monitoring loop
	go m.networkSlotLoop()

	// Start status report loop (every 4 hours)
	go m.statusReportLoop()

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

// sendShutdownCommand sends a shutdown command to a validator agent
func sendShutdownCommand(endpoint string, timeout time.Duration) error {
	cmd := api.ShutdownCommand{
		Reason:    "Manager requested shutdown",
		Timestamp: time.Now().Unix(),
	}

	jsonBody, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("failed to marshal command: %w", err)
	}

	url := fmt.Sprintf("%s/shutdown", strings.TrimSuffix(endpoint, "/"))
	client := &http.Client{Timeout: timeout}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent returned status %d: %s", resp.StatusCode, string(body))
	}

	var shutdownResp api.ShutdownResponse
	if err := json.NewDecoder(resp.Body).Decode(&shutdownResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if !shutdownResp.Success {
		return fmt.Errorf("agent failed to shutdown: %s", shutdownResp.Message)
	}

	return nil
}

// shutdownAgents sends shutdown commands to all configured agents
func shutdownAgents(cfg *config.ManagerConfig) {
	timeout := cfg.RequestTimeout.Duration()
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	endpoints := []string{cfg.ActiveValidator, cfg.PassiveValidator}

	for _, endpoint := range endpoints {
		if endpoint == "" {
			continue
		}
		log.Printf("Sending shutdown command to agent at %s...", endpoint)
		if err := sendShutdownCommand(endpoint, timeout); err != nil {
			log.Printf("Failed to shutdown agent at %s: %v", endpoint, err)
		} else {
			log.Printf("Agent at %s acknowledged shutdown", endpoint)
		}
	}
}

// detectActiveFromGossip detects which validator is active by checking gossip
// Returns activeEndpoint, passiveEndpoint, error
func detectActiveFromGossip(cfg *config.ManagerConfig) (string, string, error) {
	if cfg.GossipCheckCommand == "" {
		return "", "", fmt.Errorf("gossip_check_command not configured")
	}
	if cfg.Validator1.Endpoint == "" || cfg.Validator1.IP == "" {
		return "", "", fmt.Errorf("validator1 endpoint and ip not configured")
	}
	if cfg.Validator2.Endpoint == "" || cfg.Validator2.IP == "" {
		return "", "", fmt.Errorf("validator2 endpoint and ip not configured")
	}

	cmd := exec.Command("bash", "-c", cfg.GossipCheckCommand)
	output, err := cmd.Output()
	if err != nil {
		// grep returns exit code 1 if no match found
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "", "", fmt.Errorf("validator identity not found in gossip")
		}
		return "", "", fmt.Errorf("gossip check command failed: %w", err)
	}

	// Parse gossip output: first column is IP
	// Example: "80.251.153.166  | DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY | ..."
	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", "", fmt.Errorf("empty gossip output")
	}

	// Split by | and get first field (IP)
	parts := strings.Split(line, "|")
	if len(parts) < 1 {
		return "", "", fmt.Errorf("unexpected gossip output format: %s", line)
	}

	gossipIP := strings.TrimSpace(parts[0])
	log.Printf("Gossip check: active validator IP from gossip: %s", gossipIP)

	// Determine which validator is active based on gossip IP
	if gossipIP == cfg.Validator1.IP {
		log.Printf("Validator1 (%s) is active, Validator2 (%s) is passive",
			cfg.Validator1.Endpoint, cfg.Validator2.Endpoint)
		return cfg.Validator1.Endpoint, cfg.Validator2.Endpoint, nil
	} else if gossipIP == cfg.Validator2.IP {
		log.Printf("Validator2 (%s) is active, Validator1 (%s) is passive",
			cfg.Validator2.Endpoint, cfg.Validator1.Endpoint)
		return cfg.Validator2.Endpoint, cfg.Validator1.Endpoint, nil
	}

	return "", "", fmt.Errorf("gossip IP %s does not match validator1 (%s) or validator2 (%s)",
		gossipIP, cfg.Validator1.IP, cfg.Validator2.IP)
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
	shutdownAgent := flag.Bool("shutdown-agent", false, "Send shutdown command to all agents and exit")

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
	} else if *shutdownAgent {
		// For shutdown-agent mode, we still need endpoints but can work with partial config
		cfg = &config.ManagerConfig{
			ActiveValidator:  *activeValidator,
			PassiveValidator: *passiveValidator,
			RequestTimeout:   config.Duration(*requestTimeout),
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

	// Auto-detect active/passive from gossip if not explicitly set
	if cfg.ActiveValidator == "" || cfg.PassiveValidator == "" {
		if cfg.GossipCheckCommand != "" && cfg.Validator1.Endpoint != "" && cfg.Validator2.Endpoint != "" {
			log.Println("Auto-detecting active/passive validators from gossip...")
			activeEndpoint, passiveEndpoint, err := detectActiveFromGossip(cfg)
			if err != nil {
				log.Fatalf("Failed to detect active/passive from gossip: %v", err)
			}
			cfg.ActiveValidator = activeEndpoint
			cfg.PassiveValidator = passiveEndpoint
		} else if cfg.ActiveValidator == "" || cfg.PassiveValidator == "" {
			log.Fatal("Both active and passive validator endpoints are required. Either set active_validator/passive_validator or configure validator1, validator2, and gossip_check_command for auto-detection.")
		}
	}

	// Handle shutdown-agent mode
	if *shutdownAgent {
		if cfg.ActiveValidator == "" && cfg.PassiveValidator == "" {
			log.Fatal("At least one validator endpoint (--active or --passive) is required for --shutdown-agent")
		}
		log.Println("=== Sending shutdown commands to agents ===")
		shutdownAgents(cfg)
		log.Println("=== Shutdown commands sent ===")
		return
	}

	// Setup logging (console + file if specified)
	logFilePath := cfg.LogFile
	if *logFile != "" {
		logFilePath = *logFile
	}
	if env := os.Getenv("MANAGER_LOG_FILE"); env != "" {
		logFilePath = env
	}
	logCloser, err := logging.SetupLogging(logFilePath)
	if err != nil {
		log.Fatalf("Failed to setup logging: %v", err)
	}
	if logCloser != nil {
		defer logCloser.Close()
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
