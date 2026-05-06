# 多人共用 Pro 账号 — 反指纹优化清单

## 背景

CPA 在 ice 分支被用作"小组 N 个组员共用一个 OpenAI Pro 账号"的中转。
当前已落地的反指纹改动：

- `User-Agent` 兜底 `codex-tui/0.128.0 (Mac OS 14.5.0; arm64) iTerm.app/3.6.9 (codex-tui; 0.128.0)` —— 与真实 codex CLI 完全一致
- `X-Codex-Turn-Metadata.workspaces` 字段在 `applyCodexHeaders` / `applyCodexWebsocketHeaders` 里被剥离 —— 防止 git 仓库 / 本地路径 / commit hash 泄露多人特征
- 非 macOS 客户端 UA 强制覆写为兜底 `codexUserAgent`，并联动覆写 `Originator` 为 `codex-tui` —— 防止"同账号、多种 OS"信号泄露。仅在 OAuth 路径生效（API-key 路径不变），且尊重管理员显式配置的 `codex-header-defaults.user-agent`

只支持 `/v1/responses` 路径（路径 A），其它 codex 路径暂不需要考虑。

## 已确认无问题的项

- `prompt_cache_key` 透传客户端原值（codex CLI 自己生成 UUIDv7），N 人 × M 会话看起来等同于"单人多会话"，不暴露多人特征
- 出口 IP 单一（ice-server 一个 IP）反而比真客户端的 N 个 IP 更像"单用户固定地点"
- session_id / turn_id 由客户端生成 UUIDv7，CPA 透传，符合真实客户端行为

## 待办优化（按 ROI 排序）

### 1. TLS / HTTP/2 指纹（中-高优先级）

**问题**：CPA 用 Go 默认 `crypto/tls`，而真实 codex CLI 是 Rust（reqwest+rustls）。如果 OpenAI 后端做 JA3/JA4 校验，**所有**走 CPA 的请求会被识别为"非 codex CLI 客户端"。即使没风控，也会留下"该账号所有会话都来自同一个非官方 TLS 指纹"的奇怪信号。

**调研要点**：
- `internal/auth/claude/utls_transport.go` 里有 utls 相关代码，给 Claude 用了；codex 路径走的是 `internal/runtime/executor/codex_executor.go:codexHTTPClient` → `buildCodexTransport`，是不是用的标准 `*http.Transport`？
- `internal/runtime/executor/codex_websockets_executor.go:newProxyAwareWebsocketDialer` 走 gorilla/websocket，TLS 也是标准库
- 真实 codex CLI 的 ClientHello 长什么样（可以在 Mac 上用 `tcpdump` + `tshark -r dump.pcap -Y tls.handshake.type==1 -V` 抓一份做基准）

**改造方向**：
- 引入 `refraction-networking/utls`，模拟 reqwest 的 ClientHello（HelloChrome_xxx 还是 HelloIOS_xxx，需要对比真实抓包决定）
- 同时处理 HTTP/2 SETTINGS 帧顺序、ALPN 协商，这些也是 JA4 的输入

**风险**：utls 跟 net/http transport 集成有一些坑（H2 frame 顺序、cipher suite 协商失败回退），需要详细测试。

### ~~2. `User-Agent` 跨平台一致性~~（已落地）

非 macOS 客户端 UA 在 `applyCodexHeaders` / `applyCodexWebsocketHeaders` 里被强制覆写为 `codexUserAgent`，同时 `Originator` 联动覆写为 `codex-tui`，避免 UA/Originator 错配指纹。仅 OAuth 路径生效；管理员显式配置 `codex-header-defaults.user-agent` 时跳过强制（视为管理员明确意图）。详见下方"已落地"表格。

### 3. `X-Codex-Window-Id` / `X-Client-Request-Id` 时间戳防关联（低-中优先级）

**问题**：这两个字段都是 UUIDv7，前 48 位是毫秒时间戳。多组员的请求时间戳分布可以做行为指纹（比如全天均匀分布 vs 单人常见的间断模式）。

**当前状态**：透传客户端原值。

**改造方向**：保留 UUIDv7 格式（不能换 v4 v5，会被识别），但**不依赖客户端时间戳** —— CPA 自己生成新的 UUIDv7（基于服务器时间）。这样所有请求时间戳来自同一时区/同一时钟。

**优先级低的原因**：单看这一项不构成强信号；只在 1+2 都做完后才有边际收益。

### 4. `Authorization` Bearer token 形态（调研项，可能不是问题）

**问题**：你 Mac 抓包里看到的是 `Authorization: Bearer sk_5f9f49...`（看起来像短期 access token），跟真实 OAuth flow 一致。但 CPA 用的是后台保存的 OAuth token + 自动刷新，token 形态可能跟客户端实时刷新的有微小差异（比如 sub claim、scope 字段）。

**调研要点**：
- 解码 CPA 当前持有的 access token 看 JWT payload
- 跟真实客户端抓包的 token 比对 claim 结构

**优先级**：低。OpenAI 自己发的 token，他们当然能验签，不太可能用 token 内容反推"是 CPA 而不是真客户端"。仅作完整性记录。

### 5. 业务层并发限速（业务侧，非 CPA 代码层）

**问题**：N 人同时用 → 同账号峰值 QPS 远超单人。这是结构性的多人信号，CPA 无法在协议层伪装。

**改造方向**（不在本仓库）：
- 加入站点级速率限制器，控制同账号并发数和分钟级 QPS 不超过 OpenAI 单用户合理上限
- 错峰调度：高优组员用主账号，低优组员路由到 Free 账号
- 监控 `/v1/responses` 上游 429/capacity 错误率，超阈值自动降速

## 验证手段（每项改完都该做）

1. **本地转发抓包**：`tcpdump` 抓 ice-server 出口流量，`tshark` 解 TLS 解 HTTP/2，对比 CPA 转发的请求与真实 macOS 抓包的 codex CLI 请求。逐字段对齐。

2. **Burp / mitmproxy 中间人**：临时把 CPA 配置成走 mitmproxy 代理，看上游收到的明文请求；同时让 Mac 客户端走同一个 mitmproxy 直连 OpenAI，做侧对比。

3. **错误率监控**：每次改动后看 `upstream provider error provider=codex` 的频率和具体 status code。临时开 `debug: true` 能看到 `request error, error status: XXX`。

## 生产验证记录（#2 反指纹覆写）

2026-05-07 在 ice-server 临时打开 `request-log: true`、`debug: true`、`commercial-mode: false`，对 7 笔真实 `/v1/responses` 流量取上下游 header 对照：

- 6 笔 macOS 客户端（UA 含 `Mac OS`）：上游 UA / Originator **原样透传**，未触发覆写分支
- 1 笔非 macOS 客户端（下游 UA = `Codex-CLI/1.0`）：上游 UA 被覆写为 `codex-tui/0.128.0 (Mac OS 14.5.0; arm64) iTerm.app/3.6.9 (codex-tui; 0.128.0)`，Originator 联动设为 `codex-tui` ✅

每条上游 header 也确认带上了**对应组员**的 `Chatgpt-Account-Id`（不同请求不同账号 ID），证明账号路由没被覆写影响。

per-request 日志在 `/home/iec/deploy/auths/logs/v1-responses-*.log`，每条约 1.5 MB；后续观察期靠 cron 每小时清理 24h 前的文件控盘。

## 已落地的实现参考

| 项 | 文件 | 函数 | Commit |
|---|---|---|---|
| UA 兜底 | `internal/runtime/executor/codex_executor.go:35` | `codexUserAgent` 常量 | merge `983bc082` |
| workspaces 剥离 | `internal/runtime/executor/codex_executor.go` | `stripCodexTurnMetadataWorkspaces` | `acb58bd5` |
| 非 macOS UA 强制覆写 + Originator 同步 | `internal/runtime/executor/codex_executor.go`、`codex_websockets_executor.go` | `applyCodexHeaders` / `applyCodexWebsocketHeaders` | （本次） |

## 不要做的事

- **不要透传 `X-Forwarded-*` 头**：当前 codex 路径是白名单透传（不在白名单的 header 不会上传），已经天然 scrub。任何"扩大透传范围"的改动都要先评估是否引入新指纹。
- **不要全局清空 `X-Codex-Turn-Metadata`**：上游可能依赖 `session_id` / `turn_id` / `sandbox` 做会话连贯性。只剥 `workspaces`。
- **不要禁用 prompt_cache_key 透传**：影响真实客户端的 cache 命中体验，且 UUIDv7 透传本身不是多人指纹。
- **不要把所有组员的 originator 强制设成 `codex-tui`**：如果客户端真的是 Codex Desktop，强制改回 codex-tui 反而会跟 UA 冲突（UA 是 Desktop / Originator 是 codex-tui 这种组合在真实流量里不存在，是更强的指纹）。让 UA 和 Originator 配套覆写或都不动。

## 推荐执行顺序

1. **先做 #1 TLS 指纹调研**（不改代码，只看真实 codex CLI 的 ClientHello / HTTP/2 SETTINGS 长什么样、CPA 现在发的长什么样，差距多大）
2. 根据调研结果决定是上 utls（差距大）还是不动（差距小或 OpenAI 不校验）
3. 然后做 #2 UA 跨平台一致性（小改动、易验证）
4. 再视情况做 #3
5. #4、#5 长期看
