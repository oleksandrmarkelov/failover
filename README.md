# Solana Validator Failover System

A failover system for Solana validators written in Go. Consists of two programs:
- **Manager** - runs on manager server, monitors validators and triggers failover
- **Validator Agent** - runs on each validator server, responds to health checks and executes failover commands

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     MANAGER SERVER                          │
│                                                             │
│   ┌─────────────────────────────────────────────────────┐   │
│   │                   Manager Program                    │   │
│   │  - Pings both validators every 2 seconds            │   │
│   │  - Checks process running + slot difference         │   │
│   │  - Triggers failover if active is unhealthy         │   │
│   └───────────────────┬─────────────────┬───────────────┘   │
│                       │                 │                   │
└───────────────────────┼─────────────────┼───────────────────┘
                        │                 │
                   HTTP │                 │ HTTP
                        │                 │
        ┌───────────────▼──┐          ┌───▼───────────────┐
        │  ACTIVE SERVER   │          │  PASSIVE SERVER   │
        │                  │◀────────▶│                   │
        │  Validator Agent │  peer    │  Validator Agent  │
        │  agave-validator │ heartbeat│  agave-validator  │
        │  (VOTING)        │          │  (standby)        │
        └──────────────────┘          └───────────────────┘
```

## Features

### Manager Program
- ✅ Pings two predefined validator IPs (configured)
- ✅ Switches to passive if active doesn't respond (after N misses)
- ✅ Checks if validator process is running
- ✅ Checks slot difference (validator behind network)
- ✅ Dry-run mode (test without actual failover)

### Validator Agent Program
- ✅ Responds to manager health checks
- ✅ Reports process status and slot information
- ✅ Backs up tower file to etcd regularly
- ✅ Monitors manager heartbeat - if manager goes offline, checks peer
- ✅ Executes identity change commands on failover
- ✅ Dry-run mode (logs commands without executing)

## Building

```bash
# Build manager
go build -o failover-manager ./cmd/manager

# Build validator agent
go build -o failover-agent ./cmd/validator
```

## Usage

### Manager Server

```bash
# Using command line flags
./failover-manager \
  --active http://192.168.1.10:8080 \
  --passive http://192.168.1.11:8080 \
  --interval 2s \
  --misses 5 \
  --slot-threshold 100 \
  --dry-run=false

# Using config file
./failover-manager --config manager-config.json

# Generate example config
./failover-manager --generate-config
```

### Validator Servers

**On Active Validator (192.168.1.10):**
```bash
./failover-agent \
  --listen :8080 \
  --rpc http://127.0.0.1:8899 \
  --process agave-validator \
  --peer http://192.168.1.11:8080 \
  --active \
  --dry-run=false

# Or with config file
./failover-agent --config validator-active.json
```

**On Passive Validator (192.168.1.11):**
```bash
./failover-agent \
  --listen :8080 \
  --rpc http://127.0.0.1:8899 \
  --process agave-validator \
  --peer http://192.168.1.10:8080 \
  --dry-run=false

# Or with config file
./failover-agent --config validator-passive.json
```

## Configuration

### Manager Config (manager-config.json)

```json
{
  "active_validator": "http://192.168.1.10:8080",
  "passive_validator": "http://192.168.1.11:8080",
  "heartbeat_interval": "2s",
  "misses_before_failover": 5,
  "slot_diff_threshold": 100,
  "request_timeout": "5s",
  "dry_run": true
}
```

### Validator Config (validator-config.json)

```json
{
  "listen_addr": ":8080",
  "local_rpc": "http://127.0.0.1:8899",
  "process_name": "agave-validator",
  "peer_endpoint": "http://192.168.1.11:8080",
  "is_active_on_start": true,
  "manager_timeout": "30s",
  "tower_backup_interval": "30s",
  "tower_backup_command": "etcdctl put /solana/tower ...",
  "tower_restore_command": "etcdctl get /solana/tower ...",
  "identity_change_command": "agave-validator set-identity ...",
  "identity_remove_command": "agave-validator set-identity --unstaked ...",
  "dry_run": true
}
```

## Configuration Parameters

### Manager

| Parameter | Default | Description |
|-----------|---------|-------------|
| `heartbeat_interval` | 2s | How often to check validators |
| `misses_before_failover` | 5 | Consecutive failures before failover (total timeout: interval × misses) |
| `slot_diff_threshold` | 100 | Maximum allowed slots behind network |
| `request_timeout` | 5s | HTTP request timeout |
| `dry_run` | true | If true, logs but doesn't trigger failover |

### Validator Agent

| Parameter | Default | Description |
|-----------|---------|-------------|
| `manager_timeout` | 30s | Time without manager heartbeat before checking peer |
| `tower_backup_interval` | 30s | How often to backup tower file to etcd |
| `dry_run` | true | If true, logs commands but doesn't execute |

## Failover Scenarios

| Scenario | Detection | Action |
|----------|-----------|--------|
| Active server crash | No HTTP response for 5 consecutive checks | Switch to passive |
| agave-validator process died | `process_running: false` | Switch to passive |
| Validator behind network | `slot_diff > threshold` | Switch to passive |
| RPC unhealthy | `is_healthy: false` | Switch to passive |
| Manager goes offline | No heartbeat for 30s | Active checks peer, stays active if peer is passive |

## API Endpoints

### Validator Agent

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/status` | POST | Returns validator status (used by manager) |
| `/peer-status` | POST | Returns peer status (used by other validator) |
| `/failover` | POST | Execute failover command (become_active/become_passive) |
| `/health` | GET | Simple health check |

## Dry-Run Mode

**IMPORTANT**: Both programs start with `dry_run: true` by default.

This means:
- Manager will log "would failover" but not actually send failover commands
- Validator will log "would execute command" but not actually run shell commands

To enable actual failover:
```bash
# Command line
--dry-run=false

# Environment variable
DRY_RUN=false

# Config file
"dry_run": false
```

## Safety Features

1. **Dry-run mode**: Test without risk
2. **Multiple misses required**: Single network blip won't trigger failover
3. **Peer check**: If manager dies, active validator checks peer before doing anything
4. **Tower backup**: Continuous tower file backup prevents slashing on failover
5. **Logging**: All actions are logged for audit

## Example Tower Backup Commands

```bash
# Backup to etcd (base64 encoded)
etcdctl put /solana/tower/primary "$(cat /var/solana/ledger/tower-*.bin | base64)"

# Restore from etcd
etcdctl get /solana/tower/primary --print-value-only | base64 -d > /var/solana/ledger/tower-*.bin
```

## Example Identity Commands

```bash
# Change to voting identity
agave-validator --ledger /var/solana/ledger set-identity /var/solana/validator-keypair.json

# Change to unstaked identity (passive)
agave-validator --ledger /var/solana/ledger set-identity /var/solana/unstaked-identity.json
```

