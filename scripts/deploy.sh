#!/bin/bash
set -euo pipefail

# Vaultaire Deployment Script
# Usage: ./scripts/deploy.sh [environment] [component]

ENVIRONMENT="${1:-staging}"
COMPONENT="${2:-all}"
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo 'dev')}"
REGISTRY="${REGISTRY:-ghcr.io/fairforge}"

echo "🚀 Deploying Vaultaire"
echo "   Environment: $ENVIRONMENT"
echo "   Component:   $COMPONENT"
echo "   Version:     $VERSION"
echo ""

# Validate environment
case "$ENVIRONMENT" in
    development|dev)
        ENVIRONMENT="development"
        ;;
    staging|stage)
        ENVIRONMENT="staging"
        ;;
    production|prod)
        ENVIRONMENT="production"
        ;;
    *)
        echo "❌ Invalid environment: $ENVIRONMENT"
        echo "   Valid options: development, staging, production"
        exit 1
        ;;
esac

# Production requires confirmation
if [ "$ENVIRONMENT" = "production" ]; then
    echo "⚠️  WARNING: Deploying to PRODUCTION"
    read -p "   Type 'yes' to confirm: " confirm
    if [ "$confirm" != "yes" ]; then
        echo "❌ Deployment cancelled"
        exit 1
    fi
fi

# Build the binary
echo "📦 Building vaultaire..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.Version=$VERSION -X main.Environment=$ENVIRONMENT" \
    -o bin/vaultaire \
    ./cmd/vaultaire

# Run tests before deployment (skip in production - should already be tested)
if [ "$ENVIRONMENT" != "production" ]; then
    echo "🧪 Running tests..."
    go test -race -short ./...
fi

echo ""
echo "✅ Build complete!"
echo "   Binary: bin/vaultaire"
echo "   Version: $VERSION"
echo "   Environment: $ENVIRONMENT"
echo ""
echo "Next steps for $ENVIRONMENT deployment:"

case "$ENVIRONMENT" in
    development)
        echo "   1. Run: VAULTAIRE_ENV=development ./bin/vaultaire"
        echo "   2. Or use: docker-compose -f docker-compose.dev.yml up"
        ;;
    staging)
        echo "   1. Copy binary to staging server"
        echo "   2. Update systemd service"
        echo "   3. Reload: sudo systemctl restart vaultaire"
        ;;
    production)
        echo "   1. Copy binary to hub-nyc-1"
        echo "   2. Run pre-deployment checks"
        echo "   3. Deploy with: sudo systemctl restart vaultaire"
        echo "   4. Verify health: curl https://api.stored.ge/health"
        ;;
esac
