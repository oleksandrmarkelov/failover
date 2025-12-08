package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ManagerConfig is the configuration for the manager program
type ManagerConfig struct {
	// ActiveValidator endpoint (e.g., "http://192.168.1.10:8080")
	ActiveValidator string `json:"active_validator"`

	// PassiveValidator endpoint (e.g., "http://192.168.1.11:8080")
	PassiveValidator string `json:"passive_validator"`

	// ClusterRPC endpoint to check network slot (e.g., "https://api.mainnet-beta.solana.com")
	// This is used to detect if validators are behind the network
	ClusterRPC string `json:"cluster_rpc"`

	// SlotCheckInterval how often to check cluster slot (less frequent than heartbeat to save RPC costs)
	SlotCheckInterval Duration `json:"slot_check_interval"`

	// HeartbeatInterval how often to check validators
	HeartbeatInterval Duration `json:"heartbeat_interval"`

	// MissesBeforeFailover how many consecutive misses before failover
	MissesBeforeFailover int `json:"misses_before_failover"`

	// SlotDiffThreshold maximum allowed slot difference
	SlotDiffThreshold int64 `json:"slot_diff_threshold"`

	// RequestTimeout timeout for HTTP requests
	RequestTimeout Duration `json:"request_timeout"`

	// DryRun if true, don't actually trigger failover
	DryRun bool `json:"dry_run"`

	// LogFile path to log file (empty for stdout)
	LogFile string `json:"log_file"`
}

// ValidatorConfig is the configuration for the validator program
type ValidatorConfig struct {
	// ListenAddr address to listen on (e.g., ":8080")
	ListenAddr string `json:"listen_addr"`

	// LocalRPC local validator RPC endpoint (e.g., "http://127.0.0.1:8899")
	LocalRPC string `json:"local_rpc"`

	// ProcessName name of the validator process to monitor
	ProcessName string `json:"process_name"`

	// PeerEndpoint the other validator's endpoint for peer check
	PeerEndpoint string `json:"peer_endpoint"`

	// IsActiveOnStart whether this validator starts as active
	IsActiveOnStart bool `json:"is_active_on_start"`

	// ManagerTimeout how long without manager heartbeat before peer check
	ManagerTimeout Duration `json:"manager_timeout"`

	// TowerBackupCommand command to backup tower file to etcd
	// Note: Backup happens on each status request from manager (every ~2s)
	// Example: "etcdctl put /solana/tower/$(hostname) @/path/to/tower"
	TowerBackupCommand string `json:"tower_backup_command"`

	// TowerRestoreCommand command to restore tower file from etcd
	// Example: "etcdctl get /solana/tower/primary --print-value-only > /path/to/tower"
	TowerRestoreCommand string `json:"tower_restore_command"`

	// IdentityChangeCommand command to change validator identity (become active)
	// Example: "solana-validator set-identity /path/to/identity.json"
	IdentityChangeCommand string `json:"identity_change_command"`

	// IdentityRemoveCommand command to remove validator identity (become passive)
	// Example: "solana-validator set-identity --require-tower /path/to/unstaked-identity.json"
	IdentityRemoveCommand string `json:"identity_remove_command"`

	// DryRun if true, don't execute commands, just log them
	DryRun bool `json:"dry_run"`

	// LogFile path to log file (empty for stdout)
	LogFile string `json:"log_file"`
}

// Duration is a wrapper for time.Duration that supports JSON marshaling
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case float64:
		*d = Duration(time.Duration(value))
		return nil
	case string:
		tmp, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		*d = Duration(tmp)
		return nil
	default:
		return fmt.Errorf("invalid duration")
	}
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// LoadManagerConfig loads manager configuration from a JSON file
func LoadManagerConfig(path string) (*ManagerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config ManagerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	if config.ClusterRPC == "" {
		config.ClusterRPC = "https://api.mainnet-beta.solana.com"
	}
	if config.SlotCheckInterval == 0 {
		config.SlotCheckInterval = Duration(30 * time.Second)
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = Duration(2 * time.Second)
	}
	if config.MissesBeforeFailover == 0 {
		config.MissesBeforeFailover = 5
	}
	if config.SlotDiffThreshold == 0 {
		config.SlotDiffThreshold = 100
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = Duration(5 * time.Second)
	}

	return &config, nil
}

// LoadValidatorConfig loads validator configuration from a JSON file
func LoadValidatorConfig(path string) (*ValidatorConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config ValidatorConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	if config.ListenAddr == "" {
		config.ListenAddr = ":8080"
	}
	if config.LocalRPC == "" {
		config.LocalRPC = "http://127.0.0.1:8899"
	}
	if config.ProcessName == "" {
		config.ProcessName = "agave-validator"
	}
	if config.ManagerTimeout == 0 {
		config.ManagerTimeout = Duration(30 * time.Second)
	}

	return &config, nil
}

// DefaultManagerConfig returns a default manager configuration
func DefaultManagerConfig() *ManagerConfig {
	return &ManagerConfig{
		ActiveValidator:      "http://192.168.1.10:8080",
		PassiveValidator:     "http://192.168.1.11:8080",
		ClusterRPC:           "https://api.mainnet-beta.solana.com",
		SlotCheckInterval:    Duration(30 * time.Second),
		HeartbeatInterval:    Duration(5 * time.Second),
		MissesBeforeFailover: 5,
		SlotDiffThreshold:    100,
		RequestTimeout:       Duration(5 * time.Second),
		DryRun:               false,
	}
}

// DefaultValidatorConfig returns a default validator configuration
func DefaultValidatorConfig() *ValidatorConfig {
	return &ValidatorConfig{
		ListenAddr:            ":8080",
		LocalRPC:              "http://127.0.0.1:8899",
		ProcessName:           "agave-validator",
		PeerEndpoint:          "http://192.168.1.11:8080",
		IsActiveOnStart:       false,
		ManagerTimeout:        Duration(30 * time.Second),
		TowerBackupCommand:    "echo 'tower backup command not configured'",
		TowerRestoreCommand:   "echo 'tower restore command not configured'",
		IdentityChangeCommand: "echo 'identity change command not configured'",
		IdentityRemoveCommand: "echo 'identity remove command not configured'",
		DryRun:                true,
	}
}

// SaveConfig saves a configuration to a JSON file
func SaveConfig(path string, config interface{}) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}
