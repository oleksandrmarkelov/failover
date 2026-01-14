package api

import "time"

// ValidatorStatusRequest is sent by manager to validator
type ValidatorStatusRequest struct {
	// Timestamp when request was sent
	Timestamp int64 `json:"timestamp"`

	// LastReceivedTimestamp is the timestamp from the last response the manager
	// successfully received from this validator. This allows the agent to detect
	// asymmetric network failures where it can receive pings but its responses
	// are not reaching the manager.
	// If 0, the manager has not yet received any response from this validator.
	LastReceivedTimestamp int64 `json:"last_received_timestamp,omitempty"`
}

// ValidatorStatusResponse is returned by validator to manager
type ValidatorStatusResponse struct {
	// ProcessRunning indicates if agave-validator process is running
	ProcessRunning bool `json:"process_running"`

	// ProcessUptime is how long the process has been running in seconds
	// Used to detect crash loops (systemd restarts)
	ProcessUptime int64 `json:"process_uptime"`

	// ProcessHealthy indicates if process is running AND not recently restarted
	// A freshly restarted process is not considered healthy (might be in crash loop)
	ProcessHealthy bool `json:"process_healthy"`

	// ValidatorSlot is the current slot the validator is processing
	ValidatorSlot uint64 `json:"validator_slot"`

	// NetworkSlot is the current network slot (calculated by manager from cluster RPC)
	NetworkSlot uint64 `json:"network_slot"`

	// SlotDiff is the difference: NetworkSlot - ValidatorSlot (calculated by manager)
	SlotDiff int64 `json:"slot_diff"`

	// IsHealthy indicates if the validator RPC is healthy
	IsHealthy bool `json:"is_healthy"`

	// IsActive indicates if this validator is currently the active one
	IsActive bool `json:"is_active"`

	// LastTowerBackup timestamp of last tower file backup
	LastTowerBackup int64 `json:"last_tower_backup"`

	// Error contains any error message
	Error string `json:"error,omitempty"`

	// Timestamp when this status was generated
	Timestamp int64 `json:"timestamp"`
}

// FailoverCommand is sent by manager to trigger failover
type FailoverCommand struct {
	// Action: "become_active" or "become_passive"
	Action string `json:"action"`

	// Reason for the failover
	Reason string `json:"reason"`

	// SkipIdentity if true, agent should skip identity symlink and identity change commands
	// Used in secure identity mode where manager sets identity via SSH
	// Only applies to "become_active" action
	SkipIdentity bool `json:"skip_identity,omitempty"`

	// Timestamp when command was sent
	Timestamp int64 `json:"timestamp"`
}

// FailoverResponse is returned after failover command
type FailoverResponse struct {
	// Success indicates if the failover was successful
	Success bool `json:"success"`

	// Message contains details about the failover
	Message string `json:"message"`

	// DryRun indicates if this was a dry run
	DryRun bool `json:"dry_run"`

	// Timestamp when response was generated
	Timestamp int64 `json:"timestamp"`
}

// PeerStatusRequest is sent between validators to check peer status
type PeerStatusRequest struct {
	// FromValidator identifies the sender
	FromValidator string `json:"from_validator"`

	// Timestamp when request was sent
	Timestamp int64 `json:"timestamp"`
}

// PeerStatusResponse is returned to peer validator
type PeerStatusResponse struct {
	// IsAlive indicates the peer is alive
	IsAlive bool `json:"is_alive"`

	// IsActive indicates if peer is the active validator
	IsActive bool `json:"is_active"`

	// ProcessRunning indicates if agave-validator is running on peer
	ProcessRunning bool `json:"process_running"`

	// Timestamp when response was generated
	Timestamp int64 `json:"timestamp"`
}

// ShutdownCommand is sent by manager to shut down an agent
type ShutdownCommand struct {
	// Reason for the shutdown
	Reason string `json:"reason"`

	// Timestamp when command was sent
	Timestamp int64 `json:"timestamp"`
}

// ShutdownResponse is returned after shutdown command
type ShutdownResponse struct {
	// Success indicates if the shutdown was acknowledged
	Success bool `json:"success"`

	// Message contains details about the shutdown
	Message string `json:"message"`

	// Timestamp when response was generated
	Timestamp int64 `json:"timestamp"`
}

// IdentityResponse is returned by agent to report current validator identity
type IdentityResponse struct {
	// IdentityPubkey is the current validator identity pubkey
	IdentityPubkey string `json:"identity_pubkey"`

	// Error contains any error message
	Error string `json:"error,omitempty"`

	// Timestamp when response was generated
	Timestamp int64 `json:"timestamp"`
}

// HealthCheckInterval constants
const (
	DefaultHeartbeatInterval    = 5 * time.Second
	DefaultMissesBeforeFailover = 5
	DefaultSlotCheckInterval    = 30 * time.Second
	DefaultManagerTimeout       = 30 * time.Second
)
