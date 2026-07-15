-- P1-5 BYO SSH onboarding. The bootstrap token now binds to a server record the
-- provisioner PRE-CREATES (bootstrap_tokens.server_id already exists, migration
-- 0002); registration updates that row instead of inserting a new one. The SSH
-- provisioner also mints a per-server ed25519 bootstrap keypair (used only to log
-- in once and lay down the installer, then deleted from authorized_keys), stored
-- as a KMS-wrapped seed + its public key. `distro` records the host OS the
-- provisioner validated (Ubuntu 22.04/24.04 / Debian 12 only).

ALTER TABLE servers ADD COLUMN IF NOT EXISTS bootstrap_pubkey      TEXT;
ALTER TABLE servers ADD COLUMN IF NOT EXISTS bootstrap_key_wrapped BYTEA;
ALTER TABLE servers ADD COLUMN IF NOT EXISTS distro                TEXT;

-- Bind the token to the pre-created server for good: a token whose server is
-- deleted is meaningless, so cascade it away rather than orphan it.
ALTER TABLE bootstrap_tokens DROP CONSTRAINT IF EXISTS bootstrap_tokens_server_id_fkey;
ALTER TABLE bootstrap_tokens
  ADD CONSTRAINT bootstrap_tokens_server_id_fkey
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE;
