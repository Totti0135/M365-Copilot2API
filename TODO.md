# Project TODO

Status legend: `[x]` verified, `[ ]` not verified, `[-]` blocked, `[c]` canceled by user and not verified.

## Repository Baseline

- [x] Read global `AGENTS.md`.
- [x] Confirm project-level `AGENTS.md` is absent.
- [x] Inspect `git status`, `git diff`, and `git log --oneline -10` without altering existing work.
- [x] Reconciled the current dirty baseline without reset, checkout, clean, deletion, or overwriting existing and unknown changes.

## Graph Authorization Wizard

- [ ] Verify authorization start, status, revoke, batch user creation, error handling, and localization end to end.
- [ ] Verify newly created accounts are not represented as OAuth-authorized accounts.

## Network Fault Matrix

- [x] Register `/responses` as a behavior-identical alias of `/v1/responses`; verify it uses API-key authentication, never falls into administrator middleware, and does not return `administrator login required`. The targeted test passed 20 consecutive runs with a 5-minute timeout.
- [x] Fix WebSocket test sequencing so business frames are channel-controlled and the server maintains a read loop.
- [x] Run the corrected WebSocket close/frame-channel test 20 consecutive times with an explicit timeout.
- [x] Run the current targeted network, half-open, overload, and isolation tests 20 consecutive times with an explicit timeout. Latest rerun passed: `internal/chathub` 4.704s and `internal/web` 92.261s.
- [x] Obtain a clean 100-run concurrent stress result. The failure was caused by test design exhausting Windows ephemeral ports, not production behavior: the HTTP performance test created excessive connections across repeated process-local servers. Its transport now explicitly reuses and caps 32 connections. The latest 100-run targeted stress rerun passed with a 10-minute timeout: `internal/chathub` 21.133s, `internal/outbound` 5.271s, and `internal/web` 18.893s, without weakening assertions.
- [ ] Verify WebSocket reuse, slow consumers, cancellation propagation, server close, frame-channel close, backpressure, and goroutine steady state. Corrected frame-channel close test passed 20 consecutive runs; connection-pool, transport, cancellation, protocol, session, tool, overload, and isolation-targeted tests passed 100 consecutive runs with a 10-minute timeout. ConnPool close is now idempotent and its close/slow-consumer tests passed 100 consecutive runs. Connection-pool lifecycle and active WebSocket goroutine steady-state tests each passed 20 consecutive runs with a 2-minute timeout; remaining matrix items are not yet fully verified.
- [ ] Verify slow write and real WebSocket slow-consumer behavior. TCP reset and slow read passed 20 consecutive targeted runs; half-open connection, EOF, dial failure, TLS handshake failure, timeout, connection-pool exhaustion, idle close, and recovery also passed 20 consecutive targeted runs, all with explicit timeouts.
- [x] Verify HTTP/1.1 connection reuse and HTTP/2 multiplexing with measurable connection counts; passed 20 consecutive targeted runs.

## Isolation

- [ ] Verify isolation between clients sharing one API key across concurrent requests, sessions, conversation IDs, response frames, tool state, and buffers. Tenant/session response namespaces, request-local session headers, and request-local function/custom tool output call IDs passed 20 consecutive runs and 100 concurrent-stress repetitions; full response-frame, conversation, and buffer isolation remains pending.
- [ ] Verify Chat Completions and Responses protocol state cannot cross request or client boundaries.

## Circuit Breaking And Overload

- [x] Verify half-open admits one concurrent probe in the current targeted test suite.
- [x] Verify a successful probe restores availability in the current targeted test suite.
- [x] Verify a repeated 429 re-enters cooldown in the current targeted test suite.
- [ ] Verify repeated 429 preserves the existing configured cooldown duration in all paths.
- [x] Verify non-idempotent POST requests are not replayed in the current targeted test suite.
- [x] Verify local concurrency overload returns 503 rather than an upstream-style 429 in the current targeted test suite.

## Protocol And Product Coverage

- [x] Verify Chat Completions tool calls, tool results, usage, streaming, cancellation, and error mapping. The targeted protocol, MCP, tool, call ID, streaming, usage, cancellation, error, conversation, and isolation suite passed 20 consecutive runs in 6.041s.
- [x] Verify Responses tool calls, tool results, usage, streaming, cancellation, and error mapping. The same targeted suite passed 20 consecutive runs; the expected inner-request failure path emitted `response.created` followed by `response.failed`, never `response.completed`.
- [ ] Verify WebUI network errors, recovery messages, accessibility, and i18n. The two WebUI copies are byte-identical (SHA-256 `15135CB6CF1B2B5EB0AE8F6276E69E7F17B5EB100A1556BB93BFBD55809D574A`), but end-to-end UX, accessibility, and localization remain unverified.
- [c] Codex、OpenCode、Claude Code 三客户端完整复杂任务按用户要求取消；不执行、不作为发布阻塞，且不得标记为已验证完成。

## Performance Evidence

- [ ] Measure throughput, P50, P95, P99, failure recovery, maximum concurrency, allocations, and goroutine steady state. The WebSocket pool-hit performance test now uses 100 requests per run to avoid exhausting Windows ephemeral ports and passed 100 consecutive runs in 6.416s. Its metric is explicitly named `pool_hit_rate` because each request still creates, takes, and closes a freshly warmed connection; it does not prove cross-request connection reuse.
- [-] Cross-request WebSocket reuse is not applicable to continued conversations. `internal/chathub/client.go:392-397` permits a pre-warmed connection only for a fresh request without conversation or session IDs, while `internal/chathub/connpool_reuse_test.go:20-27` records that continuing an upstream conversation requires a fresh WebSocket to avoid an immediate empty completion. Do not weaken this protocol boundary to improve reuse metrics.
- [ ] Record the complete reproducible baseline and any after-change comparison. Latest five-run microbenchmarks: `BenchmarkParseRetryAfterSeconds` 10.74-11.25 ns/op, 0 B/op, 0 allocs/op; `BenchmarkAccountHealthTryAcquire` 41.32-41.68 ns/op, 0 B/op, 0 allocs/op; `BenchmarkServerConcurrencyLimit` 3455-3669 ns/op, 5376-5377 B/op, 15 allocs/op. The end-to-end loopback HTTP concurrency benchmark measured 92847-96791 ns/op, 6335-6396 B/op, and 73 allocs/op across five runs (Windows amd64, Intel Xeon E3-1226 v3). The corrected HTTP latency and concurrency test passed 20 consecutive runs with 1000 requests and concurrency 32; observed samples include throughput 4708.70-5726.81 req/s, P50 1.9710-2.4764 ms, P95 15.9998-20.7473 ms, P99 18.9591-25.1274 ms, and 32 simultaneously active handlers. The renamed `BenchmarkConnPoolWebSocketPoolHit` measured 1446279 ns/op, 38878 B/op, and 153 allocs/op over 100 iterations. A representative pool-hit run measured 669.86 req/s, P50 1.3522ms, P95 4.8807ms, P99 62.7633ms, maximum active 2, and pool hit rate 100%; the test passed 100 consecutive runs in 6.452s. This is pool-hit evidence only; completed requests close their WebSockets, and continued conversations intentionally require fresh connections.
- [ ] Make only a minimal optimization if measurements identify a bottleneck.
- [x] Do not introduce Radix Tree, HTTP/3, a custom HTTP stack, `sync.Pool`, `unsafe`, or sensitive-slice pooling without evidence.

## Responses Protocol Compatibility

- [ ] P0 trace Responses request conversion and upstream event sources without altering existing or unknown work.
- [ ] P0 support `instructions`, `max_output_tokens`, `parallel_tool_calls`, `tool_choice`, `reasoning`, `include`, `temperature`, `text`, `service_tier`, `context_management`, and `previous_response_id`; reject unsafe unsupported parameters with `unsupported_parameter`.
- [ ] P0 preserve per-turn highest-priority instructions without inheriting stale instructions through `previous_response_id`.
- [ ] P0 fix `function_call_output`/`call_id`, `response.failed`, premature stream disconnects, UTF-8 chunk corruption, path/link damage, and deterministic dual-source event deduplication/completion.
- [ ] P0 use only the reverse-engineered official MCP/native tool interface while preserving third-party high-priority instructions, model context, and tool-result correlation.
- [ ] P0 implement a tool-state ledger for in-progress, awaiting-result, completed, and final-response states; prevent fabricated completion and premature summaries.

## Administration And Security

- [ ] P1 implement API-key-to-account bindings, persistence, scheduling filters, failover, WebUI management, and unbound-key healthy-account load balancing.
- [ ] P1 expose existing settings safely with encrypted persistence, masked secrets, administrator authorization, and CSRF protection.
- [ ] P1 complete batch Microsoft 365 user UX, encrypted initial-password storage, administrator-only view/export, secure clearing, auditing, license availability display, and actionable master-key initialization.
- [ ] P1 synchronize `web/index.html` and `internal/web/web/index.html` without overwriting unknown changes.
- [ ] P1 complete usage passthrough and dashboard accounting semantics.

## Required Regression Coverage

- [ ] Add tests for request semantics, unsupported parameters, per-turn instructions, previous response handling, dual-source tool events, UTF-8 chunks, paths/links, call IDs, failures, and tool-loop completion.
- [ ] Add tests for API-key account bindings, settings persistence, password encryption, authorization, CSRF, and byte-identical frontend copies.

## Quality Gates

- [x] Run `gofmt` on changed Go files, including the added connection-pool steady-state test.
- [x] Rerun `go test ./... -count=1 -timeout=10m` after the final test edits. Latest full rerun passed with `GOROOT=D:\Go`; `internal/auth` 0.751s, `internal/chathub` 1.285s, `internal/mcp` 1.258s, `internal/outbound` 0.919s, and `internal/web` 6.994s.
- [x] Rerun `go vet ./...` after fixing the Windows ephemeral-port stress-test design.
- [x] Rerun `go build ./...` after fixing the Windows ephemeral-port stress-test design.
- [x] Rerun `git diff --check` after fixing the Windows ephemeral-port stress-test design. Latest rerun passed with line-ending conversion warnings only; `web/index.html` and `internal/web/web/index.html` were byte-identical.
- [c] Race 检测按用户要求取消；不执行、不作为发布阻塞，且不得标记为已验证完成或已通过。
- [-] Complete independent security, concurrency-risk, protocol, cross-request leakage, denial-of-service, and authorization-boundary review. Independent read-only Agent A is unavailable in the current tool environment, so no independent review conclusion is claimed.
- [-] Complete independent performance-evidence, over-design, test-realism, WebUI UX, and i18n review. Independent read-only Agent B is unavailable in the current tool environment, so no independent review conclusion is claimed.
- [ ] Fix every Critical and High review finding, then rerun every gate.
- [ ] Run targeted tests at least 20 consecutive times with `GOROOT=D:\Go` and the matching Go `PATH`.
- [ ] Run `go test ./... -count=1 -timeout=10m`, `go vet ./...`, `go build ./...`, and `git diff --check` after final edits.
- [ ] Rebuild and restart only the development service on port 4242; verify the home page and critical APIs without touching port 4141.
- [ ] Report root causes, changed files with line numbers, test evidence, unsupported parameters, and manual verification steps without committing or publishing.

## Version And Release Gate

- [ ] Verify version metadata and release prerequisites.
- [c] Codex、OpenCode、Claude Code 三客户端复杂任务按用户要求取消；不执行、不作为发布阻塞，且不得标记为已验证完成。
- [-] Graph 真实租户授权仍受外部凭据阻塞，因为缺少 `M365_GRAPH_CLIENT_SECRET` 和 `M365_GRAPH_TENANT_ID`；不得宣称真实租户授权已通过。
- [ ] Keep release blocked until every required gate and real-client check has evidence.
- [x] Do not deploy, commit, push, tag, release, touch `D:\M365-Copilot2API`, use port 4141, or change backend cooldown duration.
