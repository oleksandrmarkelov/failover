# Solana Validator Failover System

A failover system for Solana validators written in Go. Consists of two programs:
- **Manager** - runs on manager server, monitors validators and triggers failover
- **Validator Agent** - runs on each validator server, responds to health checks and executes failover commands

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

### 1. Configure Manager (on manager server)

Create `manager-config.json`:
```json
{
  "validator1": {
    "endpoint": "http://192.168.1.10:8080",
    "ip": "80.251.153.166"
  },
  "validator2": {
    "endpoint": "http://192.168.1.11:8080",
    "ip": "80.251.153.167"
  },
  "gossip_check_command": "solana -ut gossip | grep DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY",
  "cluster_rpc": "https://api.testnet.solana.com",
  "dry_run": false
}
```

Run:
```bash
./failover-manager --config manager-config.json
```

### 2. Configure Agents (on each validator server)

Create `validator-config.json` on each server:
```json
{
  "listen_addr": ":8080",
  "local_rpc": "http://127.0.0.1:8899",
  "process_name": "agave-validator",
  "peer_endpoint": "http://PEER_IP:8080",
  "gossip_check_command": "solana -ut gossip | grep DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY",
  "local_ip": "THIS_SERVER_PUBLIC_IP",
  "tower_file_path": "/path/to/ledger/tower-1_9-IDENTITY.bin",
  "tower_backup_command": "etcdctl put /solana/tower ...",
  "tower_restore_command": "etcdctl get /solana/tower ...",
  "identity_change_command": "agave-validator --ledger /path set-identity /path/validator-keypair.json",
  "identity_remove_command": "agave-validator --ledger /path set-identity /path/unstaked-identity.json",
  "dry_run": false
}
```

Run:
```bash
./failover-agent --config validator-config.json
```

## Auto-Detection from Gossip

Both manager and agents can automatically detect which validator is currently active by checking Solana gossip.

### How it works

1. Run gossip command: `solana -ut gossip | grep VALIDATOR_IDENTITY`
2. Parse the IP from the first column of output
3. Compare with configured IPs to determine active validator

Example gossip output:
```
80.251.153.166  | DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY | 8001   | 8004  | 8010     | 80.251.153.166:8899
```

### Manager Configuration

The manager uses `validator1`, `validator2`, and `gossip_check_command` to auto-detect:
```json
{
  "validator1": {
    "endpoint": "http://192.168.1.10:8080",
    "ip": "80.251.153.166"
  },
  "validator2": {
    "endpoint": "http://192.168.1.11:8080", 
    "ip": "80.251.153.167"
  },
  "gossip_check_command": "solana -ut gossip | grep YOUR_VALIDATOR_PUBKEY"
}
```

You can still use explicit `active_validator` and `passive_validator` if preferred.

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

## Agent Commands

```bash
# Run with config file
./failover-agent --config validator-config.json

# Run with flags
./failover-agent --listen :8080 --rpc http://127.0.0.1:8899 --peer http://peer:8080

# Generate example config
./failover-agent --generate-config
```

## Configuration Reference

### Manager Config

```json
{
  "validator1": {
    "endpoint": "http://192.168.1.10:8080",
    "ip": "80.251.153.166"
  },
  "validator2": {
    "endpoint": "http://192.168.1.11:8080",
    "ip": "80.251.153.167"
  },
  "gossip_check_command": "solana -ut gossip | grep PUBKEY",
  "cluster_rpc": "https://api.testnet.solana.com",
  "slot_check_interval": "30s",
  "heartbeat_interval": "5s",
  "misses_before_failover": 5,
  "slot_diff_threshold": 100,
  "request_timeout": "5s",
  "dry_run": true,
  "log_file": "/var/log/failover-manager.log",
  "telegram_bot_token": "YOUR_BOT_TOKEN",
  "telegram_chat_id": "YOUR_CHAT_ID"
}
```

### Validator Agent Config

```json
{
  "listen_addr": ":8080",
  "local_rpc": "http://127.0.0.1:8899",
  "process_name": "agave-validator",
  "peer_endpoint": "http://PEER_IP:8080",
  "is_active_on_start": false,
  "manager_timeout": "30s",
  "tower_backup_command": "etcdctl put /solana/tower/$(hostname) \"$(cat tower.bin | base64)\"",
  "tower_restore_command": "etcdctl get /solana/tower/peer --print-value-only | base64 -d > tower.bin",
  "identity_change_command": "agave-validator --ledger /path set-identity /path/validator-keypair.json",
  "identity_remove_command": "agave-validator --ledger /path set-identity /path/unstaked-identity.json",
  "tower_file_path": "/path/to/ledger/tower-1_9-IDENTITY.bin",
  "validator_identity": "DQx6XD5fWQ2Pbkg4Fi4gVzLbGg6c4ST7ZgXTawZZAXEY",
  "gossip_check_command": "solana -ut gossip | grep PUBKEY",
  "local_ip": "80.251.153.166",
  "dry_run": true,
  "log_file": "/var/log/failover-validator.log"
}
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
| `/health` | GET | Simple health check |

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

## Example Commands

### Tower Backup/Restore

```bash
# Backup to etcd (base64 encoded)
etcdctl put /solana/tower/primary "$(cat /var/solana/ledger/tower-1_9-*.bin | base64)"

# Restore from etcd
etcdctl get /solana/tower/primary --print-value-only | base64 -d > /var/solana/ledger/tower-1_9-*.bin
```

### Identity Commands

```bash
# Change to voting identity (become active)
agave-validator --ledger /var/solana/ledger set-identity /var/solana/validator-keypair.json

# Change to unstaked identity (become passive)
agave-validator --ledger /var/solana/ledger set-identity /var/solana/unstaked-identity.json
```

### Remote Agent Shutdown

```bash
# Shutdown all agents (validators keep running, only agents stop)
./failover-manager --config manager-config.json --shutdown-agent
```
