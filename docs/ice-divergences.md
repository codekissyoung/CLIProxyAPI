# ice-branch Divergences from Upstream

The `ice` branch deliberately carries the local divergences listed below.
When merging `origin/main` into `ice`, resolve conflicts per the AGENTS.md
Upstream Merge Policy: prefer upstream semantics, but keep every divergence
in this list. Anything divergent that is NOT listed here is merge residue —
take upstream (precedent: the `cancelTransferred` pattern in
`internal/pluginhost/host_callbacks.go`, dropped in the 2026-09-05 merge).

Key conflict sites are tagged in code with `// ice divergence: ...`.

## Auth / scheduling (sdk/cliproxy/auth)

1. **Home/temp session-affinity model** (`selector.go`, `home_selection.go`,
   `home_session_alias.go`, `home_concurrency.go`)
   `SessionAffinitySelector` keeps a second `fallbackCache` and per-slot
   `sessionLocks`; `Pick` binds a session to a "home" auth and only fails over
   through the temp path. Production evidence: stable account usage for
   long-lived Claude Code / Codex sessions. This is the documented divergence
   referenced by AGENTS.md. Upstream's Merkle LCP matcher and
   fork/subagent alias isolation layer on top of it — keep both.
   Upstream's `TestManagerSessionAffinityAliasCooldownPreservesSelection`
   (added in 5ab0bca0) asserts failover rebind semantics; the ice adaptation
   expects explicit sessions to return to the recovered home credential.

2. **Session binding counts** (`session_cache.go`)
   `bindingCounts` + `SetBindingCountObserver`/`BindingCount` report logical
   session bindings per auth (used for pool observability). Upstream's LRU
   eviction (`groups`/`evictionOrder`/`maxEntries`) must decrement these
   counts through `removeAliasGroupLocked` — eviction decrements only when an
   entry actually matched (idempotent).

3. **SessionCache `stopped` guards** (`session_cache.go`)
   All mutators reject work after `Stop()` (regression guard for
   post-shutdown writes; `TestSessionCacheRejectsWritesAfterStop`).
   Keep alongside upstream's `ensureInitializedLocked` nil-guards.

4. **xAI OAuth concurrency gating** (`conductor_execution.go`,
   `conductor.go`, `home_concurrency.go`)
   `concurrencyBusy`/`releaseConcurrency`/`wrapStreamConcurrencyRelease` plus
   `xaiOAuthConcurrencyMu`/`xaiOAuthInFlight` serialize xAI OAuth account
   usage; the busy signal short-circuits before upstream's
   `preferredExecutionAttemptError` bookkeeping. Keep both.

5. **Transition-based revocation counting** (`conductor_refresh.go`)
   `metrics.RecordAccountInvalidation` fires exactly once on the transition
   into the unauthorized state, including under upstream's
   `hasValidAccessToken` retention path (where the credential is never marked
   unavailable; `previousUnauthorized` from `LastError` is the transition
   marker). Pinned by `TestManager_RefreshAuthForRequest_RevocationCountedOnce`.

6. **Codex turn-metadata session IDs** (`selector.go`,
   `codexTurnMetadataSessionID`)
   Session IDs are also extracted from the body-mirrored
   `client_metadata.x-codex-turn-metadata` header, which upstream does not
   handle. Must stay ahead of upstream's slot/conv/thread header blocks.

7. **24h session-affinity TTL default** (`sdk/cliproxy/service_config.go`,
   `config.example.yaml`)
   ice defaults `sessionAffinityTTL` to 24h (upstream example: 1h).
   Pinned by `TestNormalizedRoutingRuntimeStateDefaultsAffinityTTLTo24Hours`.

14. **Overload failures preserve session bindings** (2026-09-10;
    `selector.go` `SessionAffinitySelector.OnResult`,
    `conductor_cooldown.go` `isOverloadResultError`;
    pinned by `selector_overload_test.go`)
    Upstream releases explicit and LCP session bindings on every non-exempt
    failure. ice keeps the binding when the failure is an upstream
    capacity/overload rejection (`server_is_overloaded` code or a 502/503/504
    whose message says the server is overloaded; 429 rate limiting is
    deliberately excluded and keeps quota semantics). Production evidence:
    OpenAI peak-hour `server_is_overloaded` storms re-homed one session 28
    times in 13 minutes, losing the prefix cache on every hop and amplifying
    the overload. The conductor cooldown still marks the overloaded credential
    unavailable with `NextRetryAfter`, so the pick path temporarily routes the
    session through a temp account and the existing `home_recovered` path
    migrates it back on recovery.

15. **Per-account in-flight capacity gate** (2026-09-11; TOCTOU review fix
    2026-09-12; `credential_capacity.go`,
    `conductor_execution.go` `acquireExecutionConcurrency`,
    `internal/config` `account-concurrency-limit`;
    pinned by `credential_capacity_test.go`,
    `internal/config/account_concurrency_test.go`)
    sub2api-style `accounts.concurrency`: each execution attempt reserves an
    in-flight slot for the picked credential through an atomic
    check-and-reserve under a single mutex (`tryAcquireAccountCapacity`) and
    releases it exactly once at the existing `releaseConcurrency` exit points
    (streams hold the slot until the channel drains). The reservation is taken
    immediately after the pick in the same loop iteration, and every abandon
    path of the attempt (prepare failure, interceptor error, per-model
    failure, exhausted candidates) flows through that release. A saturated
    credential is deliberately NOT pre-filtered at selection: the pick loop
    tries it, admission rejects it, the credential is marked tried, and the
    loop moves to the next candidate; when every candidate is saturated the
    request fails with a retryable 429 `credential_concurrency_exceeded`
    (Retry-After: 1s) instead of a plain `auth_not_found`. Error priority:
    real provider errors rank highest (unchanged), and a pick failure
    carrying a cooldown or unavailable reason (`modelCooldownError` /
    `auth_unavailable`) outranks the busy error — busy is returned only when
    the pick exhausted all candidates with a bare `auth_not_found`, i.e.
    capacity was the sole failure cause, so the 429 + Retry-After contract
    never masks a genuine cooldown. Session affinity
    keeps the home binding during such a temporary failover and the existing
    home_recovered path migrates the session back once a slot frees. Global
    default via `account-concurrency-limit` (0 = disabled, the default,
    preserving legacy behavior); per-credential override via auth-file
    `attributes["concurrency"]` ("0" = unlimited, invalid values fall back to
    the global default). No-op under Home mode (Home has its own concurrency
    lifecycle). Upstream has no equivalent.
    Review history: the first version split the limit check from the
    increment (selection-funnel pre-filter plus unconditional increment at
    execution) — a TOCTOU race that let a 50-goroutine burst push a limit-5
    credential far past its cap, and it also produced inconsistent client
    errors (`auth_not_found` vs busy). The funnel hooks were reverted; the
    gate now lives only at the atomic admission point. The gate must never be
    hooked into `isAuthBlockedForModel`, which is also consulted mid-attempt
    by `filterExecutionModels` and would self-block the attempt holding the
    slot.

## Executors (internal/runtime/executor)

8. **Per-auth+proxy Codex transport cache** (`codex_executor_request.go`,
   `codexHTTPClient`)
   Transports are cached per `auth.ID + effectiveProxyURL` to keep a
   single-client upstream shape (AGENTS.md "Forwarding model"). Upstream
   builds one-off clients; when upstream adds wrappers (e.g.
   `TrackHTTPClientRoundTripOnly`), wrap the cached client, don't replace it.

9. **Codex output-item retention cap** (`codex_executor_stream.go`,
   `collectOutputItem`, `codexOutputItemsRetainLimit` = 16 MiB)
   Bounds memory spent patching `response.completed` output. Upstream
   collects unboundedly via `collectCodexOutputItemDone`.

10. **Codex upstream-error diagnostics** (`codex_executor_terminal.go`)
    `codexErrorAuthInfo`, `logCodexUpstreamError`, context-reject
    blocked/record helpers — local diagnostics requiring the
    `cliproxyauth`/`cliproxyexecutor` imports upstream removes.

11. **Codex identity surface hardening** (2026-09-09;
    `codex_executor_request.go`, `codex_websockets_request.go`,
    `codex_websockets_connection.go`, `codex_executor_execute.go`,
    `codex_executor_stream.go`, `codex_openai_images.go`;
    pinned by `codex_identity_hardening_test.go`)
    Extensions of upstream's identity-confuse plus unconditional strips:
    - body-mirrored `client_metadata.x-codex-turn-metadata` gets its
      `workspaces` subtree stripped unconditionally
      (`stripCodexBodyTurnMetadataWorkspaces`); upstream only strips the
      header copy;
    - `client_metadata.ws_request_header_*` mirrors of identity/session
      headers are deleted (`stripCodexBodyIdentityMetadataMirrors`);
    - WS handshakes never forward `x-codex-turn-state` upstream (sticky
      turn token minted under a possibly different pool account);
    - turn-metadata `session_id`/`thread_id` and top-level
      `session_id`/`conversation` are confused per account
      (`confuseTrackedValue`);
    - proxy-generated `prompt_cache_key`/`Session-Id` are account-scoped
      (`accountScopedPromptCacheKey`) so a generated key can never appear
      under two accounts;
    - `codexIdentityConfuseEnabled` depends only on
      `codex.identity-confuse`, not on the routing strategy (a routing
      change can no longer silently disable confusion);
    - `X-Client-Request-Id` stays per-request (client value passthrough,
      fresh UUID when absent) instead of being collapsed onto the confused
      session id;
    - client-facing error bodies are parsed from the identity-restored copy
      on the HTTP/SSE/compact/WS/images paths, so confused identifiers
      never leak to the client.

## Config / docs

12. **`config.example.yaml`** — `session-affinity-ttl: "24h"` (see #7).
    All other upstream options merge in as a union.

12. **AGENTS.md Deployment Notes / Merge Policy sections** — ice-only;
    upstream's new convention bullets merge in above them.

## API surface (sdk/api/handlers, internal/logging, internal/config)

13. **Pool auth pin + auth attribution headers** (2026-09-09;
    `internal/config/sdk_config.go` `AllowPoolPinHeader`,
    `sdk/api/handlers/handlers_context.go` `WithPoolPinFromRequest`,
    `sdk/api/handlers/openai/openai_responses_handlers.go`,
    `sdk/api/handlers/handlers.go` `requestExecutionMetadata`,
    `sdk/api/handlers/header_filter.go`,
    `internal/logging/cpa_trace.go`,
    `config.example.yaml` `allow-pool-pin-header`;
    pinned by `handlers_pool_pin_test.go`,
    `openai_responses_pool_pin_test.go`, `cpa_trace_test.go`)
    When `allow-pool-pin-header: true`, the internal
    `X-Pool-Pin-Account: <auth.ID>` request header pins POST /v1/responses
    (stream and non-stream) to a specific auth ID (auth JSON path relative to
    auths/); unknown IDs fail deterministically with `auth_not_found`, no
    fallback. Every plain-HTTP response also carries `X-Pool-Account:
    <auth.ID>` of the auth that actually served the request (last selection
    wins across failover), emitted by the CPA trace commit-time writer and
    reserved against upstream spoofing in `cpaReservedResponseHeaders`.
    Local operational tooling for ai-relay pool testing; upstream has no
    equivalent — keep on merge.
