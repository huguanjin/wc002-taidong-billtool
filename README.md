# 钛动账单工具包

把 API 调用日志（Excel / CSV / TSV）一键生成：

1. **账单 Excel**（按模型 + 分组汇总）
2. **脱敏日志 Excel**（展开缓存列，去掉 `other`）

适用于钛动及同类客户出账。

---

## Web 版（Go 后端 + Vue3 前端，推荐）

业务逻辑已从 `log_to_bill.py` / `extract_cache_columns.py` 完整移植到 `backend/`（Go），
提供一个简易网页操作页面（`frontend/`），上传日志文件、填参数、点「生成账单」即可下载账单和脱敏日志，
不用再敲命令行。

目录：

```
backend/    Go 服务：出账核心逻辑 + HTTP API（端口 8080）
frontend/   Vue3 + Vite 简易操作页面
```

### 启动方式

```powershell
# 1. 启动后端（默认读取 ../data/bill_template.xlsx 和 ../data/price_table.xlsx）
cd backend
go run .

# 2. 启动前端（开发模式，代理 /api 到 http://localhost:8080）
cd frontend
npm install   # 如遇 npm 全局缓存权限报错，改用: npm install --cache ".npm-cache"
npm run dev
```

浏览器打开前端提示的地址（默认 `http://localhost:5173`），上传日志文件即可。

### 日志合并

一个账期的日志可能分成多个文件导出，页面顶部「日志合并」卡片可以先把它们合成一个，
再拿合并结果去出账（合并结果页上点「作为账单输入」会自动回填）。

- 一次可选择多个文件，或在「服务器路径」模式下每行填一个路径，至少两个。
- 表头以第一个文件为准，后续文件按**列名**对齐，因此各文件列顺序不同也没关系；
  缺少列或列数不一致会直接报错，不会写出对不上列的数据。
- 可勾选「去除完全重复的行」（默认关闭，丢弃的行数会在结果里显示），
  避免同一段日志被重复导出时把账算重。
- 结果格式可选 `xlsx`（默认；单表满 1048576 行会自动拆到「合并日志_2」等 sheet）、
  `csv` / `tsv`（无行数上限，适合超大日志）。
- 合并结果写在 `BILL_DATA_DIR`（生产即挂载的 `data/`）下，固定命名 `合并日志.<ext>`，
  已存在同名文件时追加 `-2`、`-3`……，不覆盖旧文件。**合并结果会留在 `data/` 里**，
  定期清理即可。
- 页面底部只提供本次任务的下载链接（`/api/download/<jobId>/merged`）；
  `data/` 目录本身不对外提供静态访问，服务器上要取文件请从宿主机 `data/` 目录拿。

生产部署：`npm run build` 生成 `frontend/dist`，Go 后端会自动把它当静态站点托管
（同一个 8080 端口既提供页面也提供 API），此时无需单独跑前端 dev server。

环境变量（可选）：`BILL_DATA_DIR`（模板/报价表目录，也是合并结果的输出目录）、
`BILL_ADDR`（监听地址，默认 `:8080`）、`BILL_JOB_DIR`（上传/生成文件的临时目录）、
`BILL_BROWSE_ROOT`（服务器端选文件 / 填路径的浏览根目录，默认等于 `BILL_DATA_DIR`）。

### 已知差异

- 为了简化实现，日志/报价表统一整份读入内存（原 Python 版对超大 xlsx 用了流式读取），
  正常量级的月度日志没有问题。
- `book.Discounts`（报价表第二个 sheet 的分组折扣）与原 Python 版一样，加载后当前并未在计费逻辑中使用。

---

## 部署到服务器（Docker Compose + GHCR）

代码推送到 `main` 分支后，GitHub Actions（[.github/workflows/docker-publish.yml](.github/workflows/docker-publish.yml)）
会自动构建 Docker 镜像并推送到 `ghcr.io/huguanjin/wc002-taidong-billtool`。

服务器上准备好 [docker-compose.yml](docker-compose.yml)、`.env`（从 [.env.example](.env.example) 复制并修改账号密码）
和 `data/` 下的模板文件后，两条命令即可启动：

```bash
docker compose pull
docker compose up -d
```

完整步骤（含 GHCR 登录、HTTPS 反向代理、备份、常见问题排查）见 **[DEPLOY.md](DEPLOY.md)**。

---

## 旧版命令行工具（Python，仍保留作参考/备用）

## 目录结构

```
钛动账单工具包/
├── log_to_bill.py              # 主程序
├── extract_cache_columns.py    # 从 other 解析缓存 token
├── data/
│   ├── bill_template.xlsx      # 账单 Excel 模板
│   └── price_table.xlsx        # 官方美元刊例价
├── 计费规则.md                 # GPT/Gemini 等计费细则
├── requirements.txt
└── README.md
```

---

## 环境

- Python 3.10+
- 依赖：`pip install -r requirements.txt`

---

## 快速使用

在工具包目录下打开终端：

```powershell
# 从日志生成账单 + 脱敏日志（推荐）
python log_to_bill.py -i "你的日志.xlsx"

# CSV / TSV 日志也行（根据后缀名自动识别分隔符，.csv 但实际是 Tab 分隔也能识别）
python log_to_bill.py -i "你的日志.csv"
python log_to_bill.py -i "你的日志.tsv"

# 指定输出路径
python log_to_bill.py -i "钛动8月日志.xlsx" -o "钛动8月账单.xlsx"

# 指定账期月份 / 汇率（默认汇率 7）
python log_to_bill.py -i "日志.xlsx" --month 8 --exchange-rate 7

# 强制统一折扣（默认按分组自动反推）
python log_to_bill.py -i "日志.xlsx" --discount 0.357
```

默认会：

- 输出账单：把文件名里的「日志」换成「账单」
- 输出脱敏日志：把文件名里的「日志」换成「脱敏日志」

---

## 计算方式（核心）

### 1. 结算金额（人民币）——对客户收多少钱

```
结算金额（人民币）= quota ÷ 500000
```

按 **(模型, 分组)** 汇总日志里的 `quota`，再除以 50 万。

### 2. 总金额 / 刊例（人民币）——按官方价算的标价

```
刊例美金 = 按请求累计 (
  非缓存×input + 缓存读×cache价 + 输出×output
  + 写5m×input×1.25 + 写1h×input×2
  + web_search
) / 1e6

总金额（人民币）= ROUND(刊例美金, 4) × 汇率(默认7)
```

### 3. 折扣

```
分组折扣 = 该组结算人民币合计 ÷ 该组总金额人民币合计
```

保留 3 位小数。也可用 `--discount` 强制指定。

### 4. Token 怎么拆（GPT / Gemini）

日志里的 `prompt_tokens` **含缓存**，需拆开：

```
未命中 = prompt − cache读 − cache写5m − cache写1h
输出   = completion
缓存读 / 写 = 从 other 或独立列解析
```

Claude：`prompt_tokens` 已是未命中量，不再减。

### 5. 阶梯价（按单条请求的 prompt 长度）

| 模型 | 低档条件 | 低档 $/M | 高档 $/M |
|------|----------|----------|----------|
| gpt-5.4 | &lt; 272000 | 2.5 / 15 / 0.25 | 5 / 22.5 / 0.5 |
| gpt-5.5 | &lt; 272000 | 5 / 30 / 0.5 | 10 / 45 / 1 |
| gemini-3.1-pro-preview | ≤ 200000 | 2 / 12 / 0.2 | 4 / 18 / 0.4 |

必须**逐条请求**判档后再汇总，不能先加总 token 再乘一个单价。

更多细则见 `计费规则.md`。

---

## 日志需要哪些列

至少包含：

| 列名 | 用途 |
|------|------|
| model_name | 模型 |
| group | 分组 |
| prompt_tokens | 输入 token |
| completion_tokens | 输出 token |
| quota | 站点扣费额度 |
| other 或独立 cache 列 | 缓存读写 |

可选：`created_at`、`username`、`request_id` 等。

---

## 常用参数

| 参数 | 说明 |
|------|------|
| `-i` / `--input` | 日志路径（必填） |
| `-o` / `--output` | 账单输出路径 |
| `--template` | 账单模板（默认 `data/bill_template.xlsx`） |
| `--price-table` | 报价表（默认 `data/price_table.xlsx`） |
| `--month` | 账期月份 |
| `--exchange-rate` | 美金→人民币汇率，默认 7 |
| `--discount` | 强制统一折扣 |
| `--no-sanitized-log` | 不生成脱敏日志 |
| `--keep-log` | 把原日志附带到账单文件里 |

---

## 钛动历史账期参考（结算人民币 ÷ 7 ≈ 美元）

| 时段 | 结算 CNY | 约合 USD |
|------|----------|----------|
| 7/1–7/31 | — | ≈ $51,591 |
| 8/1–3 | — | ≈ $64,237 |
| 8/4–11 | — | ≈ $193,033 |
| 8/12–15 | ≈ ¥437,961 | ≈ $62,566 |

（具体以当次日志 quota 为准。）
