package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ValidatorEndpoint holds endpoint and IP for a validator
type ValidatorEndpoint struct {
	// Endpoint is the agent HTTP endpoint (e.g., "http://192.168.1.10:8080")
	Endpoint string `json:"endpoint"`

	// IP is the public IP of this validator for gossip matching (e.g., "80.251.153.166")
	IP string `json:"ip"`

	// LedgerPath is the path to the validator's ledger directory (required for secure identity mode)
	// Example: "/home/solana/ledger"
	LedgerPath string `json:"ledger_path"`
}

// ManagerConfig is the configuration for the manager program
type ManagerConfig struct {
	// ActiveValidator endpoint (e.g., "http://192.168.1.10:8080")
	// Can be auto-detected from gossip if Validator1, Validator2, and GossipCheckCommand are set
	ActiveValidator string `json:"active_validator"`

	// PassiveValidator endpoint (e.g., "http://192.168.1.11:8080")
	// Can be auto-detected from gossip if Validator1, Validator2, and GossipCheckCommand are set
	PassiveValidator string `json:"passive_validator"`

	// Validator1 endpoint and IP for auto-detection (e.g., {"endpoint": "http://192.168.1.10:8080", "ip": "80.251.153.166"})
	Validator1 ValidatorEndpoint `json:"validator1"`

	// Validator2 endpoint and IP for auto-detection (e.g., {"endpoint": "http://192.168.1.11:8080", "ip": "80.251.153.167"})
	Validator2 ValidatorEndpoint `json:"validator2"`

	// GossipCheckCommand command to check gossip for validator identity
	// Should output the gossip line for the validator identity
	// Example: "solana -ut gossip | grep DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY"
	GossipCheckCommand string `json:"gossip_check_command"`

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

	// StartupGracePeriod is the duration after manager startup during which no failover
	// will be triggered. This allows time to verify configuration is correct.
	// Default: 2 minutes
	StartupGracePeriod Duration `json:"startup_grace_period"`

	// LogFile path to log file (empty for stdout)
	LogFile string `json:"log_file"`

	// TelegramBotToken is the Telegram bot token for notifications
	// Get it from @BotFather
	TelegramBotToken string `json:"telegram_bot_token"`

	// TelegramChatID is the chat ID to send notifications to
	// Can be a user ID, group ID, or channel ID (e.g., "-1001234567890")
	TelegramChatID string `json:"telegram_chat_id"`

	// SecureIdentityMode if true, manager holds the identity keypair and sends it via SSH
	// when activating a validator. The keypair never resides on the validator server.
	// When enabled, agent's identity_change_command and active_identity_symlink_command are ignored.
	SecureIdentityMode bool `json:"secure_identity_mode"`

	// IdentityKeypairPath path to the staked identity keypair JSON file on the manager machine
	// Required when SecureIdentityMode is true
	// Example: "/home/solana/identity.json"
	IdentityKeypairPath string `json:"identity_keypair_path"`

	// SSHUser username for SSH connections to validator servers
	// Required when SecureIdentityMode is true
	// Example: "solana"
	SSHUser string `json:"ssh_user"`

	// SSHKeyPath path to the SSH private key for passwordless authentication
	// Required when SecureIdentityMode is true
	// Example: "~/.ssh/failover_key"
	SSHKeyPath string `json:"ssh_key_path"`

	// SSHSetIdentityCommand command template to set validator identity via SSH
	// Use {ledger} as placeholder for ledger path
	// The identity keypair file will be redirected to stdin via shell redirection
	// Example: "agave-validator --ledger {ledger} set-identity"
	SSHSetIdentityCommand string `json:"ssh_set_identity_command"`

	// SSHAuthorizedVoterCommand command template to add authorized voter via SSH
	// Use {ledger} as placeholder for ledger path
	// The identity keypair file will be redirected to stdin via shell redirection
	// Example: "agave-validator --ledger {ledger} authorized-voter add"
	SSHAuthorizedVoterCommand string `json:"ssh_authorized_voter_command"`

	// StakedIdentityPubkey is the expected public key of the staked identity
	// Used on startup to verify which validator is actually active (voting)
	// by comparing with the identity reported by each agent
	// Example: "DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY"
	StakedIdentityPubkey string `json:"staked_identity_pubkey"`

	// VoteAccountPubkey is the public key of the vote account associated with this validator
	// Used to verify if the validator is still actively voting on the network before failover
	// When manager loses connection to active validator, it checks if the vote account is still
	// voting (recent lastVote slot) via cluster RPC before triggering failover
	// This prevents false failovers when only manager-to-validator connection is broken
	// Example: "8SQEcP4FaYQySktNQeyxF3w8pvArx3oMEh7fPrzkN9pu"
	VoteAccountPubkey string `json:"vote_account_pubkey"`

	// StaleVoteSlotThreshold is the maximum number of slots behind network slot
	// that the vote account's lastVote can be before considering the validator as not voting
	// If vote account's lastVote is within this threshold of network slot, failover is blocked
	// Default: 150 slots (~1 minute at 400ms/slot)
	StaleVoteSlotThreshold int64 `json:"stale_vote_slot_threshold"`
}

// ValidatorConfig is the configuration for the validator program
type ValidatorConfig struct {
	// ListenAddr address to listen on (e.g., ":8080")
	ListenAddr string `json:"listen_addr"`

	// AllowedIPs list of IP addresses allowed to access the agent API
	// If empty, all IPs are allowed (no restriction)
	// Example: ["192.168.1.100", "10.0.0.50"]
	AllowedIPs []string `json:"allowed_ips"`

	// LocalRPC local validator RPC endpoint (e.g., "http://127.0.0.1:8899")
	LocalRPC string `json:"local_rpc"`

	// ProcessName name of the validator process to monitor
	ProcessName string `json:"process_name"`

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

	// PassiveIdentitySymlinkCommand command to update identity symlink when becoming passive
	// Example: "ln -sf /home/solana/solana_testnet/unstaked-identity.json /home/solana/solana_testnet/identity.json"
	PassiveIdentitySymlinkCommand string `json:"passive_identity_symlink_command"`

	// ActiveIdentitySymlinkCommand command to update identity symlink when becoming active
	// Example: "ln -sf /home/solana/solana_testnet/testnet-validator-keypair.json /home/solana/solana_testnet/identity.json"
	ActiveIdentitySymlinkCommand string `json:"active_identity_symlink_command"`

	// TowerFilePath path to tower file to remove when becoming passive
	// Use {validator_identity} as placeholder for the validator identity
	// Example: "/home/solana/solana_testnet/validator-ledger/tower-1_9-{validator_identity}.bin"
	TowerFilePath string `json:"tower_file_path"`

	// ValidatorIdentity is the public key of the validator
	// Used to substitute {validator_identity} in gossip_check_command and tower_file_path
	// Example: "DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY"
	ValidatorIdentity string `json:"validator_identity"`

	// GossipCheckCommand command to check gossip for validator identity
	// Use {validator_identity} as placeholder for the validator identity
	// Example: "solana -ut gossip | grep {validator_identity}"
	GossipCheckCommand string `json:"gossip_check_command"`

	// LocalIP is the IP address of this server to compare with gossip output
	// Optional: if not set, will be auto-detected
	// Example: "80.251.153.166"
	LocalIP string `json:"local_ip"`

	// ValidatorRestartCommand command to restart the validator service
	// Used when identity switch fails during passive startup
	// Example: "sudo systemctl restart solana"
	ValidatorRestartCommand string `json:"validator_restart_command"`

	// AgentStopCommand command to stop the agent service
	// Used when manager sends shutdown command
	// Example: "sudo systemctl stop failover-agent"
	AgentStopCommand string `json:"agent_stop_command"`

	// IdentityCheckCommand command to get current validator identity pubkey
	// Used by manager on startup to verify actual identity state
	// Use {local_rpc} as placeholder for local RPC endpoint
	// Example: "curl -s {local_rpc} -X POST -H 'Content-Type: application/json' -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getIdentity\"}' | jq -r '.result.identity'"
	IdentityCheckCommand string `json:"identity_check_command"`

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
	if config.StartupGracePeriod == 0 {
		config.StartupGracePeriod = Duration(2 * time.Minute)
	}
	if config.StaleVoteSlotThreshold == 0 {
		config.StaleVoteSlotThreshold = 150 // ~1 minute at 400ms/slot
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
		IsActiveOnStart:       false,
		ManagerTimeout:        Duration(30 * time.Second),
		TowerBackupCommand:    "echo 'tower backup command not configured'",
		TowerRestoreCommand:   "echo 'tower restore command not configured'",
		IdentityChangeCommand: "echo 'identity change command not configured'",
		IdentityRemoveCommand: "echo 'identity remove command not configured'",
		TowerFilePath:         "",
		ValidatorIdentity:     "",
		GossipCheckCommand:    "",
		LocalIP:               "",
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
