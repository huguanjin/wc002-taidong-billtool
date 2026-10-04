# 实施提示词：脱敏日志补充逐条 Token / 计费明细列

> 交付对象：AI 编码助手。
> 目标仓库/目录：`wc002-taidong-billtool/`（Go 后端 `backend/`，Vue 前端 `frontend/`）。
> 这是一份**可直接执行的任务说明**。动手前先按第 2 节把相关文件读完，再按第 4 节的清单逐项改。

---

## 0. 一句话任务

让「生成脱敏日志」这个功能的输出文件，在现有 4 个缓存列之外，**再补上每一条请求的明细 token 数据与（可选）计费过程字段**，使客户拿到的脱敏日志能逐条核对缓存读、缓存写、非缓存输入、输出、图片/音频/推理 token 等数据，而不必依赖已被脱敏丢弃的 `other` 列。

---

## 1. 背景与现状

### 1.1 客户诉求

客户要求脱敏日志明细提供**每条记录对应的缓存读取、缓存写入 token 等明细 token 数据**。这些数据在日志里目前只存在于 `logs.other`（JSON 字符串）字段中，而脱敏日志出于脱敏要求会把整列 `other` 删掉。

### 1.2 现状（重要，先确认再动手）

脱敏日志**已经展开**了 4 个缓存列（见 `backend/billing/constants.go` 的 `SanitizedCacheColumns`）：

| 现有列 | 含义 | 写入值 |
|---|---|---|
| `cache_tokens` | 缓存读取 token | 生效值，见 1.3 |
| `cache_creation_tokens` | 缓存写入总量 | `cacheWrite5m + cacheWrite1h` |
| `cache_creation_tokens_5m` | 5 分钟档缓存写入 | 生效值 |
| `cache_creation_tokens_1h` | 1 小时档缓存写入 | 生效值 |

`cache_creation_tokens` 写的是 5m+1h 的**合计**，这是刻意设计（`excel_write.go` / `csv_write.go` 里分别是 `cacheWrite5m+cacheWrite1h` 与同样的拼接），**不要**"顺手修正"成别的口径。

所以本次任务包含两部分：

- **Part A（核对）**：确认这 4 列在所有日志形态下都有正确取值（尤其 Claude 5m/1h 分档、OpenAI 原生 `cache_write_tokens`、老日志只有 `cache_creation_tokens` 三种形态）。
- **Part B（新增）**：把 `other` 里剩余的逐条明细字段展开成新列。

### 1.3 现有的"生效值"口径（必须沿用，不要重写）

`AggregateFromRows`（`backend/billing/aggregate.go`）逐行算出的 `cacheRead / cacheWrite5m / cacheWrite1h` 是**计费时实际采用的值**，不是日志列的原始值：

- 若日志同时有 `cache_tokens` + `cache_creation_tokens` 两列：`cacheRead = max(列值, other.cache_tokens)`，写档位则看 `other` 里有没有 `cache_creation_tokens_5m`（有则按 5m/1h，没有则整列算 5m 档）。
- 否则回退到 `ParseCacheTokens(other)`（`pricing.go`），其内部优先结构化 JSON、失败再走 `ExtractCacheFields` 的容错解析（URL 解码 / 去转义 / 正则兜底）。

新列的取值必须**和这一套口径同源**，否则客户会看到「同一行里缓存读列 != 明细列」的自相矛盾。**推荐做法**：新列直接复用循环里已经算好的局部变量与已有解析函数，不要另起一套解析。

---

## 2. 动手前必读（数据流）

按这个顺序读，理解一行的数据是怎么从日志流到输出文件的：

1. `backend/billing/service.go` — `GenerateBill()`：决定是否生成脱敏日志、选 xlsx 还是 csv/tsv、构造 writer，最后把 `SanitizedRowWriter` 传给聚合函数。
2. `backend/billing/aggregate.go` — `AggregateFromRows()`：逐行解析，算出 `cacheRead/cacheWrite5m/cacheWrite1h/uncached/completion/...`，在**行循环内部**调用 `sanitizedWriter.WriteRow(row, cacheRead, cacheWrite5m, cacheWrite1h)`。这是唯一能给 writer 传逐行数据的地方。
3. `backend/billing/excel_write.go` — `ExcelSanitizedWriter`（`buildSanitizedHeaders` 决定表头、`WriteRow` 决定每行值），以及 `csv_write.go` 里的 `CSVSanitizedWriter`。
4. `backend/billing/constants.go` — `SanitizedCacheColumns`（新列清单应加在这里）与 `SanitizedDropColumns`（决定 `other` 等列被删除）。
5. `backend/billing/pricing.go` — 现成的 `other` 解析函数：`jsonNumber`、`ParseCacheTokens`、`ParseExtraTokens`、`ParseWebSearch`、`ParseModelPrice`、`UsageSemanticFromOther`、`paramsFromOther`。**新解析一律复用这些**，不要新写 JSON 解析风格。
6. `backend/billing/log_merge.go` — 日志合并功能，详见第 5.3 节，新增列会影响它。

主库侧参考（只读，不要改）：`new-api/service/log_info_generate.go`、`new-api/service/text_quota.go` 是 `other` 各字段的写入方，字段含义以它们为准。

---

## 3. 目标产物：新增列契约

**表头名一律沿用 `other` 里的原字段名**（便于客户和后台日志对照），**列顺序**统一追加在现有 4 个缓存列**之后**，保持「原日志所有列（去掉 other 及缓存列）→ 4 个缓存列 → 新列」的既有结构。

### 3.1 Part B 必做列

| 表头 | 来源（`other` 键 / 计算） | 说明 |
|---|---|---|
| `uncached_input_tokens` | 循环里的 `uncached` 变量（`UncachedInputTokens(prompt, cacheRead, cacheWrite5m, cacheWrite1h, semantic)`） | 非缓存输入 token，计费口径值。注意 anthropic 语义下 `prompt_tokens` 本身就是未命中量，openai/gemini 语义下要扣掉缓存读+写 |
| `input_tokens_total` | `other.input_tokens_total` | 归一化后的输入总量；缺失时留空（**不要**自行相加推算，主库明确写了不能反推） |
| `usage_semantic` | `other.usage_semantic` | `anthropic` / `openai` 等；决定上面 `uncached` 的口径，客户核对时需要 |
| `cache_write_tokens` | `other.cache_write_tokens` | 归一化缓存写入总量（主库同时写缓存分档时的合计），缺失时留空 |
| `text_input_tokens` | `other.text_input` | 仅 WSS/音频请求才有；缺失留空 |
| `text_output_tokens` | `other.text_output` | 同上 |
| `audio_input_tokens` | `other.audio_input`，为 0 时回退 `other.audio_input_token_count` | 音频输入；缺失留空 |
| `audio_output_tokens` | `other.audio_output` | 同上 |
| `image_output_tokens` | `other.image_output`（`ParseExtraTokens` 里对应 `completion_tokens_details.image_tokens`） | 图片输出 token；缺失留空 |
| `reasoning_tokens` | `other` 内 `completion_tokens_details.reasoning_tokens` | 推理 token。若主库当前不写此字段，**保持取值逻辑就位但不报错**，将来主库补写即可自动生效；是否要顺带读 `other.reasoning_tokens` 顶层键由实现者按实际日志确认 |
| `web_search_calls` | `ParseWebSearch(other)` 的第一个返回值 | 工具调用次数（0 表示没有） |
| `tool_surcharges` | `other.tool_surcharges`（数组，元素 `{name, count, price}`） | 序列化成简短文本，如 `web_search×1@10`；多元素用 `;` 分隔；未命中留空 |

### 3.2 可选列（默认关闭，需用户显式开关）

以下字段暴露站点内部计费参数，客户是否可见属于商务决定，**默认不输出**：

| 表头 | 来源 |
|---|---|
| `model_ratio` / `completion_ratio` / `group_ratio` / `user_group_ratio` | `other` 同名键 |
| `cache_ratio` / `cache_creation_ratio` / `cache_creation_ratio_5m` / `cache_creation_ratio_1h` | `other` 同名键 |
| `model_price` | `other.model_price` |
| `billing_mode` | `other.billing_mode`（`token` / `per_call` / `tiered_expr`） |
| `matched_tier` | `other.matched_tier` |
| `pre_consumed_quota` / `actual_quota` | `other` 同名键 |

实现方式：`billing.Params` 增加 `IncludeBillingParams bool`，前端在"生成脱敏日志"下面加一个复选框（默认不勾选），后端 `main.go` 解析表单字段传入。**未开启时这些列完全不出现在输出文件里**（表头也不要出现），不要出现"列在但整列空"的情况。

### 3.3 缺失值约定

- 数字类字段：缺字段 **留空**（Excel 写 `nil`，CSV 写空串），写 `0` 会被读成"这一项发生了但为 0"。
- 已确定为计费口径的字段（`cache_*`、`uncached_input_tokens`）：**写数字，包括 0**，与现有 4 列行为一致。
- 文本类（`usage_semantic`、`tool_surcharges`、`matched_tier`）：缺字段留空。

---

## 4. 改动清单（按顺序做）

### 4.1 `backend/billing/types.go`：新增逐行明细结构

```go
// RowDetails 脱敏日志需要额外展开的单行明细，全部来自日志 other 字段
// 或聚合循环已算出的计费口径值。零值表示「该日志没有这个字段」，
// 写出时留空，不要写成 0。
type RowDetails struct {
    UncachedInputTokens float64 // 计费口径的非缓存输入
    UsageSemantic       string
    InputTokensTotal    *float64
    CacheWriteTokens    *float64
    TextInput           *float64
    TextOutput          *float64
    AudioInput          *float64
    AudioOutput         *float64
    ImageOutput         *float64
    ReasoningTokens     *float64
    WebSearchCalls      float64
    ToolSurcharges      string
    // BillingParams 仅当 Params.IncludeBillingParams 为真时使用
    Billing BillingDetails
}

// BillingDetails 站点内部计费参数（可选输出，默认关闭）。
type BillingDetails struct {
    ModelRatio           *float64
    CompletionRatio      *float64
    GroupRatio           *float64
    UserGroupRatio       *float64
    CacheRatio           *float64
    CacheCreationRatio   *float64
    CacheCreationRatio5m *float64
    CacheCreationRatio1h *float64
    ModelPrice           *float64
    BillingMode          string
    MatchedTier          string
    PreConsumedQuota     *float64
    ActualQuota          *float64
}
```

用 `*float64` 表达"有/无"（与项目里"可选标量用指针 + omitempty"的既有约定一致）。

### 4.2 `backend/billing/pricing.go`：新增解析函数

新增 `ParseRowDetails(other string, includeBilling bool) RowDetails`：

- **一次** `json.Unmarshal` 拿 `map[string]interface{}`，然后全部用现成的 `jsonNumber` 取值；不要为每个字段单独 unmarshal（现有代码每行已经因为 `ParseCacheTokens` / `ParseExtraTokens` / `ParseBillingExpr` / `UsageSemanticFromOther` 各 unmarshal 一次）。
- 非 JSON 时不要报错：缓存族字段由 `ParseCacheTokens` 的容错路径负责，其余字段留空即可。
- `tool_surcharges` 用 `[]interface{}` 遍历，取 `name/count/price`；`count` 或 `price` 缺失就跳过该元素；整体为空则留空串。
- `includeBilling=false` 时不要填充 `Billing`（省一次遍历，也避免将来误输出）。

### 4.3 `backend/billing/aggregate.go`：接口签名带明细

- `SanitizedRowWriter` 改为 `WriteRow(row []string, details RowDetails) error`。
- 行循环里把原来的 `WriteRow(row, cacheRead, cacheWrite5m, cacheWrite1h)` 改为一处构造：

```go
details := ParseRowDetails(other, includeBilling)
details.UncachedInputTokens = uncached
details.WebSearchCalls = wsCalls
// 注意顺序：`uncached` / `wsCalls` 必须在调用 WriteRow 之前算出来。
```

- `AggregateFromRows` 需要新增一个 `includeBilling bool` 参数（或把开关放进一个新的 options 结构体，二选一，保持调用点清晰）。**`AggRow` 结构、聚合结果、账单逻辑一行都不要动。**

> 现有代码把 `WriteRow` 调用放在 `uncached`/`wsCalls` 计算**之前**（`aggregate.go` 里 `if sanitizedWriter != nil` 那段在 `UncachedInputTokens` 之前）。改造时把这几个计算的顺序理顺，或把 `WriteRow` 调用下移到计算之后——**只调整顺序，不要改任何计算逻辑**。这是本次改动最容易出错的地方，改完请对照账单校验一次数字。

### 4.4 `backend/billing/constants.go`：集中列清单

```go
// SanitizedDetailColumns 脱敏日志在缓存列之后追加的明细列（顺序即输出顺序）。
var SanitizedDetailColumns = []string{...} // 按 3.1 表格顺序

// SanitizedBillingColumns 可选输出的计费参数字段（需 IncludeBillingParams 开启）。
var SanitizedBillingColumns = []string{...} // 按 3.2 表格顺序
```

同时给 `SanitizedDropColumns` 补上注释说明：新列是"从 other 里提出来"的，`other` 本身仍整体丢弃（不因新增列而改变脱敏策略）。

### 4.5 `backend/billing/excel_write.go` / `csv_write.go`：写出新列

- `buildSanitizedHeaders` 增加开关参数：按 `includeBilling` 决定是否追加 `SanitizedBillingColumns`。注意它在两个 writer 的构造函数里都被调用（`NewExcelSanitizedWriter` / `NewCSVSanitizedWriter`），需要一并加参数，并检查 `service.go` 的调用点。
- `WriteRow` 按同一顺序追写值：xlsx 走 `cellValueForSanitized` 保持数值型单元格；csv 走 `formatFloat`。**空值**：xlsx 写 `nil`，csv 写 `""`。
- xlsx 的单 sheet 104 万行拆分逻辑（`ExcelMaxRowsPerSheet`）不受影响，但新增列后 `startSheet` 写表头的行内容要同步。

### 4.6 `backend/main.go` / `frontend/src/App.vue`

- `handleGenerateBill`：解析 `includeBillingParams` 表单字段（`"true"`），填入 `Params`。
- `Params` 增加 `IncludeBillingParams bool` 字段。
- `App.vue`：`form` 默认值区（约 229 行附近）增加 `includeBillingParams: false`；提交处（约 404 行附近）`fd.append('includeBillingParams', String(form.value.includeBillingParams))`；"生成脱敏日志格式"下拉框下方增加一个复选框，文案示例："附带计费参数列（模型/分组倍率等内部参数，默认不导出）"。
- **不要**改 `/api/bill` 的响应结构，也不要动 `result.summary`。

---

## 5. 硬性约束与已知边界

### 5.1 兼容性

- 现有列名、列顺序、`cache_creation_tokens = 5m + 1h` 的口径**都不许改**。新列只能追加。
- 不改变计费算法：`AggRow`、`RowListUSD`、`ComputeGroupDiscounts`、`WriteBillFromTemplate`、账单模板列全部不动。本次是**纯输出扩展**。
- 不导出 `other` 原始 JSON，也不导出任何被 `SanitizedDropColumns` 明确删除的字段（含 `admin_info` 下的渠道/密钥类信息）。

### 5.2 不要顺手做的事

- 不要为"少写一次 `json.Unmarshal`"重构 `ParseCacheTokens` 的对外行为；要合并解析就在被调用方（`ParseRowDetails`）做，且必须保持 `ParseCacheTokens` 现有语义与容错路径不变。
- 不要新增只有一个调用点的小工具函数（项目规范明确反对）；确实需要的帮助函数应当是稳定领域概念（例如 `SanitizedColumns(includeBilling bool) []string` 这类"列集"概念是合适的）。
- 不要把 `SanitizedRowWriter` 拆成多个接口或改成变参 `...interface{}`，保持强类型。

### 5.3 与「日志合并」功能的冲突（必须处理或明确说明）

`log_merge.go` 的 `mergeColumnPermutation` 要求各源文件的**表头列集合完全一致**，否则报「缺少列 / 表头列数不一致」并中断合并。

新增列后：**旧版生成的脱敏日志与新版的无法合并**（哪怕只是多了一列）。请按以下方式处理并在交付说明里写清楚：

1. 首选方案：把 `mergeColumnPermutation` 放宽为「基准表头里有、源文件缺的列 → 该列按空值补齐；源文件多出基准没有的列 → 仍然报错」。同时**必须**同步更新 `log_merge_test.go`（现有测试断言了缺列报错的严格行为，需要拆成"缺列留空"与"多列报错"两条用例）。
2. 备选方案：不改合并逻辑，只在 UI/文档里提示「脱敏日志列集会随版本变化，请用同版本产物合并」。

无论选哪种，**不要**让合并功能静默错位。

### 5.4 空值语义

`0` 与"缺字段"必须可区分（见 3.3）。客户按这些列对账时，"写了 0"和"没这一项"是两种结论。

---

## 6. 测试要求

沿用 `backend/billing/` 现有测试风格（表驱动、显式输入与期望值）。测试库用已引入的 `github.com/stretchr/testify`（`require` 做前置断言、`assert` 做值断言）：

1. `ParseRowDetails` 表驱动测试：
   - `usage_semantic: anthropic` 的 Claude 形态（含 `cache_creation_tokens_5m` / `1h`）；
   - OpenAI 原生 `cache_write_tokens` 形态；
   - 双重编码 JSON（`other` 本身是带引号的字符串）与 URL 编码形态；
   - 完全不是 JSON（走容错路径，不得 panic、不得报错）；
   - 空字符串；
   - `tool_surcharges` 单元素 / 多元素 / 空数组；
   - `includeBilling=false` 时 `Billing` 为零值。
2. 脱敏日志写出测试（xlsx 与 tsv 各一条，至少覆盖 tsv）：
   - 断言**完整表头切片**（顺序敏感），含与不含计费参数列两种情况；
   - 断言首行新列取值与 `other` 原字段一致（取一个有值行 + 一个全空行）；
   - 断言 `cache_creation_tokens` 仍等于 5m+1h；
   - 断言 `other` 列确实不在表头里。
3. 不要写只跑通、无断言、堆随机输入的"冒烟"测试。

---

## 7. 验收标准

- [ ] `cd backend && go build ./... && go vet ./... && go test ./...` 全绿。
- [ ] cwd 在 `backend/`，用至少一份**真实形态**的样例日志（覆盖 Claude 分档缓存、OpenAI 原生 cache_write、tiered_expr 表达式、web_search 工具调用）跑一次 `/api/bill`，xlsx 与 tsv 两种脱敏格式都验证。
- [ ] 输出文件表头与第 3 节契约完全一致（顺序、拼写、大小写）；默认不含 3.2 的计费参数列。
- [ ] 抽查若干行：新列数值与后台日志详情页显示的对应字段一致；缓存三列与明细列口径自洽。
- [ ] 账单 xlsx 内容与改造前逐格一致（`_scratch_dump.py` 可用来做前后对比 dump）。
- [ ] 前端勾选/不勾选"附带计费参数列"各生成一次，确认列集差异符合预期。
- [ ] 若采纳 5.3 的方案一，新旧日志混合合并有明确行为（缺列补齐），并有测试覆盖。

---

## 8. 不做的事（Non-goals）

- 不改 `new-api` 主库任何代码（本次纯消费侧改动）。
- 不改账单模板 `data/bill_template.xlsx` 与账单写出列结构。
- 不改计费/定价/折扣算法与 `AggRow` 聚合口径。
- 不把 `other` 原样导出，也不恢复任何被脱敏删除的列。
- 不动登录鉴权、下载接口、任务目录管理。

---

## 9. 交付物

1. 代码改动（后端 + 前端）与新增测试。
2. 一段简短的变更说明，包含：
   - 新增列清单（表头名 + 含义 + 取值来源一句话）；
   - 一组样例表头（默认模式与开启计费参数各一）；
   - 5.3 的最终选择与理由；
   - 是否所有新列都能在当版日志里取到值，取不到的列出字段名与原因（例如主库暂未写入 `completion_tokens_details.reasoning_tokens`）。
3. 提交信息用中文，风格与仓库现有提交一致（如"脱敏日志补充逐条 token 明细列"）。
