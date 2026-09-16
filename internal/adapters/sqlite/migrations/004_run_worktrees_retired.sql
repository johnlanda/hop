-- Migration 004: the post-merge worktree retirement run fact
-- (docs/plan/phase-3-worktree-retirement.md). worktrees_retired_at is set
-- once, inside a lease-fenced controller transaction, when every worktree
-- row of the run has reached a final retirement state; it is never cleared
-- or moved. NULL means not retired, which is every row written before this
-- migration.

ALTER TABLE runs ADD COLUMN worktrees_retired_at TEXT;
