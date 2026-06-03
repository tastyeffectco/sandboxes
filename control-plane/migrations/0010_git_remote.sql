-- 0010 — per-sandbox git push target (ops/design auto-git-push).
--
-- On each task finish, sandboxd pushes the sandbox's app workspace to
-- this remote, host-side, with a master token from
-- /etc/sandboxed/git/token injected inline. The sandbox never
-- sees the token. NULL = the feature is off for this sandbox.
--
-- Generic https git remote (GitHub/GitLab/Gitea); the platform stores
-- only the URL (not a secret). See migrations/0005 — the runner owns
-- the `migration` bookkeeping table.

ALTER TABLE sandbox ADD COLUMN git_remote_url TEXT;
