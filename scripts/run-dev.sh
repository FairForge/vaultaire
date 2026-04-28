#!/bin/bash
# Development run script

export STORAGE_MODE=local
export LOCAL_STORAGE_PATH=/tmp/vaultaire-data
export PORT=8000
export DATABASE_URL="postgres://localhost/vaultaire?sslmode=disable"

echo "Starting Vaultaire in development mode..."
go run cmd/vaultaire/main.go
