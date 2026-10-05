# 部署到 Ubuntu 服务器

本项目通过 GitHub Actions 自动构建 Docker 镜像并推送到 GHCR（`ghcr.io/huguanjin/wc002-taidong-billtool`），
服务器端用 `docker compose` 拉取镜像运行即可，无需在服务器上安装 Go / Node。

## 0. 前置条件

- Ubuntu 服务器已安装 [Docker Engine](https://docs.docker.com/engine/install/ubuntu/) 及 Compose 插件（`docker compose version` 能正常输出）。
- 服务器能访问 `ghcr.io`（国内服务器如访问受限，需自行配置镜像加速或代理）。
- 已推送代码到 `main` 分支或打过 `v*.*.*` 标签，触发过一次 [.github/workflows/docker-publish.yml](.github/workflows/docker-publish.yml) 并构建成功
  （去 GitHub 仓库 Actions 页签确认，或去 Packages 页签确认镜像已存在）。

## 1. 在服务器上准备部署目录

```bash
sudo mkdir -p /opt/taidong-bill
cd /opt/taidong-bill
```

需要放到部署目录 `/opt/taidong-bill` 的文件：

```
docker-compose.yml
.env.example
data/bill_template.xlsx
data/price_table.xlsx
```

`docker-compose.yml` / `.env.example` 已提交到 git 仓库，在服务器上拉取即可，任选一种：

```bash
# 方式一：仓库为 public 时，直接用 curl 下载单个文件（无需 clone 整个仓库）
curl -fsSL -o docker-compose.yml \
  https://raw.githubusercontent.com/huguanjin/wc002-taidong-billtool/main/docker-compose.yml
curl -fsSL -o .env.example \
  https://raw.githubusercontent.com/huguanjin/wc002-taidong-billtool/main/.env.example
```

```bash
# 方式二：git sparse-checkout，只拉取需要的几个文件（仓库为 private 时需先配置好 SSH key 或 PAT）
git init
git remote add origin https://github.com/huguanjin/wc002-taidong-billtool.git
git sparse-checkout init --cone
git sparse-checkout set docker-compose.yml .env.example
git pull origin main
```

> `data/` 下的两个 xlsx **没有提交到 git 仓库**（业务数据，见 [.gitignore](.gitignore)），
> 以上两种方式都拉不到，需要从本地 Windows 机器手动上传，例如在本地 PowerShell 里执行：
> ```powershell
> scp -r "d:\My-LocalGitFile\15.wangchuankeji\wc002-taidong-billtool\data" ubuntu@<服务器IP>:/opt/taidong-bill/
> ```
> 之后有更新也可以用 `rsync` 增量同步：
> ```powershell
> rsync -avz "d:\My-LocalGitFile\15.wangchuankeji\wc002-taidong-billtool\data/" ubuntu@<服务器IP>:/opt/taidong-bill/data/
> ```

## 2. 配置 `.env`

```bash
cp .env.example .env
vim .env   # 修改 BILL_AUTH_USERNAME / BILL_AUTH_PASSWORD 为真实账号密码
```

如果源日志文件另外存放在服务器某个目录，想用「服务器路径 / 可视化选择文件」功能直接读取，
额外打开 `.env` 里的 `BILL_BROWSE_ROOT` 注释并改成容器内路径，同时在 `docker-compose.yml` 里
取消对应 `volumes` 挂载的注释（详见文件内注释）。

## 3. （如果镜像包是 private）登录 GHCR

GHCR 上新推送的包默认是 private。两种方式二选一：

- **推荐**：去 GitHub 仓库 Packages 页面把包设为 Public，服务器端无需登录即可拉取。
- **或者**：在服务器上用具备 `read:packages` 权限的 GitHub PAT 登录：
  ```bash
  echo <你的PAT> | docker login ghcr.io -u <你的GitHub用户名> --password-stdin
  ```

## 4. 启动服务

```bash
cd /opt/taidong-bill
docker compose pull
docker compose up -d
```

## 5. 验证

```bash
# 查看容器状态（STATUS 应显示 healthy）
docker compose ps

# 查看启动日志
docker compose logs -f --tail=100

# 健康检查
curl http://127.0.0.1:8080/api/health
```

浏览器访问 `http://<服务器IP>:8080`，用 `.env` 里配置的账号密码登录，测试上传/生成账单。

## 6. 更新版本

代码有新变更并合并到 `main`（或打了新 tag）后，GitHub Actions 会自动构建并推送新镜像。
服务器上拉取最新镜像并重启容器即可：

```bash
cd /opt/taidong-bill
docker compose pull
docker compose up -d
```

## 7. 对外暴露 / HTTPS（可选）

`docker-compose.yml` 默认只监听 `8080` 端口且没有 TLS。生产环境建议在前面加一层反向代理
（Nginx / Caddy / Traefik）做 HTTPS 终结和域名转发，例如 Nginx 示例：

```nginx
server {
    listen 443 ssl;
    server_name bill.example.com;

    ssl_certificate     /etc/letsencrypt/live/bill.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/bill.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

配好反向代理后，可以把 `docker-compose.yml` 里的端口映射改成只监听本机：`"127.0.0.1:8080:8080"`，
避免容器端口直接暴露在公网。

## 8. 从数据库直连导出日志（可选功能）

「导出日志明细」功能可以按时间段 + 客户账号，直接从业务数据库的 `logs` 表把消费日志导出成
tsv，替代人工在服务器上执行：

```bash
mysql -h127.0.0.1 -P3316 -uroot -p'...' -D "new-api" -e "
SELECT id, username, type, created_at, token_id, token_name, model_name,
       \`group\`, prompt_tokens, completion_tokens, quota, use_time,
       is_stream, request_id, other
FROM logs
WHERE type = 2 AND username IN ('a37836323','test02')
AND created_at >= 1790179200 AND created_at < 1790697600
ORDER BY created_at, id;" > yunwu9.24-9.29.tsv
```

**与人工导出的差异（刻意为之，逐条列出）**

| 项 | 人工导出 | 本功能 |
|---|---|---|
| `other` 列 | 没有 | **追加在末尾** |
| `ORDER BY` | 无（返回顺序不保证稳定） | `ORDER BY created_at, id`，保证同样输入得到同样文件 |
| 时间精度 | 手工换算到秒 | 界面精确到秒，也可只填日期（按整段处理） |
| 时间区间 | `BETWEEN a AND b`（双闭） | `created_at >= a AND created_at < b+1s`（等价，起止时刻都含在内） |
| 文件名 | 手动指定 | `日志查询_<起始日>_<结束日>.tsv`，落在 `data/` 目录 |
| 时区 | 手动算时间戳 | 按北京时间 +08:00 解释，与容器时区无关 |

**为什么必须带 `other`**：缓存读/写、阶梯计费表达式、web_search、工具调用全部存在这一列里。
人工导出没有它，`ParseCacheTokens` 会全部返回 0，**整份账单的缓存计费静默变成 0**。
导出文件含原始请求信息，属敏感数据，**只留在服务器 data 目录，不要直接交给客户**——
生成脱敏日志时 `other` 会被整列丢弃，那份才是可以给客户的。

**配置**：复用 `BILL_DB_*`（建议只读账号），日志表名单独用 `BILL_DB_LOG_TABLE`（默认 `logs`）。
未配置 `BILL_DB_HOST` 时该功能不可用。

**限制**：仅当日志落在 MySQL 时可用。若 new-api 设置了 `LOG_SQL_DSN` 指向 ClickHouse 或独立
日志库，MySQL 驱动连不上日志表，请退回手动导出
（`clickhouse-client --query "..." --format TabSeparatedWithNames`）。
单次导出跨度上限 92 天——`logs` 表在 `(username, created_at)` 上没有组合索引，跨度过大
会扫掉大量行。

## 9. 成本估算与利润（可选功能）

「渠道成本倍率维护」+ 出账时勾选「生成成本利润表」，用于按渠道估算上游成本、看毛利。

**口径**：与站内折扣对称，只是把「分组倍率」换成「上游倍率」：

```
上游折扣 = 上游倍率 ÷ 7
上游成本 = 官方刊例（人民币） × 上游折扣
利润     = 客户结算额（V 列） − 上游成本（AG 列）
```

**用法**：

1. 「渠道成本倍率维护」页点「拉取渠道清单」——从业务库 `channels` 表只读拉取（渠道 ID / 名称 / 类型 / 状态），
   存到本地 PostgreSQL。**不会回写业务库**，也不存 key、base_url 等敏感字段。
2. 为每个渠道填上游倍率并保存。倍率是「该渠道上游给我们的结算倍率」。
   保存按钮上的数字是**待保存的改动数**；填完必须点它才算存进 PostgreSQL。
3. 出账时勾选「生成成本利润表」，会多产出一份 `成本利润_*.xlsx`，列布局是在账单 29 列之后追加
   `AD=渠道ID`、`AE=渠道名称`、`AF=上游折扣`、`AG=上游成本（人民币）`、`AH=利润（人民币）`。
   `V`（客户结算额）− `AG`（上游成本）就是利润，`AH` 已把这条公式写进单元格，可在 Excel 里追溯。
4. 结果区会给出一段**可复制的成本利润摘要**（账期 / 结算金额 / 上游成本 / 利润 / 毛利率 / 覆盖渠道数），
   直接粘到邮件或聊天里即可。这段文字由后端按与表内公式同源的口径生成，不会与 xlsx 对不上。

**注意**：部署在 http 上时浏览器剪贴板 API 不可用（它只在 HTTPS / localhost 的安全上下文里存在），
此时「复制」按钮会退化为**选中文本**，按 `Ctrl+C` 即可；也可以直接手动选中。

**前置条件**：日志必须含 `channel_id` 列。用本工具的「导出日志明细」导出的日志会自动带上；
**旧日志没有这一列，必须重新导出**。

**未维护倍率的渠道不会被静默估算**：成本列留空且不计入合计，账单备注里也会写明。
按 0 算会让成本虚低、按 1 算会虚高，两者都会误导毛利判断，所以宁可留空。
出账时会先检查：有未维护渠道则**账单照常生成**、成本利润表不出，页面列出待补录渠道；
补录后重新生成即可（不会丢掉已填好的出账参数）。

摘要里若出现「另有 N 行因渠道未维护上游倍率未计入成本与结算额合计」，说明这几个渠道的成本未知，
此时给出的利润**只覆盖已维护倍率的部分**，不等于整体毛利——这两个数都跳过，不会出现
「全量结算 − 部分成本」那种虚高算法。

用 `channels` 表里查不到的渠道号（业务库已硬删除的渠道）无法维护倍率，
成本利润表里同样留空——这类会在提示里单独列出，与「待补录」区分开。
它们**不会**拦住成本利润表的生成（拦了也补不了）。

## 10. 客户信息与账单导出任务（可选功能）

前面的成本估算回答的是「这一份日志赚多少」，这一节回答的是
「**这个月给客户导了几张账单、成本多少利润多少**」——账单产物本身是一次性的，
但数字要留下来。

**「客户信息」页**：维护客户名称 + 该客户名下的业务库账号列表。
账号可换行、逗号或空格分隔，重复的自动去掉。这是出账任务的前置：
任务只需要选客户，账号列表从这里取。

**「账单任务」页**分三块：

1. **默认出账参数**——单价来源 / 汇率 / 折扣 / 国产模型标识，存本地 PG，
   改一次长期生效，建计划时就不必每次重填。折扣**留空**表示按分组自动反推。
2. **新建/编辑计划**——客户 + 计划名 + 起止日期 + 勾选「生成脱敏日志」「生成成本利润表」。
   日期有三个快捷预设（上月整月 / 上周 / 本月至今）。**一个客户可以建多条计划**：
   月度对账一条、按周导的每周一条。
   时段可先留空后补；单次跨度上限 92 天。
3. **计划列表**——勾选若干条，点「执行选中（N）」一次跑完。执行前会先做整体校验，
   把不能执行的计划一次列全（客户没配账号、时段不合法、勾了成本表但一个渠道倍率都没维护），
   改完再执行。失败的计划保持原样，不影响其他计划已成功的记录。

执行时每条计划会自动：

- 按该客户的账号列表 + 计划时段，从业务库只读导出消费日志到 `data/`
- 出账，产出账单 / 脱敏日志 / 成本利润表
- 把结算额、成本、利润写进本地 PostgreSQL

**产物是临时的，数字是永久的**：三个文件落在 `bill-jobs` 卷里，
**6 小时后自动清理**（沿用既有的任务清理机制）。没来得及下载就点该行的「执行」重跑——
统计记录不受影响，不需要重新维护任何参数。

**重复执行同一条计划会覆盖它的结果**（结果框只保存「最近一次」）：
重跑的含义是「刷新这条计划的数字」。想同时留两份（例如同一时段按两种口径各出一份），
**手工建两条计划**即可——计划的唯一性来自它自己，不再受客户+账期约束。

**「未执行」与「未核算成本」是两回事**：

| 状态 | 含义 |
|---|---|
| 未执行 | 计划还没跑过，金额列为空。**不计入**月度汇总的账单数与金额 |
| 未核算成本 | 跑过了，但没生成成本利润表（没勾选，或渠道倍率没维护被拦） |
| 成本不全 | 跑过了也有成本，但部分渠道没维护倍率，成本只覆盖一部分行 |

前两种在金额列上看着都像空，含义完全不同，所以列表用状态列区分，
汇总也只统计已执行的计划——把没跑过的算进去会出现「有账单但金额是 0」这种对不上的行。

**利润的口径**（这点最容易看错）：渠道没维护上游倍率时，
- `账单结算额` = 客户实际要付的全部
- `成本` / `利润` 只覆盖**成本能对应上的那部分行**

所以汇总里的利润是拿「参与成本核算的结算额」减成本得出的，**不是**拿全部结算额减成本——
后者等于把没算成本的那部分当成零成本，利润会虚高。汇总的「说明」列会标出
`N 条无成本` / `N 条成本不全` / `N 条未跑`,前两种的利润不等于整体毛利。

**账期归属**：按计划的**开始日期**所在月计算，跨月计划（如 8/28~9/3）整个计入开始月，
不按天拆分——账单本身是一份不可分的文件。时间一律按北京时间（+08:00）判定，
与日志导出同一套口径。

**时段跨月是可以的**（周度任务天然跨月），只是账期归到开始月。
整月对账直接用「上月整月」预设。

**依赖**：「账单任务」需要同时配好 `BILL_DB_*`（只读业务库，用来导日志）
与 `BILL_PG_*`（存客户、计划、默认参数）。两者缺一都会在页面上直接报出来。

**权限**：客户与任务数据只写本地 PostgreSQL，**从不回写业务库**；
导出日志全程只有 `SELECT`。

## 11. 备份

需要定期备份的内容：

- `data/`：账单模板、报价表（业务基础数据，改动不频繁）。
- **命名卷 `bill-pgdata`**：本地 PostgreSQL 的数据目录，**存放客户信息、账单任务统计、
  渠道倍率、折扣快照——这些是没有第二份的**，删了就没了：
  ```bash
  docker run --rm -v taidong-bill_bill-pgdata:/data -v $(pwd):/backup alpine \
    tar czf /backup/bill-pgdata-backup.tar.gz -C /data .
  ```
  或直接用 `pg_dump`（更稳妥，且可跨版本恢复）：
  ```bash
  docker compose exec postgres pg_dump -U "${BILL_PG_USER:-billtool}" "${BILL_PG_DBNAME:-billtool}" \
    > billtool-$(date +%F).sql
  ```
- 命名卷 `bill-jobs`（上传/生成的临时文件，默认 6 小时自动清理，一般无需备份）：
  ```bash
  docker run --rm -v taidong-bill_bill-jobs:/data -v $(pwd):/backup alpine \
    tar czf /backup/bill-jobs-backup.tar.gz -C /data .
  ```
- `.env`：账号密码配置，妥善保管，不要提交到 git。

## 常见问题

| 现象 | 排查方向 |
|------|----------|
| `docker compose pull` 报 403/未授权 | 镜像包是 private，参照第 3 步登录 GHCR，或把包设为 Public |
| 健康检查一直 unhealthy | `docker compose logs` 看后端是否因缺少 `data/` 下的模板文件启动失败 |
| 页面能打开但生成账单报错「账单模板不存在」 | 确认 `data/bill_template.xlsx`、`data/price_table.xlsx` 已放在部署目录并正确挂载 |
| 登录一直提示用户名密码错误 | 确认 `.env` 已生效：`docker compose config` 查看解析后的环境变量 |
| 想用服务器路径读取源文件但报「路径超出允许范围」 | 检查 `BILL_BROWSE_ROOT` 与实际 `volumes` 挂载路径是否一致 |
| 导出日志报「未配置业务数据库连接信息」 | 未设置 `BILL_DB_HOST`；参照第 8 节补齐 `BILL_DB_*` |
| 导出日志报连接/查询失败 | 确认日志落在 MySQL（未配置 `LOG_SQL_DSN` 指向 ClickHouse）；核对 `BILL_DB_LOG_TABLE` 表名 |
| 导出 0 行 | 不算错误，但通常说明账号拼写或时间段有误；账号需填 `logs.username` 的值 |
| 账单里缓存读/写全是 0 | 输入日志用的可能是人工导出（缺 `other` 列）。用「导出日志明细」重新导出，或手动给 SQL 加上 `other` |
| 客户信息 / 账单任务页报「未配置 PostgreSQL」 | 未设置 `BILL_PG_HOST`；参照 `.env.example` 补齐 `BILL_PG_*`，`docker compose` 里已自带 postgres 服务 |
| 执行任务报「该客户还没有配置业务库账号」 | 到「客户信息」页给该客户填账号列表（多个用换行或逗号分隔） |
| 执行任务报「在 2026-09 没有消费记录」 | 账号拼写不对，或该账期确实无消费。账号需填 `logs.username` 的值 |
| 任务页汇总里利润比预期高很多 | 看「说明」列是否标了 `无成本` / `成本不全`——渠道上游倍率没维护时，利润只覆盖有成本的部分。去「渠道成本倍率」页补齐 |
| 升级后客户/任务相关表不存在 | 后端启动时建表失败只打警告不退出。`docker compose logs` 找「初始化客户信息表失败」；确认 postgres 已 healthy（compose 已配 `condition: service_healthy`），必要时 `docker compose restart backend` |
| 升级后建不了「同一客户同一账期」的第二条计划 | 老库上还留着 `UNIQUE (customer_id, period_year, period_month)`。启动时已自动 DROP 该约束（`EnsureBillTaskSchema` 里的迁移），若失败会在日志里报「迁移 bill_export_tasks 失败」 |
| 计划执行报「还没有设置时段」 | 建计划时起止日期留空了。编辑该计划补上即可 |
| 计划执行报「客户还没有配置业务库账号」 | 到「客户信息」页给该客户填账号列表（多个用换行或逗号分隔） |
| 执行前校验说「还没有维护任何渠道上游倍率」 | 计划勾了生成成本利润表。先到「渠道成本倍率」页拉取渠道并维护倍率，或取消该计划的勾选 |
