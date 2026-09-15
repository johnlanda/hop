-- Migration 002: workspace-trust seed evidence on launch claims. hop
-- launch records its pre-seeding outcome ("workspace trust seeded for
-- <worktree>", or "workspace trust not seeded: <reason>") with the claim
-- it writes before exec. The column is evidence only — no decision reads
-- it — and NULL on rows written before this migration.

ALTER TABLE launch_claims ADD COLUMN seed_evidence TEXT;
