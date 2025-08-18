#!/bin/bash

echo "==================================="
echo "VAULTAIRE STATUS VERIFICATION"
echo "==================================="
echo ""

echo "📁 PROJECT STRUCTURE:"
echo "-------------------"
tree -I 'vendor|.git|bin|*.exe' -L 3 || ls -la

echo ""
echo "📊 GIT STATUS:"
echo "-------------"
git status --short
git log --oneline -5

echo ""
echo "📈 PROGRESS CHECK:"
echo "-----------------"
grep -E "Step [0-9]+:" PROGRESS.md | tail -5

echo ""
echo "✅ COMPLETED FEATURES:"
echo "---------------------"
echo "Checking for key files..."
[ -f "internal/api/s3_handler.go" ] && echo "✓ S3 Handler exists" || echo "✗ S3 Handler missing"
[ -f "internal/drivers/local.go" ] && echo "✓ Local driver exists" || echo "✗ Local driver missing"
[ -f "internal/engine/engine.go" ] && echo "✓ Engine exists" || echo "✗ Engine missing"
[ -f "internal/events/logger.go" ] && echo "✓ Event logger exists" || echo "✗ Event logger missing"
[ -f "internal/tenant/tenant.go" ] && echo "✓ Tenant system exists" || echo "✗ Tenant system missing"

echo ""
echo "🧪 TEST STATUS:"
echo "--------------"
go test ./... 2>&1 | grep -E "ok|FAIL" | head -10

echo ""
echo "📦 GO MODULES:"
echo "-------------"
grep -E "require|module" go.mod | head -10

echo ""
echo "🎯 CURRENT STEP:"
echo "---------------"
tail -3 PROGRESS.md | grep -E "Step|Current"

echo ""
echo "📋 KEY PATTERNS IN USE:"
echo "----------------------"
grep -r "Container" internal/ --include="*.go" | wc -l | xargs echo "Container pattern uses:"
grep -r "Engine" internal/ --include="*.go" | wc -l | xargs echo "Engine pattern uses:"
grep -r "Stream" internal/ --include="*.go" | wc -l | xargs echo "Stream pattern uses:"
grep -r "io.Reader" internal/ --include="*.go" | wc -l | xargs echo "io.Reader uses:"

echo ""
echo "==================================="
echo "VERIFICATION COMPLETE"
echo "==================================="
