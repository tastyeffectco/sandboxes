-- Stage 3: multi-provider gateway. Adds `provider` to the global
-- agent config so the in-sandbox opencode.json baseURL can be flipped
-- between e.g. zai and opencode-go without nginx changes — all keys
-- live on the host permanently, the PUT just picks which one is live.
--
-- Default 'opencode-go' for existing rows. The supported set is
-- enforced in code (internal/api/handlers.go providerBaseURLs); the
-- column is a free TEXT here so a new provider doesn't need a
-- migration, just code + nginx route.

ALTER TABLE agent_config_default
    ADD COLUMN provider TEXT NOT NULL DEFAULT 'opencode-go';
