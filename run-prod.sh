#!/bin/bash
# Production run script - loads from .env.production
if [ ! -f .env.production ]; then
    echo "ERROR: .env.production not found"
    echo "Create it with your credentials (never commit it)"
    exit 1
fi

# Source and export all variables
set -a
source .env.production
set +a

./bin/vaultaire
