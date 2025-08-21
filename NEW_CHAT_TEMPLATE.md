# Continue Vaultaire Development - Step [XX]

Please review my project documentation and help me continue development.

## Project Context Files
Please read these files in my project to understand the full context:

1. **VAULTAIRE_MASTER_PLAN.md** - Complete project vision, architecture, and 510-step plan
2. **CLAUDE.md** - My development standards and workflow
3. **PROGRESS.md** - Current progress (last 50 lines)
4. **STEP_[XX-1]_COMPLETE.md** - What I just completed
5. **STEP_CHECKLIST_TEMPLATE.md** - My TDD process

## Current Status
- Completed: Step [XX-1]
- Working on: Step [XX]
- Architecture: Engine/Container/Artifact (NOT storage/bucket/object)
- Method: Strict TDD (tests first, always)

## Step [XX] Requirements
[Copy from VAULTAIRE_MASTER_PLAN.md]

## My Workflow Reminders
1. Write tests FIRST (Red phase)
2. Write minimal code to pass (Green phase)
3. Refactor while keeping tests green
4. Coverage must be >80%
5. Use io.Reader for streaming (never []byte)
6. Wrap all errors with context
7. Tenant isolation via context

## Help Needed
Help me implement Step [XX] following my TDD process:
1. First, write the test file
2. Then implement to make tests pass
3. Show integration points
4. Provide curl commands for testing

Let's continue building Vaultaire!