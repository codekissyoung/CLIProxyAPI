# 多人共用 Pro 账号：Codex CLI 0.153.4 上游身份收敛

## 目标

`ice` 分支把同一 OAuth 账号的上游请求收敛为一台 Ubuntu x86_64 主机上的
Codex CLI 0.153.4 TUI，而不是暴露各下游客户端的系统、版本和传输栈差异。
这只改变上游可见身份，不改变账号选择、会话黏性、计费或响应翻译。

当前固定应用身份为：

- `User-Agent: codex-tui/0.153.4 (Ubuntu 22.4.0; x86_64) vscode/1.135.0 (codex-tui; 0.153.4)`
- `Originator: codex-tui`
- 缺失时补 `Version: 0.153.4`
- 缺失时补 `X-Codex-Beta-Features: remote_compaction_v2`

> 2026-09-05：应用层身份从 0.147.0 升到 0.153.4。OpenAI 后端对
> `gpt-6-astra` 按上游自报版本硬 400（"requires a newer version of Codex"），
> 0.147.0 身份全部命中；sub2api 同步官方最新稳定版（GitHub `openai/codex`
> release）可正常服务，证明门槛在应用层版本字串。TLS ClientHello 沿用
> 0.147.0 捕获基线未换——reqwest/OpenSSL 与 rustls 栈在 0.147→0.153
> 之间无变化，且该门控与 TLS 指纹无关。
>
> 2026-09-09：UA 平台/终端段从 `(Ubuntu 24.4.0; x86_64) dumb` 改为
> `(Ubuntu 22.4.0; x86_64) vscode/1.135.0`。依据 `usage_logs.user_agent`
> 近 14 天真实 codex-tui 0.153.4 流量，`TERM=dumb` 在真实种群中不存在
> （服务器/CI 特征），该字符串是观测到请求量最高的真实 Linux 0.153.4 UA；
> 保持 Linux 平台与 reqwest/OpenSSL ClientHello 基线自洽。

OAuth 请求默认强制使用这一组身份；管理员显式设置
`codex-header-defaults.user-agent` 或开启 `disable-codex-cloaking` 时仍按配置处理。
API-key 路径继续保留调用方显式身份，但绝不允许空 UA 退化成 Go 默认 UA。

`X-Codex-Turn-Metadata.workspaces` 仍会在 HTTP 和 WebSocket 路径剥离，避免把
本地路径、Git 远端和 commit 信息带到共享账号上游。其余会话字段保持透传。

> 2026-09-09：身份面加固（详见 `docs/ice-divergences.md` 分歧 #11）。
> body 镜像的 `client_metadata.x-codex-turn-metadata` 现在同样剥离 workspaces
> （此前只剥 header 版）；`client_metadata.ws_request_header_*` 的身份/会话头镜像
> 被删除；WS 握手不再向上游透传 `x-codex-turn-state`；turn-metadata 内的
> `session_id`/`thread_id` 与顶层 `session_id`/`conversation` 纳入按账号混淆；
> 代理生成的 `prompt_cache_key`/`Session-Id` 一律按账号派生，不再出现跨账号同值；
> `identity-confuse` 不再以路由策略为前提（防止路由改动静默关闭混淆）；
> `X-Client-Request-Id` 恢复真实客户端的每请求 UUID 语义（透传或新生成），
> 不再被覆写为会话常量；各路径返回给客户端的错误体先做混淆值反向还原。

## 0.147.0 传输基线

2026-08-15 使用本机官方 Codex CLI 0.147.0 二进制分别捕获 HTTPS fallback、
Responses WebSocket 和 TUI/exec 请求头。抓包只使用 dummy Bearer；临时 pcap、
TLS key log 和探针文件验证后已删除。

### HTTPS Responses

- 传输：HTTP/1.1；ClientHello 不声明 ALPN
- TLS 实现特征：reqwest/OpenSSL
- JA3：`0b85eb0d4981e69064e40753e4f0ac5f`
- JA4：`t13d301100_1d37bd780c83_8e6e362c5eac`
- 扩展顺序固定：`65281,0,11,10,35,22,23,13,43,45,51`
- supported groups：`4588,29,23,30,24,25,256,257`
- key shares：`4588,29`
- HTTP/1.1 请求头按真实 0.147.0 次序写出；连接池按账号和有效代理隔离

### Responses WebSocket

- TLS 实现特征：rustls 0.23 + AWS-LC
- 稳定 JA4：`t13d101000_61a7ad8aa9b6_f9531d972513`
- cipher 顺序：`4866,4865,4867,49196,49195,52393,49200,49199,52392,255`
- 扩展集合：`0,5,10,11,13,23,35,43,45,51`
- supported groups：`4588,29,23,24`
- key shares：`4588,29`
- 不声明 ALPN，使用 HTTP/1.1 Upgrade
- rustls 每次连接随机扩展顺序，因此 JA3 可变而 JA4 稳定；代理端保持同样行为

上述基线由离线 ClientHello 回归测试锁定，测试不连接第三方指纹服务。

## IPv4 是独立的生产安全约束

Codex CLI 指纹模拟不等于照搬本机 Happy Eyeballs。所有 Codex HTTPS 和 Responses
WebSocket 直连仍把 `tcp` 改写为 `tcp4`，因为当前 VPS IPv6 出口更容易触发
Cloudflare 风控。显式 `direct` 同样必须走 IPv4；任何自定义拨号器都必须实现
`proxy.ContextDialer`，保证请求取消能中止 TCP 和 TLS 握手。

不要为了更像原生 CLI 恢复 IPv6。只有在独立验证 IPv6 出口信誉、Cloudflare
错误率和回滚路径后，才可重新评估这一约束。

## 保持不变的会话语义

- `prompt_cache_key` 保持客户端 UUID；不能为了指纹统一而全局清空。
- `session_id`、`thread_id`、`turn_id` 和 `X-Client-Request-Id` 保持透传或沿用现有
  生成逻辑，避免破坏上下文连续性。
- `Chatgpt-Account-Id` 继续来自当前 OAuth auth metadata，不能跨账号覆写。
- 连接池必须以 `auth.ID + effectiveProxyURL` 隔离；不能做跨账号共享连接池。
- 账号级并发和 QPS 是业务层多人信号，TLS/UA 收敛不能替代并发治理。

## 验证要求

改动上游传输后至少完成：

1. 离线 HTTP 与 WebSocket ClientHello 回归测试。
2. 拨号和 TLS handshake 的 context cancellation 回归测试。
3. `go test ./...`、`go vet ./...` 和必需的 `cmd/server` 编译。
4. 生产发布后使用测试账号执行一次 pinned Codex Responses 请求；普通 healthz
   不能覆盖 `chatgpt.com` 的自定义 uTLS 路径。
5. 观察 Codex 上游 401/403/429、Cloudflare 1010/1020 和 WebSocket 握手失败率。

## 不要做的事

- 不要透传 `X-Forwarded-*` 或扩大 Codex header 白名单而不先审计身份泄露。
- 不要全局删除 `X-Codex-Turn-Metadata`；只删除 `workspaces`。
- 不要禁用 `prompt_cache_key`。
- 不要把 UA、Originator、Version 拆成不同客户端版本；它们必须成套更新。
- 不要用 Chrome/HTTP2 profile 替代当前捕获基线；Codex CLI 0.147.0 的 HTTPS
  fallback 是 OpenSSL 风格 HTTP/1.1，WebSocket 才是 rustls 风格。
