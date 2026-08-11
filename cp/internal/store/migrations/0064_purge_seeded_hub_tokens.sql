-- SIGMA-302: purge the operator's Hugging Face token out of every tenant row.
--
-- CP_HUGGING_FACE_TOKEN is a single operator-owned Hub account. seedHubTokenTx
-- wrote it into each tenant's llm_endpoints row at resource-create time and
-- resolveLLMSecretsTx handed it to the agent as an env-mode secret, so it landed
-- in the container's process environment as HUGGING_FACE_HUB_TOKEN — on a GPU
-- host the customer owns and has a shell on. Any tenant could read it out of
-- /proc/<pid>/environ or `docker inspect` and walk off with a credential valid
-- across every other tenant and every gated repo the operator had accepted terms
-- for.
--
-- Stopping the seed is not enough on its own, and that is why this migration
-- exists rather than a code change alone: every endpoint seeded before today
-- would keep delivering the credential on the next reconcile, so the fix would
-- read as done while the leak continued.
--
-- The columns stay. They are the per-endpoint weights credential — the rotation
-- in secrets_rotate.go re-wraps them on DEK rotation (SIGMA-281) and
-- resolveLLMSecretsTx still delivers one if it is ever present. What changes is
-- that nothing writes the OPERATOR's token into them. A tenant that needs gated
-- weights supplies their own HUGGING_FACE_HUB_TOKEN project secret, which
-- resolveLLMSecretsTx already prefers over anything stored here.
UPDATE llm_endpoints
   SET token_ciphertext = NULL,
       token_nonce      = NULL,
       token_dek_id     = NULL
 WHERE token_ciphertext IS NOT NULL;
