# Magpie 用量与金额计算对照审计

## 范围与版本

- 目标：检查本地 CPA Mini Usage 的用量、API 等价值和额度外推；参考 `yetone/magpie`。未修改生产逻辑、历史数据，未部署。
- 本地提交：`898f3cf7c3379b81e4dc34d6c5651ac06ffff6b8`。
- Magpie 最新发布（检查时）：`v0.1.1230`，提交 `8f34bbcd1158b0351c8730b59d3d10f14c5f3405`。
- Magpie 主分支（检查时）：`2076943915c4ccd1f90570396416abc3ef957fdd`。
- CPA 参考版本：`v8.0.22`，提交 `67465884ca179a8f9098328d03a361b50003fdd7`。这不是对用户当前部署版本的确认。
- Magpie 主分支相对发布版增加了按自定义端点选择价格目录等逻辑；下述 `CostSplit` 和窗口外推核心逻辑在二者中相同。以下 Magpie 行号以主分支为准。

## 1. Magpie 如何统计 token

网关请求、客户端会话文件、客户端上报可以进入请求账本；匹配的会话与网关记录会去重。额度容量估算则单独读取自己的网关日志，并不直接使用合并后的账本总额。

内部普通输入不含缓存。例如 Responses 的原始 `input_tokens` 已含缓存，Magpie 用 `max(input_tokens - cached_tokens - cache_write_tokens, 0)` 得到普通输入，缓存读写另外存储（`internal/gateway/responses.go:1195–1220`）。Anthropic 的普通输入、缓存读、缓存写本来就是独立桶。

本地保留 CPA 原生字段：OpenAI/Codex 的 Input 含缓存，在 `eventCost` 扣除缓存；Claude 的 Input 不含缓存，计价时直接加独立缓存桶（`engine.go:450–490`）。对合法且完整的标准回调，两者四桶公式等价，不能照搬 Magpie 输入字段定义后再扣一遍。

Magpie `WindowHolds.Tokens` 是普通输入加输出，缓存流量另列；本地 `ObservedTokens` 累计 CPA `TotalTokens`，通常包含缓存。二者 token 数不同不一定是错误（Magpie `internal/provider/window_holds.go:14–32`；本地 `engine.go:526–545`）。

## 2. Magpie 如何算钱

统一金额公式：

```
USD = (普通输入 × 输入单价 + 输出 × 输出单价
       + 缓存读 × 缓存读单价
       + 5分钟缓存写 × 5分钟写单价
       + 1小时缓存写 × 1小时写单价) / 1,000,000
```

- `internal/catalog/catalog.go:239–277`：按包含全部缓存的 prompt 长度选择跨越的最高上下文档位，整次请求使用该档位，而非只给超出部分加价；reasoning 已计入输出，不再重复收费。
- `internal/usage/usage.go:363–370`：明确上报的 `OwnCost` 优先，否则 `CostSplit`。
- `internal/usage/ledger.go:451–488`、`internal/provider/models.go:1788–1812`：账本价格优先用户指定价格，再服务商目录及厂商目录，可应用服务商倍率；原模型无价格时才尝试响应声明模型。不是一律相信响应名重算。
- 查询使用同一价格设置快照，历史会重新估值；并非保留请求发生时的真实发票价格。
- 容量外推的 `apiPricer` 使用 API 目录价格，不使用用户为订阅设置的零价格或服务商计费倍率（`internal/usage/window_holds.go:239–259`）。

官方文档与当前价格源均成功 HTTP 200：

- https://developers.openai.com/api/docs/models/gpt-6-astra （确认 >272K 阈值及整次请求加价）
- https://developers.openai.com/api/docs/pricing
- https://platform.claude.com/docs/en/about-claude/pricing
- https://models.dev/api.json

本地标准 GPT-6 Astra 价格与当前官方表一致：短上下文输入/缓存读/缓存写/输出分别 $10/$1/$12.5/$50 每百万；长上下文分别 $20/$2/$25/$75。标准模式下 304,950 输入、141 输出、无缓存的估值是 `$6.109575`。若真正观测到 300,000 缓存读，则为 `$0.709575`。后一项是条件示例，不能为降低金额而假造缓存命中。Fast、Ultrafast、Flex、地区处理是另外的口径，当前本地只能表达标准 API 等价值。

## 3. Magpie 如何外推额度容量

官方百分比来自供应商，不从 token 或美元反推。公式仍然是：

```
API 等价值容量 ≈ 截至额度读取时的网关已观测 API 等价值 × 100 / 官方已用百分比
```

`internal/usage/window_holds.go:143–234` 有重要筛选：

1. 官方已用比例至少 5%；`HoldsFloor` 注释明确是降低百分比舍入导致的噪声，不是供应商计费规定。
2. 网关最早合格日志不晚于窗口开始；这只是全局日志覆盖门槛，不证明每个账号完整采集，更不能覆盖其他客户端活动。
3. 匹配账号、供应商和窗口模型范围，只用截至 `ReadAt` 的记录。
4. 拒绝出错等不适合外推的额度卡；结合历史额度样本排除可能混入 credits/额外用量的区间。
5. 有未定价请求则不提供完整美元容量。
6. 此路径没有本地插件的“上一相邻时间段实际用量兜底”和“保留上次有效容量缓存”。

本地已正确把分子与 `FetchedAt` 配对，而不是拿持续增长的最新请求除旧百分比（`engine.go:594–618`）。问题主要在可靠性限制较少：任意正百分比即可外推，`PartialHistory` 只是标记而不阻止计算，上一段实际用量也可以作为总量显示（`engine.go:580–584,621–631`）。它可能随模型组合、外部请求、采集覆盖和额度样本延迟改变；绝不是固定订阅美元上限。

## 4. 本地已确认的问题与限制

### A. 确定可复现：有总 token、没有可计价分桶却显示已知 $0

回调 `InputTokens=0, OutputTokens=0, TotalTokens=1000` 且模型存在价格时，`capture` 保存 1000 token，`eventCost` 返回 `(0, true)`；前端 `missing_price=false`。无法分配输入/输出的 token 不应被表述为确定零费用。真正零 token 请求、空窗口和明确免费模型必须仍能表示零，不能统统变成未知。

另一个复现是 Codex input=100、cache read=200、output=20：计价强行夹取缓存为 100 后返回已知 `$0.0011`，但展示仍保留缓存 200。异常分桶应有独立质量状态，不能以夹取后的数字假装准确。

### B. 已确认口径风险：不完整历史与微小比例仍外推

隔离样本：窗口开始很久以后才采集到 `$1`，官方累计已用 50%，本地即使 `PartialHistory=true` 仍给 `$2` 总量。该值只证明本地比例计算，不证明账号容量。低百分比同样放大舍入和更新延迟。建议借鉴 Magpie 的保守门槛，并记录真正的采集覆盖信息；不能用“本窗口第一条请求晚”直接判定未完整采集，因为账号可能自然空闲。

上一相邻时间段的实际消费不是上一周期的完整容量，应与比例外推分开，或不作为当前“预计总量”的兜底。

### C. 上游信息限制：1小时缓存和标准化质量字段

官方 Claude 5分钟写为 1.25×输入价，1小时写为 2×输入价。Magpie 保存 `CacheWrite1h`；本地只有总 CacheWrite。例如输入价 $3/M，100k 1小时写应为 $0.60，本地按5分钟价会是 $0.375。这只是条件计算，不证明用户线上请求确实用了1小时缓存。

**CPA 8.0.22 核心拥有 TokenBreakdown，但插件 ABI 没有传它。** `sdk/cliproxy/usage/manager.go:79–88` 有标准化分解；`sdk/pluginapi/types.go:1552–1568` 与 `internal/pluginhost/adapters_usage_translation.go:199–207` 仅传旧计数，没有分桶质量、1小时写细分。不能误称插件忽略了已经收到的核心质量标记，也不能只升级插件就假造这些缺失信息。需兼容旧 ABI 做基本一致性检查；未来字段只能在实际可用时读取，旧历史不补造。

ServiceTier/ResponseServiceTier 确实由适配层传出，但本地 Event 不保存；如要表示非标准模式真实 API 估值，应另做价格模式支持，而不是把标准等价值标签偷换成账单。

### D. 合成输入可复现、当前目录未触发：档位解码

- 档位只给 input，未给 output 时，本地把 output 单价覆盖成 0；Magpie继承基础价。合成请求本地 `$0.006`，继承输出价应为 `$0.008`。
- 多个上下文档位时，本地只保存最小阈值；合成请求本地 `$0.013`，最高适用档应为 `$0.021`。
- 本次实际 models.dev 的 OpenAI/Anthropic 条目没有多个 context 档，不能把此边界缺陷当成当前高金额原因。
- 缺少 cache_read 时本地默认输入价10%，Magpie结构缺省为0；两者差异并不证明任一缺省必然符合官方。应保持“缺失、明确零、已知价”三态，避免无依据猜测。

## 5. 建议修复顺序

1. 修复不可分类 token 的费用已知性，增加异常分桶质量状态；保留合法零费用。
2. 收紧比例外推：覆盖不足、过小比例、错误/陈旧样本、额外付费用量不生成新的可靠容量。明确缓存旧估计的来源与陈旧程度。
3. 不把上一段实际消费当官方容量；区分实际、外推、历史参照。
4. 支持所有上下文档位和缺省继承；明确缺价策略。
5. 若实际回调支持，再补缓存 TTL、响应服务档位；不回填不存在的数据，不默认切换到响应模型价格。

## 6. 执行的验证

- 本地 `go test ./... -count=1`：通过。
- 本地 `go test -race ./... -count=1`：通过。
- 本地 `go vet ./...`：通过。
- 本地 `node --test testdata/dashboard_test.cjs`：140/140 通过。
- Magpie 主分支 `go test ./internal/catalog -count=1` 与 `go test ./internal/usage -count=1`：通过。未运行全部 Magpie 包。
- 临时副本五项 `TestAuditReproductions` 与旧 ABI 格式 `TestAuditLegacyTotalOnly`：全部复现上述行为。它们是诊断测试，通过表示成功重现问题，不表示行为正确。
- `git diff --check` 与新增报告的空白检查：无格式诊断。
- 未读取线上事件、部署价格缓存、OAuth 凭据或真实账单；因此尚不能认定哪个缺口解释用户某一笔线上异常金额。

固定源码入口：

- https://github.com/yetone/magpie/blob/2076943915c4ccd1f90570396416abc3ef957fdd/internal/catalog/catalog.go
- https://github.com/yetone/magpie/blob/2076943915c4ccd1f90570396416abc3ef957fdd/internal/usage/window_holds.go
- https://github.com/yetone/magpie/blob/2076943915c4ccd1f90570396416abc3ef957fdd/internal/usage/ledger.go
- https://github.com/router-for-me/CLIProxyAPI/blob/67465884ca179a8f9098328d03a361b50003fdd7/internal/pluginhost/adapters_usage_translation.go
