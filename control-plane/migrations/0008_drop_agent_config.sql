-- Drop the single-global-row agent-config table.
--
-- /v1/agent-config has been removed: agent configuration (AGENTS.md,
-- CLAUDE.md, opencode.json, etc.) is now the upstream backend-side and pushed via
-- the generic PUT /v1/sandboxes/{id}/files endpoint. The platform no
-- longer has a "provider" / "model" / "brief" concept.

DROP TABLE IF EXISTS agent_config_default;
