# 用户分组倍率的数据来源

面向对象：`wc002-taidong-billtool` 中"自动拉取客户折扣"功能的开发。

本文档梳理 new-api 主库中，一个用户最终适用的"分组倍率"（即客户折扣）由哪些表、哪些字段决定，
以及主库自身是如何把这些字段拼成最终倍率的（对应 `service.GetUserGroupRatio`，
`d:\My-LocalGitFile\09new-api\new-api\service\group.go:127`）。`optiontable.go` 里已有的
`fetchModelPricesFromDB` 是同一模式的先例——直接连业务库、按 `key` 读 `options` 表——新功能可以照此风格扩展。

## 结论速览

一个用户在某次请求里实际用到的倍率 = **`users.group`（或 `tokens.group` 覆盖后的分组）** 在
**`options` 表 `GroupGroupRatio` / `GroupRatio` 两个 JSON 配置项**里查到的值，优先级：

1. 专属倍率：`GroupGroupRatio[用户分组][实际使用分组]`，如果存在就用它；
2. 否则退回通用倍率：`GroupRatio[实际使用分组]`；
3. 如果两者都查不到，主库兜底为 `1`（即无折扣）。

"特殊分组的专属倍率"说的就是第 1 条：`GroupGroupRatio` 这张表的 key 是**用户所在分组**，value 是
**该用户分组在使用某个具体分组时对应的倍率**，与所有用户通用的 `GroupRatio` 是分开存的。

## 涉及的表与字段

### 1. `users` 表 —— 用户默认分组

| 字段 | 说明 |
|---|---|
| `group` | `varchar(64)`，默认 `'default'`。用户被后台分配的计费分组，即 `userGroup`。 |

代码位置：`model/user.go:98`（`User.Group`）。读取入口 `model.GetUserGroup(id, fromDB)`
（`model/user.go:1196`），实际查询 `SELECT group FROM users WHERE id = ?`（Redis 命中时走缓存，
`fromDB=true` 或缓存未命中会读库）。

`users` 表用的是 GORM 软删除（`DeletedAt gorm.DeletedAt`，`model/user.go:104`），billtool 直连
数据库走的是原生 SQL、不经过 GORM 的自动过滤，所以拉取时必须自己加 `deleted_at IS NULL`，
否则可能查到已注销账号。`username` 字段是唯一索引（`gorm:"unique;index"`，`model/user.go:81`），
可以按用户名反查 `id`：

```sql
SELECT id, username, `group`
FROM users
WHERE deleted_at IS NULL AND (id = ? OR username = ?)
LIMIT 1;
```

billtool 侧应支持"输入用户名或用户 ID 二选一"，两个参数都传、SQL 里用 `OR` 兼容即可；
**必须走参数化查询（`db.Query(sql, id, username)`），不能把用户输入直接拼进 SQL 字符串**——
这是本功能第一次把用户可控的输入（用户名/ID）带入业务库查询，和现有 `fetchModelPricesFromDB`
（`optiontable.go:87`）查询里全是写死的 key 不同，需要单独注意注入风险。

### 2. `tokens` 表 —— 令牌对分组的覆盖（可选）

| 字段 | 说明 |
|---|---|
| `group` | `varchar`，默认 `''`。API Key/令牌可以绑定一个具体分组，覆盖用户默认分组。空字符串表示不覆盖。 |
| `auto_groups` | `text`，JSON 数组。令牌用 `auto` 分组时可携带的候选分组列表，不直接影响倍率数值，只影响"自动选哪个分组"。 |

代码位置：`model/token.go:29`。覆盖逻辑在 `middleware/auth.go:493-510`：

```go
userGroup := userCache.Group   // 来自 users.group
tokenGroup := token.Group      // 来自 tokens.group
if tokenGroup != "" {
    // 校验 tokenGroup 必须在用户可用分组内，且必须存在于 GroupRatio（或为 "auto"）
    userGroup = tokenGroup
}
// userGroup 此时就是"实际使用分组"（UsingGroup）
```

也就是说：**如果这个用户名下某个令牌单独指定了分组，该令牌发起的请求按令牌的分组算倍率，
而不是用户档案上的默认分组**。同一个用户名下不同令牌可能因此适用不同倍率。

`tokens` 表同样有 `deleted_at`（软删除，`model/token.go:32`）和 `status` 字段
（`model/token.go:18`，`1` 为启用），billtool 第一版只做「按用户维度核对折扣」，不下钻到令牌，
所以不需要查 `tokens` 表——直接用 `users.group` 当作 `userGroup` 传入下面的解析算法即可。
如果之后要做令牌粒度的核对，再回来查 `tokens` 表时记得带上
`deleted_at IS NULL AND status = 1` 的过滤条件。

### 3. `options` 表 —— 倍率配置本体（key/value）

`options` 表结构固定为两列：`key`（主键）、`value`（文本，一般是 JSON 字符串）。与
`optiontable.go` 现有的 `ModelRatio` / `CompletionRatio` 是同一张表、同一种存法。

| `key` | `value` 结构 | 含义 |
|---|---|---|
| `GroupRatio` | `map[string]float64`，如 `{"default":1,"vip":1,"svip":1}` | **通用分组倍率**：每个分组自己的基础倍率。 |
| `GroupGroupRatio` | `map[string]map[string]float64`，如 `{"vip":{"edit_this":0.9}}` | **专属倍率**：外层 key 是用户分组，内层 key 是"实际使用的分组"，value 是该组合下的专属倍率。示例含义：`vip` 分组的用户，如果被路由到/选择了 `edit_this` 分组，按 0.9 而不是 `GroupRatio["edit_this"]` 计费。 |
| `UserUsableGroups` | `map[string]string` | 每个分组对普通用户是否可见/可选（分组名 → 描述）。不影响倍率数值，但决定某个分组对该用户是否"可用"。 |
| `group_ratio_setting.group_special_usable_group` | `map[string]map[string]string` | 按用户分组追加/移除可选分组的白名单（`+:xxx` / `-:xxx` 前缀语法）。同样只影响可见性，不影响倍率数值。 |

代码位置：
- 内存态定义与默认值：`setting/ratio_setting/group_ratio.go`。
- 写入/读出 `options` 表：`model/option.go:147-148`（写入 `common.OptionMap["GroupRatio"]` /
  `["GroupGroupRatio"]`，来源分别是 `ratio_setting.GroupRatio2JSONString()` /
  `GroupGroupRatio2JSONString()`）；`model/option.go:556-559`（后台保存配置时，
  按 `key == "GroupRatio"` / `"GroupGroupRatio"` 反序列化回内存）。
- 主库进程内每隔一段时间会重新从 `options` 表加载一次（`model.SyncOptions(frequency)`，
  `model/option.go:200`），所以后台改了倍率后不是立刻生效，而是有一个同步周期；billtool
  拉取时应留意这一点，不要认为"数据库里的值等于当前正在计费的值"是瞬时的。

> 注意：`setting/ratio_setting/group_ratio.go` 里同时注册了一套形如
> `group_ratio_setting.group_ratio` 的分层配置 key（`config.GlobalConfig`），但实际持久化到
> `options` 表、且被后台管理页面读写的是上面这两个**扁平 key**：`GroupRatio` 和
> `GroupGroupRatio`。拉取时以这两个 key 为准（`optiontable.go` 现有代码拉 `ModelRatio` /
> `CompletionRatio` 用的也是这种扁平 key）。

## 倍率解析算法（与主库保持一致）

对应源码：`service.GetUserGroupRatio(userGroup, group)`（`service/group.go:124-133`），
以及计费路径上等价的 `relay/helper/price.go:58-68`（`HandleGroupRatio`）。

```go
func GetUserGroupRatio(userGroup, group string) float64 {
    if ratio, ok := GroupGroupRatio[userGroup][group]; ok {
        return ratio // 专属倍率命中
    }
    if ratio, ok := GroupRatio[group]; ok {
        return ratio // 退回通用倍率
    }
    return 1 // 都没有，兜底不打折
}
```

其中：
- `userGroup` = `users.group`；
- `group`（实际使用分组）= 若该请求的令牌指定了 `tokens.group` 则用它，否则等于 `userGroup`；
  如果分组是 `auto`，还会被中间件按 `auto_group` 上下文进一步替换成一个具体分组
  （`relay/helper/price.go:52-56`），billtool 做"客户折扣"通常不需要处理这一层自动路由，
  按用户/令牌上配置的静态分组取值即可。

## 给 billtool 新功能的建议实现

1. 复用 `optiontable.go` 里现成的 DB 连接方式（`sql.Open("mysql", cfg.dsn())`），一次性把两个
   key 都查出来：

   ```sql
   SELECT `key`, `value` FROM `options` WHERE `key` IN ('GroupRatio', 'GroupGroupRatio');
   ```

2. 分别 `json.Unmarshal` 成 `map[string]float64`（`GroupRatio`）和
   `map[string]map[string]float64`（`GroupGroupRatio`）。

3. 按用户名或用户 ID 查 `users` 表（见上面「1. users 表」一节的 SQL），拿到
   `userGroup = users.group`。第一版只做用户维度核对，不查 `tokens.group`。

4. 按上面「倍率解析算法」的优先级（先查 `GroupGroupRatio[userGroup][userGroup]`，查不到再查
   `GroupRatio[userGroup]`，都查不到用 `1`）算出最终倍率。

   > 这里 `group`（实际使用分组）取 `userGroup` 本身，因为不查令牌覆盖——对应「倍率解析算法」
   > 一节里 `group` 的定义在没有令牌覆盖时就等于 `userGroup`。

5. 【倍率 → 折扣换算，billtool 自有约定，非 new-api 逻辑】把第 4 步算出的倍率换算成折扣：
   `折扣 = 倍率 / DiscountBaseFactor`，`DiscountBaseFactor` 先写死为 `7`（即分组倍率 1 对应
   折扣 1/7），做成一个有注释说明来源的常量（放在 `constants.go`，与 `DiscountDecimals` 放一起），
   不要散落成裸数字 `7`。这个换算和 `pricing.go` 里 `ComputeGroupDiscounts` 反推出来的折扣
   （结算 CNY / 刊例 CNY）是两套不同口径的数字——反推值只能保证账面对得上，这里算出来的才是
   "系统实际配置的折扣"，两者不一致正是本功能要核对出来的问题，写结果时不要把两者混在一起显示。

6. 如果后续要支持"按令牌粒度"核对（同一用户名下令牌分组不同），再扩展查 `tokens` 表、按
   `token.group` 覆盖 `userGroup`，并把结果按令牌分别展示，不能直接汇总成一个折扣。

## 相关但不影响倍率数值、无需拉取的表/字段

- `UserUsableGroups`、`group_ratio_setting.group_special_usable_group`：只影响某个分组对用户
  是否"可见/可选"，不参与倍率计算，做折扣统计可以忽略。
- `tokens.auto_groups`：只在分组为 `auto` 时用于筛选候选分组，不是静态倍率来源。
