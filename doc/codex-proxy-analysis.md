# Codex 代理安全性分析

> 分析日期：2026-04-20

## 部署情况

- 服务：`cliproxyapi`，单实例，监听 `8317` 端口
- 部署脚本：`./server-build.sh`（编译 + 带时间戳部署 + 重启 + 健康检查 + 自动回滚）
- Auth 文件目录：`/home/iec/deploy/auths/`
- 导入脚本：`scripts/import_codex_ios_tokens.py`

---

## Codex 账号状态（2026-04-20）

| 到期时间 | 数量 | 备注 |
|---------|------|------|
| 2026-04-27 | 43 个 | 最早一批，需优先续费 |
| 2026-04-28 | 15 个 | |
| 2026-04-30 | 21 个 | 含 codex.txt + codex2.txt 新导入的 20 个 |
| **合计** | **79 个** | |

---

## 请求重构机制分析

### 结论：代理完全重构请求，不透明转发

**User-Agent**（`codex_executor.go:33`）

硬编码常量：
```
codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)
```
优先级：config 配置 > 原始客户端 UA > 硬编码常量。

> ⚠️ 注意：真实客户端已升至 `0.121.0`，硬编码常量偏旧，建议同步更新。

**请求 Body**（`codex_executor.go:104-119`）

原始 body 经 `sdktranslator.TranslateRequest()` 完整翻译重构，不直接透传。

主要处理：
- `model` 替换为 baseModel
- `stream` 强制为 `true`
- 删除字段：`previous_response_id`、`prompt_cache_retention`、`safety_identifier`、`stream_options`
- 注入 `session_id`、cache key

**Headers**

| 类型 | 处理方式 |
|------|---------|
| `Authorization` | 替换为账号自身 token |
| `User-Agent` | 按优先级重构 |
| `Content-Type` / `Accept` / `Connection` | 固定值 |
| `X-Codex-Turn-Metadata`、`X-Client-Request-Id` 等 | 从原始请求透传 |

---

## 安全风险评估

### 透明代理 vs 重构代理

| 维度 | 透明代理 | 重构代理（当前） |
|------|---------|--------------|
| 客户端指纹暴露 | 高风险 | 低风险 |
| 异常字段泄露 | 高风险 | 低风险（主动删除非标字段） |
| 批量行为特征 | 高风险 | 低风险 |
| 账号关联风险 | 高风险 | 低风险 |

### 多账号轮询识别风险

| 维度 | 风险等级 | 说明 |
|------|---------|------|
| IP 关联 | 高 | 多账号同 IP，最显著信号 |
| 请求时序规律 | 低-中 | 轮询模式有规律，但工具型请求本身就是短会话 |
| 会话特征 | 低 | 单次无上下文请求在开发者场景中很常见 |
| 账号注册特征 | 已存在 | 批量注册账号在注册时可能已打标签，与运行时行为无关 |

**最大风险点**：IP 维度的账号关联，而非单请求内容特征。

---

## 抓包验证（真实请求示例）

请求头关键字段：
```
User-Agent: codex-tui/0.121.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.8
X-Codex-Turn-Metadata: {"session_id":"...","thread_source":"user",...}
X-Client-Request-Id: 019da92e-51e8-7b30-b84a-8c7c228a6f87
Originator: codex-tui
Content-Length: 166071
```

Body 结构：`model: gpt-5.4` + 完整 `instructions` 系统 prompt + 多轮 `input` 历史（含加密 reasoning 块）

代理转发后：系统 prompt 映射到 Codex API 字段，历史消息翻译，加密 reasoning 块删除，model 替换，OpenAI 收到标准 codex-tui 请求。
