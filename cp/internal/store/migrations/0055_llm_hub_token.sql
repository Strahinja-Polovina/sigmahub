-- The Hugging Face credential an inference endpoint pulls its weights with
-- (SIGMA-213).
--
-- The llm runtime catalog has always rendered HUGGING_FACE_HUB_TOKEN as a
-- secret REFERENCE, resolved agent-side at container create. Nothing ever
-- created the secret behind it. Two failures came out of that, and they are
-- opposite ends of the same missing column:
--
--   * the agent refuses to create a container whose reference the control plane
--     does not answer ("secret %q referenced but not provided"), so EVERY vLLM
--     endpoint failed its first apply unless an operator happened to have made
--     a project secret of exactly that name — including the ordinary case of a
--     public model that needs no credential at all; and
--   * the wizard gated gated-model selection on CP_HUGGING_FACE_TOKEN, which
--     authenticates the model PICKER's metadata calls inside the control-plane
--     process and has never had anything to do with the download. A gated model
--     therefore sailed through the wizard and 401'd tens of gigabytes into a
--     pull, on a host already billed at GPU rates.
--
-- So the token becomes a value the control plane stores per endpoint, seeded at
-- provision from CP_HUGGING_FACE_TOKEN — one variable, one meaning: the Hugging
-- Face account this control plane acts as, for looking a model up AND for
-- fetching it.
--
-- The three columns are the same envelope db_credentials uses for a generated
-- database password (0011): AES-256-GCM under the org DEK, with the AAD bound
-- to (org, 'llm', resource id) so a ciphertext moved between rows fails to
-- open. NULLable, and NULL is the common state, not an unfinished one: a
-- control plane that holds no token, an endpoint whose project already carries
-- the operator's own HUGGING_FACE_HUB_TOKEN secret, and every ollama endpoint
-- all store nothing here. The reconciler renders the reference only when there
-- is a value behind it, which is what makes the public-model case work again.
ALTER TABLE llm_endpoints
    ADD COLUMN IF NOT EXISTS token_ciphertext BYTEA,
    ADD COLUMN IF NOT EXISTS token_nonce      BYTEA,
    ADD COLUMN IF NOT EXISTS token_dek_id     TEXT REFERENCES org_deks (id);
