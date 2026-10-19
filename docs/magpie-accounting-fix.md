# CPA 用量计算核查与统一 API 等价值

## 最终计算口径

**所有请求（包括 Codex OAuth）统一按标准 API 价格估值，使用模型目录声明的上下文档位。** 这是为了与 API 计价看板横向比较，不推断订阅实际扣费或消耗倍数。单笔、凭证/模型汇总、窗口观测金额与美元外推共享该计算；订阅剩余额度仍直接读取官方百分比。

```
USD = (普通输入 × 适用输入价 + 输出 × 适用输出价
     + 缓存读 × 适用缓存读价 + 缓存写 × 适用缓存写价) / 1,000,000
```

Codex/OpenAI 原始 input 已含缓存，普通输入 = input − cache read − cache write。Claude input 与缓存读写独立。Reasoning 已含在 output 中，不重复收费。

| Astra 样本（输出均为141） | 统一标准 API 等价值 |
| --- | ---: |
| input=272000，无缓存 | $2.72705 |
| input=272001，无缓存 | $5.450595 |
| input=304950，无缓存 | **$6.109575** |
| input=304950，read=300000 | **$0.709575** |
| input=304950，read=200000，write=50000 | $2.759575 |

在本次核查范围内，Magpie、CPA Manager Plus 和 New API 的 Astra 内置规则都采用上下文价卡；Sub2API 有默认关闭的账号开关和特定订阅豁免。用户最终选择统一 API 等价值以便比较，已移除订阅专用基础价分支。以上结论不代表全行业统计，也不保证各部署自定义价格相同。

## 本地实现与兼容

- 统一使用 `eventCost` 和当前模型价表，不按 OAuth/API Key 身份切换估值规则，无需新增计价来源字段或猜测旧记录来源。
- Codex 预估缓存使用 `pricing_policy=api-standard-v1`；其他策略（包括缺标记或旧基础价）失效后按可用样本重算，无可用依据则保持未知。请求历史和官方额度快照保留。
- 中英文提示明确“按标准 API 价格比较，包含适用的长上下文档位；不是订阅扣款”。

此前已修复的问题仍保留：

| 问题 | 当前行为 |
| --- | --- |
| 仅TotalTokens非零，没有可计价分桶 | 金额未知，保留total；`usage_quality=unclassified` |
| Codex缓存读写超过输入 | 金额未知，不按裁剪数字伪装准确；`usage_quality=inconsistent` |
| 缓存桶被使用但缺对应价格 | 金额未知，不猜测read为input×10%或write为input |
| 显式零价、合法零token | 保留已知零金额，与缺价区分 |
| API多上下文档位 | 保留全部阈值，选择严格跨越的最高档；缺省字段继承基础价 |
| 负token回调 | 拒收；旧负数记录不估值 |

所有金额视图共享 `eventCost`。旧 `prices.json` 的单个 `context_tier` 继续可读。

## 固定源码与参考项目结论

本地检查起点 `3e5f622`。当前工作区没有线上 `events.jsonl`、部署价格缓存或CPA运行日志，下述样本为合成回调，不代表真实账单。

### Sub2API

拉取时主分支：`8f2cbb53d9d1c40e1865b07ac460571f7d4c48de`。

- [Account.IsOpenAILongContextBillingEnabled](https://github.com/Wei-Shaw/sub2api/blob/8f2cbb53d9d1c40e1865b07ac460571f7d4c48de/backend/internal/service/account.go)：仅显式布尔true开启；字段缺失、false或类型不符均关闭。
- [openai_gateway_usage.go](https://github.com/Wei-Shaw/sub2api/blob/8f2cbb53d9d1c40e1865b07ac460571f7d4c48de/backend/internal/service/openai_gateway_usage.go)：实际计价调用接收账号开关与模型豁免；`openAISubscriptionLongContextExempt` 使用实际OAuth类凭据和最终发送模型，对GPT-6.1 Sol订阅豁免模型长上下文附加费，API Key继续使用API价卡。
- 可确认其默认关闭和特定订阅豁免，不能把它描述为所有配置/所有模型始终不加价；该网关计费策略不作为本地统一 API 等价值的豁免依据。

### New API

拉取时主分支：`32e587406c6ebe891f18ab9aeb2f4a1f6b7b9e14`。

- [builtin_billing.go](https://github.com/QuantumNous/new-api/blob/32e587406c6ebe891f18ab9aeb2f4a1f6b7b9e14/setting/billing_setting/builtin_billing.go) 内置Astra表达式为 `len <= 272000` 用基础价，否则输入/缓存2倍、输出1.5倍。
- [tiered_billing.go](https://github.com/QuantumNous/new-api/blob/32e587406c6ebe891f18ab9aeb2f4a1f6b7b9e14/setting/billing_setting/tiered_billing.go) 的 `GetBillingMode` / `GetBillingExpr` 确认该内置表达式会生效，除非已有管理员价格配置或显式计费模式覆盖。
- 未发现Codex适配器中单独的272K计价分支，不能证明New API所有订阅场景均不加价。未运行New API全项目测试。

### Magpie

最新发布 [v0.1.1294](https://github.com/yetone/magpie/releases/tag/v0.1.1294) 提交 `2c3c903974b65c145f9f009d83a2cf7e0fb3ee01`；拉取时主分支 `5f73537f5b029e48f525e497adfd19b6d4f3474c`。相关catalog/usage/provider模型文件与发布版无差异。

[Price.UnmarshalJSON / At / CostSplit](https://github.com/yetone/magpie/blob/2c3c903974b65c145f9f009d83a2cf7e0fb3ee01/internal/catalog/catalog.go) 的四桶与API档位规则已核查；本地采用同样严格阈值、整请求估值和基础价继承原则。

[窗口容量](https://github.com/yetone/magpie/blob/2c3c903974b65c145f9f009d83a2cf7e0fb3ee01/internal/usage/window_holds.go) 使用查询时观测用量×100/已用百分比，并有5%最低进度、日志覆盖、模型范围和额外付费区间筛选。

### CPA Manager Plus

[v1.14.7](https://github.com/seakee/CPA-Manager-Plus/releases/tag/v1.14.7) 与拉取时主分支同为 `64ce94e6aa1af691555429a3211e67c59c0480ad`。

- [缓存归一化](https://github.com/seakee/CPA-Manager-Plus/blob/64ce94e6aa1af691555429a3211e67c59c0480ad/apps/manager-server/internal/usage/event.go)：先统一总输入并消除legacy镜像。正常短请求合成样本与本地一致：Codex `$0.004925`、Claude `$0.0014625`、Claude显式读为零的缓存写样本 `$0.0013125`。
- [cost.go](https://github.com/seakee/CPA-Manager-Plus/blob/64ce94e6aa1af691555429a3211e67c59c0480ad/apps/manager-server/internal/service/pricing/cost.go)：仍有缺缓存价兜底与异常计数夹取。已复现缺缓存价时返回确定金额，本地保留未知。
- 同函数命中context档位后提前返回，跳过服务档位价格；合成Astra长上下文default/priority/flex均算出 `$6.109575`。这仅是API定价函数的条件行为，不证明Codex订阅消耗应翻倍或实际记录被少收费。
- [estimateWindowUsage](https://github.com/seakee/CPA-Manager-Plus/blob/64ce94e6aa1af691555429a3211e67c59c0480ad/apps/web/src/features/accounts/model/estimateWindowUsage.ts) 支持任意正百分比和previous兜底，美元按分取整。调用方另检查采样时间、模型范围、周期和预估一致性。
- 其目录 `models.dev/catalog.json` 下载成功，本次Astra及Claude Sonnet条目的cost与本地api.json对应项一致；不证明所有模型或部署价格完全相同。

## 官方资料与信息限制

[Codex官方定价](https://developers.openai.com/codex/pricing) 明确：“API token prices are separate from subscription usage; don’t use them to estimate included tasks.” [Astra API文档](https://developers.openai.com/api/docs/models/gpt-6-astra) 确认API的272K阈值，但不能据此推断订阅消耗倍数。此前252K为用户笔误，已澄清为272K。帮助中心页面返回403，未作为验证成功的依据。

CPA [v8.0.35](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.0.35)，提交 `ae3ee3be303fb07fb8173279923413005bf32177`。其[插件适配器](https://github.com/router-for-me/CLIProxyAPI/blob/ae3ee3be303fb07fb8173279923413005bf32177/internal/pluginhost/adapters_usage_translation.go) 仍只传旧token计数，没有核心TokenBreakdown和缓存TTL。显式CacheReadTokens零优先于legacy CachedTokens，避免把Claude写镜像当缓存读。

Claude一小时写入价、Fast/Flex与地区倍率仍不由本插件完整表达。未知字段不回填。`usage_quality=complete`仅表示通过旧接口可做的基本一致性检查，不代表拿到了核心标准化质量证明。

本地额度外推继续使用FetchedAt配对分子与百分比；已有任意正比例、已标记previous实际用量与有效缓存策略保留。微小百分比、缺失历史、其他客户端和模型/上下文组合仍影响可靠性；统一API等价值并不让该指标变成官方美元容量。

## 验证

本轮已通过：

- `go test ./... -count=1`；`go test -race ./... -count=1`；`go vet ./...`。
- macOS arm64 `go build -buildmode=c-shared`，产物在scratch；未构建Linux发布包。
- `node --test testdata/dashboard_test.cjs testdata/dashboard_visual_test.cjs`：144通过、1跳过，跳过项为未配置Playwright的真实浏览器矩阵。
- Codex/OpenAI统一阈值回归覆盖272000、272001、304950、缓存读写、请求及账号/模型金额、旧历史统一重估，以及缺标记/旧基础价预估缓存失效。
- `git diff --check`。

前轮已执行：Manager Plus pricing/usage/model/modelprice四包测试、三个定向诊断及原始额度函数四个断言；真实models.dev目录解码66模型。本轮改为统一API等价值回归，订阅专用分支已移除。

本报告不包含运行中 CPA 的部署或升级验证；未读取真实订阅日志或账单。发布与附件验证以 GitHub Release 和对应 CI 记录为准。
