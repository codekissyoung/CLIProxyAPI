# CLIProxyAPI 优化排查纪要（2026-04-24）

> 目的：把这次在生产环境里已经确认的问题、证据、优先级和落地建议整理出来，后续可以直接和 Claude Code 一起过。
>
> 范围：`cliproxyapi` 主服务、`cliproxy-profiler` 监控抓样结果、Codex `/v1/responses` 路径。

---

## 1. 本次先说结论

当前线上监控看到的 CPU 抖高 **不是 profiler 自己造成的假象**，而是 `cliproxyapi` 在处理 Codex OpenAI Responses 请求时，确实存在比较重的 JSON 翻译和重复转换开销。

本轮排查确认了 3 类问题：

1. **真实 CPU 峰值存在，而且频率不低**
2. **核心热点在 Codex Responses 请求翻译链路，不是死锁、协程泄漏、线程暴涨**
3. **部署重启间隔偏长的主要原因是单实例优雅退出，不是之前那个 shutdown bug**

另外，之前已经确认并修复过一个独立 bug：

- **shutdown 复用启动时创建的 30s context，导致长时间运行后退出时 context 已过期**
- 这个 bug 已修，且已在生产环境二次重启验证通过

---

## 2. 监控里已经看到的异常

### 2.1 CPU spike 确认是真实异常

`cliproxy-profiler` 已连续抓到多次高 CPU 样本目录：

- `20260423-235746-pid115448-burst-107.7`
- `20260424-000750-pid115448-burst-127.0`
- `20260424-001333-pid115448-burst-128.2`
- `20260424-002353-pid115448-burst-101.5`
- `20260424-004351-pid370005-burst-105.5`

`journalctl -u cliproxy-profiler` 里还能看到大量被 cooldown 限流跳过的 spike，例如：

- `2026-04-24 00:11:34`：`cpu=220.86%`
- `2026-04-24 00:14:05`：`cpu=149.44%`
- `2026-04-24 00:24:03`：再次成功抓样

这说明：

- 线上 spike **不是偶发一次**
- spike **不止刚过阈值**，有时会冲到双核以上
- profiler 的 cooldown 只是防止过度抓样，不代表问题消失

### 2.2 spike 发生时，线上本身就有较多长请求

以 `20260424-004351-pid370005-burst-105.5` 这个样本为例：

- `ss-summary.txt`：`TCP estab 231`
- request log tail 中有大量 `/v1/responses`
- 多个请求耗时在 `10s ~ 45s`
- 个别请求到 `1m3s`、`1m44s`

结论：

- 线上确实存在较高并发和长请求
- CPU spike 发生时不是空载环境
- 所以这次优化要优先考虑**每次请求的固定 CPU 成本**，而不是只看连接数

---

## 3. 已经确认的核心性能热点

样本文件：

- 二进制：`/home/iec/deploy/bin/cliproxyapi.20260424-003522`
- 目录：`/home/iec/deploy/log/cliproxy-profiler/20260424-004351-pid370005-burst-105.5`

### 3.1 CPU pprof 结论

`go tool pprof -top` 看到的主要热点：

- `github.com/tidwall/gjson.parseSquash`：`0.99s / 1.55s`，约 `63.87%`
- `github.com/tidwall/gjson.Get`：累计约 `1.13s / 1.55s`，约 `72.90%`

这已经很明确地指向：

- 不是系统调用本身最重
- 不是锁竞争最重
- 而是 **JSON 反复扫描 / 反复路径读取** 最重

### 3.2 行级热点已经定位到具体代码

`go tool pprof -list convertSystemRoleToDeveloper`：

热点在：

- 文件：`internal/translator/codex/openai/responses/codex_openai-responses_request.go`
- 函数：`convertSystemRoleToDeveloper(...)`
- 最热行：

```go
if gjson.GetBytes(result, rolePath).String() == "system" {
```

线上样本里，这一行单点就吃掉了约：

- `1.03s / 1.55s`（本次最新样本）

根因很清楚：

- 代码先拿到 `inputArray := inputResult.Array()`
- 但循环里又继续对整份 `result` 做 `gjson.GetBytes(result, "input.%d.role")`
- 每次都走一次新的路径扫描
- 如果消息数组很长，请求体很大，这个开销会被放大得非常明显

### 3.3 堆内存热点说明有明显复制/重写放大

`go tool pprof -top -inuse_space` 看到：

- `github.com/tidwall/sjson.appendRawPaths`：`42.95%`
- `io.ReadAll`：`19.45%`
- `responsesSSEFramer.WriteChunk`
- `net/http.(*http2Framer).startWriteDataPadded`

说明：

1. **请求/响应 JSON 的重写分配非常重**
2. **上游 body 的整块读取仍然带来不少额外内存占用**
3. SSE 输出本身有开销，但不是这次最先该动的点

---

## 4. 已确认的代码层问题清单

下面按“优先级 + 影响面”整理。

### P0-1：Codex executor 对同一请求做了两次翻译

文件：`internal/runtime/executor/codex_executor.go`

现状：

```go
originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, ...)
body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, ...)
```

问题点：

- 在常见路径下，`opts.OriginalRequest` 和 `req.Payload` 实际上就是同一份原始请求
- 但现在会做两次 `TranslateRequest(...)`
- 对大 body、长 `input`、多轮 `responses` 请求来说，这就是**直接翻倍的翻译成本**

为什么我认为这条要先做：

- 修改范围较小
- 收益直接
- 不需要先大改 translator 架构
- 当前生产配置里未看到 `payload:` 配置块，说明“为了 payload rule 保留一份原文”这条路径在现在的线上收益很低

建议：

- 先判断 `req.Payload` 与 `opts.OriginalRequest` 是否等价/同源
- 相同就复用一次翻译结果
- 只有在确实需要保留“原始请求”和“改写后请求”两份语义时，才走双翻译

### P0-2：`convertSystemRoleToDeveloper` 是当前最重 CPU 热点

文件：`internal/translator/codex/openai/responses/codex_openai-responses_request.go`

现状问题：

- 先解析了一次 `input`
- 后面又在循环里基于字符串 path 对整份 JSON 重扫
- 同时还伴随 `sjson.SetBytes(...)` 的逐次改写

本质上这是一个：

- **对大 JSON 做 repeated scan + repeated rewrite** 的实现

建议方向：

1. **不要在循环里再 `gjson.GetBytes(result, path)`**
2. 直接使用已经拿到的 `inputArray` 判定哪些 index 的 role 是 `system`
3. 尽量收集完需要修改的位置后，再做更少次数的写回
4. 更进一步可以考虑把 `input` 局部重建后一次性 `SetRawBytes`

这是当前最明确、最值得优先动手的热点。

### P1-1：`normalizeCodexBuiltinTools` 也存在同类 repeated scan / rewrite 问题

文件同上。

现状：

- 遍历 `tools`
- 每个元素都动态拼 path，再 `gjson.GetBytes` / `sjson.SetBytes`
- `tool_choice.type`、`tool_choice.tools.*.type` 也重复走同样逻辑

这个函数通常没有 `convertSystemRoleToDeveloper` 那么热，但问题模式相同：

- 多次路径解析
- 多次字符串拼接
- 多次 JSON 重写

建议：

- 如果这轮要顺手优化 translator，最好一起收掉
- 至少应避免“已经从数组拿到对象后，还回头按 path 全文再查一次”的做法

### P1-2：请求体越大，当前翻译链路放大越明显

本地做过一轮合成压测，当前 translator 的平均耗时大致是：

- small `21688` bytes：`~535µs`
- medium `63290` bytes：`~1.77ms`
- large `309720` bytes：`~17.14ms`
- xlarge `616212` bytes：`~49.95ms`

这个曲线说明：

- 不是简单线性增长
- body 变大以后，性能退化明显
- 和线上 pprof 看到的 repeated scan / repeated rewrite 是对得上的

### P1-3：内存分配偏重，容易放大 GC 压力

证据：heap pprof 中 `sjson.appendRawPaths` 占比最高。

意味着：

- 只要走大量 JSON path 改写，内存分配就会持续增加
- 在并发较高时，CPU 不只花在业务逻辑，还会花在分配和 GC 上

这也是为什么单次请求看起来“只是几毫秒”，但线上并发起来后会被放大成 burst。

### P2-1：Codex User-Agent 常量偏旧

文件：`internal/runtime/executor/codex_executor.go`

当前硬编码：

```go
codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)
```

之前真实抓包里看到的客户端版本已经到 `0.121.0`。

这不是当前 CPU 的主因，但它属于一个顺手应该修的小问题：

- 指纹与真实客户端版本略有偏差
- 不影响功能，但有一致性风险

---

## 5. 非性能类但这次一起确认过的问题

### 5.1 shutdown bug（已修复，可不再重复排查）

问题：

- `sdk/cliproxy/service.go` 在服务启动时就创建了 30s shutdown context
- 退出时复用它
- 服务运行久了以后，这个 context 其实早就过期
- 所以退出时会报 `context deadline exceeded`

现状：

- 已改成在真正 shutdown 时再创建新的 graceful context
- `sdk/cliproxy/pprof_server.go` 也同步复用同一套 helper
- 已补测试：`sdk/cliproxy/service_shutdown_test.go`
- 已在生产环境二次重启验证通过

结论：

- 这个问题已经关闭
- 后续文档评审时只需要作为“已修复背景”保留，不需要再重复投入

### 5.2 单实例部署下，重启期间存在天然短暂空窗

这次观察到关闭到恢复健康有大约十几秒量级的间隔，根因不是新 bug，而是：

- 单实例服务
- 需要等正在处理的长请求优雅退出
- `/v1/responses` 本身存在 10s、20s、40s 级别请求

所以：

- 如果继续保持单实例 + 原地重启，就一定有窗口期
- 当前更合理的方向是**保留优雅退出**，不要为了追求秒级重启去粗暴缩短 timeout
- 若后面真要进一步压缩发布空窗，更适合做 **蓝绿/切换式发布**，而不是简单把 graceful timeout 砍小

对于 `cliproxyapi` 这类号池服务，我目前不建议直接做双实例同时在线滚动发布，原因是：

- 会让同一批上游账号同时承受两套活跃实例的连接池和请求调度
- 对外指纹更复杂
- 对 Codex 这类账号池场景，收益未必比风险更大

---

## 6. 建议的优化优先级

### 第一优先级（先做）

1. **executor 避免对同一请求做两次 `TranslateRequest`**
2. **重写 `convertSystemRoleToDeveloper`，去掉循环里的全文重复扫描**

原因：

- 直接命中当前线上最热路径
- 修改收益比最高
- 风险相对可控

### 第二优先级（紧接着做）

3. **顺手优化 `normalizeCodexBuiltinTools`**
4. **检查是否还能减少 translator 内部的 repeated `sjson.Set/Delete` 次数**

### 第三优先级（视收益决定）

5. **继续评估 `io.ReadAll` 是否能减少整块读取**
6. **看 SSE 输出链路是否需要进一步减分配**
7. **同步更新 Codex UA 常量**

---

## 7. 我建议的落地方式

### 方案 A：最小改动、先拿结果

先只做两处：

- `internal/runtime/executor/codex_executor.go`
- `internal/translator/codex/openai/responses/codex_openai-responses_request.go`

目标：

- 不大改整体 translator 架构
- 先把重复翻译和最热点函数打掉
- 然后上线观察 profiler 抓样频率是否明显下降

这是我最推荐的第一步。

### 方案 B：局部重构 translator

如果和 Claude Code 评审后认为收益足够，可以继续做：

- 把 `input` / `tools` 这类数组型结构改成“解析一次、批量改写、一次写回”的模式
- 尽量少用“path 字符串 + 全文重扫”的更新方式

这个方案收益更大，但要更谨慎测兼容性。

---

## 8. 上线后怎么验证优化是否真的生效

建议按这个顺序验证：

1. **功能正确性**
   - 原有 `/v1/responses` 正常
   - Codex builtin tool 映射正常
   - `system -> developer` 语义不变

2. **编译与测试**
   - `gofmt -w ...`
   - 针对 executor / translator 增加或跑定向测试
   - `go build -o test-output ./cmd/server && rm test-output`

3. **生产部署后观察**
   - 继续看 `/home/iec/deploy/log/cliproxy-profiler/`
   - 看 `journalctl -u cliproxy-profiler`
   - 对比 burst 抓样频率是否下降
   - 对比 spike 峰值是否从 100%+ 明显回落

4. **如果需要，再抓新样本复盘**
   - 重点看 `gjson.parseSquash`
   - 看 `convertSystemRoleToDeveloper` 是否从热点榜显著下移
   - 看 `sjson.appendRawPaths` 的占比是否下降

---

## 9. 这次排查涉及的关键文件

### 线上运行与证据

- `/home/iec/deploy/etc/cliproxyapi.yaml`
- `/home/iec/deploy/log/cliproxy-profiler/20260424-004351-pid370005-burst-105.5/`
- `/home/iec/deploy/auths/logs/main.log`

### 代码位置

- `internal/runtime/executor/codex_executor.go`
- `internal/translator/codex/openai/responses/codex_openai-responses_request.go`
- `sdk/cliproxy/service.go`
- `sdk/cliproxy/pprof_server.go`
- `sdk/cliproxy/service_shutdown_test.go`

---

## 10. 给后续和 Claude Code 过文档时的简版结论

如果只保留一句话：

> 线上 CPU spike 的主因已经基本坐实，就是 Codex `/v1/responses` 请求在进入上游前，做了过重的 JSON 翻译与重复转换；第一步应该先砍掉 executor 双翻译，再重写 `convertSystemRoleToDeveloper` 这个热点函数。

如果保留三句话：

1. **spike 真实存在，而且不轻**，不是监控误报。
2. **热点是 JSON 翻译链路**，不是死锁、线程泄漏、系统层异常。
3. **优化优先级非常明确**：先砍双翻译，再砍 repeated scan / repeated rewrite。

---

## 11. 落地验证结果（2026-04-24 02:00 – 07:25）

优化已于 2026-04-24 01:53 部署上线（版本 `cliproxyapi.20260424-015248`），健康检查通过，服务 PID 376010，自动回滚未触发。观察窗口约 5h30m，结论如下。

### 11.1 实际改动清单

- `internal/translator/codex/openai/responses/codex_openai-responses_request.go`
  - `convertSystemRoleToDeveloper`：循环内 `gjson.GetBytes(result, "input.%d.role")` 改为 `inputArray[i].Get("role")`，将 O(N²) 扫描降到 O(N)。
  - `normalizeCodexBuiltinTools`：`tools[]` 与 `tool_choice.tools[]` 循环内同样改用预解析数组元素取 `type`，不再 `normalizeCodexBuiltinToolAtPath` 做全文重扫。`tool_choice.type` 单路径分支保留不变。
- `internal/runtime/executor/codex_executor.go`
  - `Execute` / `executeCompact` / `ExecuteStream` 三处双次 `TranslateRequest`：当 `opts.OriginalRequest` 为空时（线上常见路径）直接复用 `body` 作 `originalTranslated`，省掉一次完整翻译。
  - `codexUserAgent` 常量：`0.118.0 → 0.121.0`，与真实客户端对齐。
- 正确性：`sjson` 默认 `inplace=false`，别名切片不会被 in-place 改写；`ApplyPayloadConfigWithRoot` 对 `original` 仅走 `gjson.Exists()` 只读检查。已核实不会破坏语义。
- 单元测试：`./internal/translator/codex/openai/responses/...` 与 `./internal/runtime/executor/...` 全部通过。

### 11.2 Burst 频率与峰值对比

| 版本 | 观察窗口 | 捕到 burst | cooldown 跳过 | 总 spike 频率 | 峰值 CPU |
|------|---------|------------|---------------|---------------|----------|
| 旧 PID 115448 | 23:57 – 00:24（27m）| 4 次 | 多次（含 220.86%、149.44%）| ~1 次/7min | 220%+ |
| 旧 PID 370005 | 00:44 – 01:44（60m）| 2 次 | 8 次（102%~123%）| ~1 次/6min | 127% |
| **新 PID 376010** | **01:53 – 07:23（5h30m）** | **1 次** | **1 次（100.44%）** | **~1 次/165min** | **109.7%** |

Burst 频率下降 ~25 倍，峰值从持续 120%+（含 220%）降到刚过阈值。

### 11.3 CPU pprof 对比

样本来源：改前 `20260424-004351-pid370005-burst-105.5` / 改后 `20260424-071856-pid376010-burst-109.7`，两次触发时 TCP established 量级接近（231 vs 161），都有密集 `/v1/responses` 长请求。

**改前（10s 窗口总样本 1.55s）：**

```
gjson.parseSquash          0.99s / 1.55s   63.87%  flat
gjson.Get (cum)            1.13s / 1.55s   72.90%  cum
convertSystemRoleToDeveloper 单行          1.03s
```

**改后（10s 窗口总样本 270ms）：**

```
runtime.memmove            50ms / 270ms    18.52%  flat
gjson.parseString          30ms / 270ms    11.11%  ← 普通字符串解析
syscall6                   30ms / 270ms    11.11%
runtime.scanObjectsSmall   20ms / 270ms     7.41%  ← GC
gjson.squash               10ms / 270ms     3.70%
convertSystemRoleToDeveloper               不在 top 20
```

关键量化：
- 10s 采样窗口内总 CPU 样本从 1.55s 降到 270ms（-83%）
- `gjson.parseSquash` 从 63.87% flat 掉出 top 20
- `convertSystemRoleToDeveloper` 从热点榜彻底消失
- 剩余 CPU 全在 `memmove` / 真正的字符串解析 / syscall / GC，都是正常开销

### 11.4 堆内存对比（inuse_space）

| 指标 | 改前 | 改后 | 变化 |
|------|------|------|------|
| `sjson.appendRawPaths` | 42.95% | 31.23% | -27% |
| `io.ReadAll` | 19.45% | 10.47% | -46% |

砍双翻译把 sjson 写操作数量级减半，`io.ReadAll` 下降说明整体请求链路分配也降低。GC 压力相应下降。

### 11.5 服务健康指标

- 瞬时 CPU：2.8% → 1.2%（随着启动预热完成继续下降）
- 内存：启动后峰值 392 MB → 回落至 80 MB RSS，GC 正常工作
- 自动回滚未触发，systemd service 持续 `active (running)`

### 11.6 达成情况对照

| 文档预期 | 实测结果 |
|---------|---------|
| burst 抓样频率明显下降 | ✅ ~25× 下降 |
| spike 峰值从 100%+ 明显回落 | ✅ 从 220% 回落到 109% |
| `gjson.parseSquash` 显著下移 | ✅ 从 63.87% 掉出 top 20 |
| `sjson.appendRawPaths` 占比下降 | ✅ 42.95% → 31.23% |
| `convertSystemRoleToDeveloper` 从热点榜下移 | ✅ 不在 top 20 |

文档 §8 列出的四类验证全部通过。本次优化结案。

