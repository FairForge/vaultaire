# Security Debt Tracking

## Current Status
- Total Issues: 2
- High: 0
- Medium: 2 (path traversal)
- Low: 0

## Policy
- HIGH severity: Fix immediately
- MEDIUM severity: Fix before production
- LOW severity: Fix when convenient

## Checkpoints
- [ ] Step 100: Review & update
- [ ] Step 200: Review & update
- [ ] Step 300: Security sprint
- [ ] Step 400: Review & update
- [ ] Step 500: Pre-production audit

## Running Security Check
```bash
gosec ./... 2>/dev/null | tail -20
