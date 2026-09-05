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

## Config / docs

11. **`config.example.yaml`** — `session-affinity-ttl: "24h"` (see #7).
    All other upstream options merge in as a union.

12. **AGENTS.md Deployment Notes / Merge Policy sections** — ice-only;
    upstream's new convention bullets merge in above them.
