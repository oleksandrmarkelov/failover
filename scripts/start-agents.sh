#!/bin/bash
#
# Start failover agents in the correct order: passive first, then active.
#
# Prerequisites:
#   1. SSH key-based authentication must be configured
#   2. Update PASSIVE_HOST, ACTIVE_HOST, SSH_USER, and SSH_KEY below
#
# Setup SSH keys (run once on the machine where this script runs):
#   ssh-keygen -t ed25519 -f ~/.ssh/failover_key -N ""
#   ssh-copy-id -i ~/.ssh/failover_key.pub user@PASSIVE_HOST
#   ssh-copy-id -i ~/.ssh/failover_key.pub user@ACTIVE_HOST
#

set -e

# Configuration - UPDATE THESE VALUES
PASSIVE_HOST="80.251.153.166"
ACTIVE_HOST="144.76.155.11"
SSH_USER="solana"
SSH_KEY="~/.ssh/failover_key"
AGENT_SERVICE="failover-agent"
AGENT_PORT="8080"
WAIT_TIMEOUT=30

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

ssh_cmd() {
    local host=$1
    local cmd=$2
    ssh -i "$SSH_KEY" -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new "$SSH_USER@$host" "$cmd"
}

wait_for_agent() {
    local host=$1
    local timeout=$2
    local elapsed=0

    log_info "Waiting for agent on $host to be ready..."

    while [ $elapsed -lt $timeout ]; do
        if curl -s --connect-timeout 2 "http://$host:$AGENT_PORT/peer-status" > /dev/null 2>&1; then
            log_info "Agent on $host is responding"
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done

    log_error "Agent on $host did not respond within ${timeout}s"
    return 1
}

# Main script
log_info "Starting failover agents..."
log_info "Passive host: $PASSIVE_HOST"
log_info "Active host: $ACTIVE_HOST"
echo ""

# Step 1: Start passive agent
log_info "Step 1: Starting passive agent on $PASSIVE_HOST..."
if ssh_cmd "$PASSIVE_HOST" "sudo systemctl start $AGENT_SERVICE"; then
    log_info "Passive agent service started"
else
    log_error "Failed to start passive agent"
    exit 1
fi

# Step 2: Wait for passive agent to be ready
if ! wait_for_agent "$PASSIVE_HOST" "$WAIT_TIMEOUT"; then
    log_error "Passive agent is not responding. Aborting."
    exit 1
fi
echo ""

# Step 3: Start active agent
log_info "Step 2: Starting active agent on $ACTIVE_HOST..."
if ssh_cmd "$ACTIVE_HOST" "sudo systemctl start $AGENT_SERVICE"; then
    log_info "Active agent service started"
else
    log_error "Failed to start active agent"
    exit 1
fi

# Step 4: Wait for active agent to be ready
if ! wait_for_agent "$ACTIVE_HOST" "$WAIT_TIMEOUT"; then
    log_warn "Active agent is not responding, but service was started"
fi

echo ""
log_info "========================================="
log_info "Both agents started successfully!"
log_info "Passive: $PASSIVE_HOST"
log_info "Active:  $ACTIVE_HOST"
log_info "========================================="
