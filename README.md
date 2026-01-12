# Solana Validator Failover System

A failover system for Solana validators written in Go. Consists of two programs:
- **Manager** - runs on manager server, monitors validators and triggers failover
- **Validator Agent** - runs on each validator server, responds to health checks and executes failover commands

## Prerequisites

Before setting up the failover system, ensure the following requirements are met on each validator server:

### 1. etcd Installation

etcd must be installed and running as a service on at least 3 servers for tower file synchronization (manager and both validators).

https://etcd.io/docs/v2.3/clustering/

### 2. Validator Snapshots

Snapshots must be enabled on your Solana validator to allow faster restarts and state recovery.


### 3. Autostart Configuration

All services (Solana validator, failover agent/manager, etcd) should be configured to start automatically after a system restart.

### 4. Sudoers Configuration

The failover agent needs to execute certain commands with sudo without password prompts:

```bash
sudo visudo
```

Add the following line (replace `solana` with your user):
```
solana ALL=(ALL) NOPASSWD: /usr/bin/systemctl stop failover-agent, /usr/bin/systemctl restart solana
```

## Architecture

```
                                                             
                         MANAGER SERVER                          
                                                             
      +-----------------------------------------------------+   
      |                   Manager Program                   |   
      |  - Pings both validators every 5 seconds            |   
      |  - Auto-detects active/passive from gossip          |
      |  - Checks process running + slot difference         |   
      |  - Triggers failover if active is unhealthy         |   
      +------------------------+----------------------------+   
                               |                 
                               |                 
                          HTTP | HTTP
                               |                 
        +----------------------+---------------------+
        |                                            |
        v                                            v
  +--------------+                           +--------------+
  | VALIDATOR 1  |                           | VALIDATOR 2  |
  |              |<------------------------->|              |
  |  Agent       |       peer heartbeat      |  Agent       |
  |  agave-valid |                           |  agave-valid |
  |  (active)    |                           |  (passive)   |
  +--------------+                           +--------------+
```

## Features

### Manager Program
- Auto-detects active/passive validators from gossip
- Pings two validator agents regularly
- Switches to passive if active doesn't respond (after N misses)
- Checks if validator process is running
- Checks slot difference (validator behind network)
- Telegram notifications for critical events
- Dry-run mode (test without actual failover)
- Remote agent shutdown command (`--shutdown-agent`)

### Validator Agent Program
- Auto-detects active state from gossip on startup
- Responds to manager health checks
- Reports process status and slot information
- Backs up tower file to etcd on each manager ping
- Monitors manager heartbeat - if manager goes offline, checks peer
- Executes identity change commands on failover
- Removes tower file when becoming passive (prevents stale tower)
- Remote shutdown endpoint
- Dry-run mode (logs commands without executing)

## Building

```bash
# Build both programs
go build -o failover-manager ./cmd/manager
go build -o failover-agent ./cmd/validator
```

## Quick Start

### 1. Starting order

First start passive validator agent,
then active validator agent and afterward manager.

### 2. Configure Agents (on each validator server)

Create `validator-config.json` on each server (replace IDENTITY, IP, PEER_IP (ip of second agent), LEDGER_PATH, SOLANA_PATH and IDENTITY_PATH):
```json
{
  "listen_addr": ":8080",
  "local_rpc": "http://127.0.0.1:8899",
  "process_name": "agave-validator",
  "peer_endpoint": "http://PEPEER_IP:8080",
  "is_active_on_start": false,
  "manager_timeout": "30s",
  "tower_backup_command": "etcdctl put /solana/tower/active \"$(base64 -w0 LEDGER_PATH/tower-1_9-*.bin)\"",
  "tower_restore_command": "etcdctl get /solana/tower/active --print-value-only | base64 -d > LEDGER_PATH/tower-1_9-IDENTITY.bin",
  "identity_change_command": "SOLANA_PATH/agave-validator  -l LEDGER_PATH set-identity IDENTITY_PATH/testnet-validator-keypair.json",
  "identity_remove_command": "SOLANA_PATH/agave-validator  -l LEDGER_PATH set-identity IDENTITY_PATH/unstaked-identity.json",
  "dry_run": false,
  "tower_file_path": "LEDGER_PATH/tower-1_9-{validator_identity}.bin",
  "validator_identity": "IDENTITY",
  "gossip_check_command": "solana -ut gossip | grep {validator_identity}",
  "local_ip": "IP",
  "log_file": "/home/solana/failover/agent.log",
  "validator_restart_command": "sudo systemctl restart solana",
  "agent_stop_command": "sudo systemctl stop failover-agent",
  "active_identity_symlink_command": "ln -sf IDENTITY_PATH/testnet-validator-keypair.json IDENTITY_PATH/identity.json",
  "passive_identity_symlink_command": "ln -sf IDENTITY_PATH/unstaked-identity.json IDENTITY_PATH/identity.json"
}
```
Fields "is_active_on_start" and "local_ip" are optional. If not provided, the agent will retrieve it from gossip and use the local IP address.

Run as service:
```bash
./failover-agent --config validator-config.json
```

### 2. Configure Manager (on manager server)

Create `manager-config.json`:
```json
{
  "validator1": {
    "endpoint": "http://AGENT_1_IP:8080",
    "ip": "AGENT_1_IP",
    "ledger_path": "/home/solana/ledger"
  },
  "validator2": {
    "endpoint": "http://AGENT_2_IP:8080",
    "ip": "AGENT_2_IP",
    "ledger_path": "/home/solana/ledger"
  },
  "gossip_check_command": "solana -ut gossip | grep IDENTITY",
  "cluster_rpc": "https://api.testnet.solana.com",
  "heartbeat_interval": "5s",
  "misses_before_failover": 5,
  "slot_diff_threshold": 100,
  "request_timeout": "5s",
  "dry_run": false,
  "telegram_bot_token": "BOT_TOKEN",
  "telegram_chat_id": "-CHAT_ID",
  "log_file": "/home/solana/failover/manager.log"
}
```

Run as service:
```bash
./failover-manager --config manager-config.json
```

## Secure Identity Mode

In secure identity mode, the staked identity keypair is stored only on the manager server and never on the validator servers. When failover occurs, the manager sends the identity via SSH.

### Configuration

Add these fields to manager config:
```json
{
  "secure_identity_mode": true,
  "identity_keypair_path": "/home/solana/identity.json",
  "ssh_user": "solana",
  "ssh_key_path": "~/.ssh/failover_key",
  "ssh_set_identity_command": "agave-validator --ledger {ledger} set-identity",
  "ssh_authorized_voter_command": "agave-validator --ledger {ledger} authorized-voter add"
}
```

| Field | Description |
|-------|-------------|
| `secure_identity_mode` | Enable secure mode (default: false) |
| `identity_keypair_path` | Path to staked identity keypair on manager machine |
| `ssh_user` | SSH username for validator servers |
| `ssh_key_path` | Path to SSH private key (supports `~`) |
| `ssh_set_identity_command` | Command template for set-identity. Use `{ledger}` placeholder |
| `ssh_authorized_voter_command` | Command template for authorized-voter. Use `{ledger}` placeholder |
| `ledger_path` | Ledger path on each validator (in validator1/validator2 config) |

### SSH Setup

```bash
# Generate SSH key on manager
ssh-keygen -t ed25519 -f ~/.ssh/failover_key -N ""

# Copy to validator servers
ssh-copy-id -i ~/.ssh/failover_key.pub solana@VALIDATOR1_IP
ssh-copy-id -i ~/.ssh/failover_key.pub solana@VALIDATOR2_IP
```

### How It Works

1. Manager sends `become_active` to agent with `skip_identity=true`
2. Agent only restores tower file (skips identity commands)
3. Manager SSHs to validator with identity keypair redirected to stdin:
   - `ssh user@host "agave-validator --ledger /path set-identity" < identity.json`
   - `ssh user@host "agave-validator --ledger /path authorized-voter add" < identity.json`

In this mode, the agent's `identity_change_command` and `active_identity_symlink_command` are ignored.



## Auto-Detection from Gossip

Both manager and agents can automatically detect which validator is currently active by checking Solana gossip.

### How it works

1. Run gossip command: `solana -ut gossip | grep VALIDATOR_IDENTITY`
2. Parse the IP from the first column of output
3. Compare with configured IPs to determine active validator

Example gossip output:
```
80.251.153.166  | IDENTITY_ID | 8001   | 8004  | 8010     | 80.251.153.166:8899
```

### Agent Configuration

The agent uses `gossip_check_command` and `local_ip` to auto-detect on startup:
```json
{
  "gossip_check_command": "solana -ut gossip | grep YOUR_VALIDATOR_PUBKEY",
  "local_ip": "80.251.153.166"
}
```

If gossip IP matches `local_ip`, the agent starts as active. Falls back to `is_active_on_start` if gossip check fails.

## Manager Commands

```bash
# Run with config file
./failover-manager --config manager-config.json

# Run with explicit endpoints (no auto-detection)
./failover-manager --active http://host1:8080 --passive http://host2:8080

# Shutdown all agents remotely
./failover-manager --config manager-config.json --shutdown-agent

# Generate example config
./failover-manager --generate-config
```

### Manager Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | | Path to config file |
| `--active` | | Active validator endpoint |
| `--passive` | | Passive validator endpoint |
| `--interval` | 5s | Heartbeat interval |
| `--misses` | 5 | Misses before failover |
| `--slot-threshold` | 100 | Max slot difference |
| `--timeout` | 5s | Request timeout |
| `--dry-run` | true | Don't trigger failover |
| `--shutdown-agent` | false | Send shutdown to agents and exit |
| `--log-file` | | Log to file |

### Manager Config Options

| Field | Default | Description |
|-------|---------|-------------|
| `startup_grace_period` | 2m | Duration after startup during which no failover is triggered. Allows time to verify configuration. |

## Agent Commands

```bash
# Run with config file
./failover-agent --config validator-config.json

# Run with flags
./failover-agent --listen :8080 --rpc http://127.0.0.1:8899 --peer http://peer:8080

# Generate example config
./failover-agent --generate-config
```

## Failover Process

### When Active Becomes Unhealthy

1. Manager detects unhealthy active (unreachable, process down, behind slots, etc.)
2. Manager sends `become_passive` to old active:
   - Agent backs up tower file
   - Agent removes identity (switches to unstaked)
   - Agent deletes tower file
   - Agent marks itself as passive
3. Manager sends `become_active` to new active:
   - Agent restores tower file from backup
   - Agent sets voting identity
   - Agent marks itself as active

### When Manager Goes Offline

1. Active agent detects no manager heartbeat for 30s
2. Active agent checks peer status
3. If peer is already active: become passive (avoid split-brain)
4. If peer is passive: stay active, wait for manager

## API Endpoints

### Validator Agent

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/status` | POST | Returns validator status (used by manager) |
| `/peer-status` | POST | Returns peer status (used by other validator) |
| `/failover` | POST | Execute failover command |
| `/shutdown` | POST | Shutdown the agent |

## Dry-Run Mode

**IMPORTANT**: Both programs start with `dry_run: true` by default.

This means:
- Manager will log "would failover" but not send failover commands
- Validator will log "would execute" but not run shell commands

To enable actual failover:
```bash
# Command line
--dry-run=false

# Environment variable
DRY_RUN=false

# Config file
"dry_run": false
```

## Telegram Notifications

The manager can send notifications to Telegram for critical events:

- 🔄 **Failover complete** - when failover succeeds (with reason and validator info)
- 🔴 **Server unreachable** - when a validator becomes unreachable (sent only once)
- 🟢 **Server back online** - when a validator becomes reachable again
- 🟢 **Server status** - sends status each 4 hours

### Setup

1. Create a bot with [@BotFather](https://t.me/BotFather) and get the token
2. Get your chat ID (send a message to your bot, then visit `https://api.telegram.org/bot<TOKEN>/getUpdates`)
3. Add to config:

```json
{
  "telegram_bot_token": "123456789:ABCdefGHIjklMNOpqrsTUVwxyz",
  "telegram_chat_id": "-1001234567890"
}
```

For group chats, the chat ID is negative. For private chats, use your user ID.

## Safety Features

1. **Dry-run mode**: Test without risk
2. **Multiple misses required**: Single network blip won't trigger failover
3. **Peer check**: If manager dies, active validator checks peer before action
4. **Tower backup**: Continuous tower file backup prevents slashing
5. **Tower removal**: Tower file deleted when becoming passive
6. **Gossip-based detection**: Automatic active/passive detection
7. **Telegram alerts**: Instant notifications for critical events
8. **Logging**: All actions logged for audit
