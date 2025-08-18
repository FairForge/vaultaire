I'm building Vaultaire, a multi-cloud storage orchestration platform. I'm at Step 48 of 510.

## CRITICAL CONTEXT - READ FIRST:
- GitHub: github.com/fairforge/vaultaire
- I have CLAUDE.md with all rules and patterns
- PROJECT_MASTER.md has complete documentation  
- PROGRESS.md tracks steps completed

## Current Status:
- Steps 1-47: COMPLETE ✅
- Step 48: Rate Limiting (starting now)
- Architecture: Engine/Container/Artifact patterns (NOT storage/bucket/object)
- Multi-tenancy: COMPLETE with isolation

## Project Structure Verified:
vaultaire/
├── internal/
│   ├── api/          # S3 handlers ✅
│   ├── drivers/      # Storage backends ✅
│   ├── engine/       # Core orchestration ✅
│   ├── events/       # ML logging ✅
│   └── tenant/       # Multi-tenancy ✅
├── cmd/vaultaire/    # Main server ✅
├── CLAUDE.md         # Development rules
├── PROJECT_MASTER.md # Full documentation
└── PROGRESS.md       # Step tracker

## Key Patterns (MUST MAINTAIN):
1. Every function uses context.Context
2. Stream with io.Reader (never []byte)
3. Log events for ML training
4. Namespace all tenant data
5. Use Engine/Container/Artifact internally

## Step 48 Requirements:
Implement rate limiting middleware using golang.org/x/time/rate (already installed).
- Per-tenant limits
- Token bucket algorithm
- HTTP 429 responses
- Proper headers

## Modified Plan:
- Total steps: 510 (added 10 for 3FS patterns)
- Step 75: Parallel I/O (critical)
- Step 85: Transaction logging (critical)
- Must maintain 15 steps/day velocity

Help me implement Step 48. All code must follow patterns in CLAUDE.md.
