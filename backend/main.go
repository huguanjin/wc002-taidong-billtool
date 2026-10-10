package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"taidong-bill-backend/billing"
)

var (
	dataDir      string
	jobDir       string
	frontendDist string
	dbConfig     *billing.DBConfig // 为空表示未配置业务数据库，PriceSourceDB 不可用
	pgConfig     *billing.PGConfig // 为空表示未配置本地 PostgreSQL，客户折扣拉取功能不可用
)

type jobRecord struct {
	billPath      string
	sanitizedPath string
	mergedPath    string
	exportedPath  string
	costPath      string
	createdAt     time.Time
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]jobRecord{}
)

func main() {
	root, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	dataDir = filepath.Join(root, "..", "data")
	if v := os.Getenv("BILL_DATA_DIR"); v != "" {
		dataDir = v
	}
	jobDir = filepath.Join(os.TempDir(), "taidong-bill-jobs")
	if v := os.Getenv("BILL_JOB_DIR"); v != "" {
		jobDir = v
	}
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		log.Fatal(err)
	}
	frontendDist = filepath.Join(root, "..", "frontend", "dist")
	if v := os.Getenv("BILL_FRONTEND_DIST"); v != "" {
		frontendDist = v
	}

	initAuth()
	initBrowseRoot()
	initDBConfig()
	initPGConfig()
	if pgConfig != nil {
		if err := billing.EnsureUserDiscountSchema(*pgConfig); err != nil {
			log.Printf("警告: 初始化 PostgreSQL 折扣快照表失败，客户折扣拉取功能可能不可用: %v", err)
		}
		if err := billing.EnsureChannelSchema(*pgConfig); err != nil {
			log.Printf("警告: 初始化渠道倍率表失败，成本估算功能可能不可用: %v", err)
		}
		if err := billing.EnsureChannelModelBillingSchema(*pgConfig); err != nil {
			log.Printf("警告: 初始化上游计费方式表失败，严格区分按次计费功能可能不可用: %v", err)
		}
		if err := billing.EnsureCustomerSchema(*pgConfig); err != nil {
			log.Printf("警告: 初始化客户信息表失败，客户管理与账单任务功能可能不可用: %v", err)
		}
		// 任务表有外键指向 customers，必须在客户表之后建。
		if err := billing.EnsureBillTaskSchema(*pgConfig); err != nil {
			log.Printf("警告: 初始化账单任务表失败，账单任务功能可能不可用: %v", err)
		}
		// 手工折扣表也有外键指向 customers，同样必须在客户表之后建。
		if err := billing.EnsureCustomerGroupDiscountSchema(*pgConfig); err != nil {
			log.Printf("警告: 初始化客户手工折扣表失败，手工折扣功能可能不可用: %v", err)
		}
		if err := billing.EnsureSettingsSchema(*pgConfig); err != nil {
			log.Printf("警告: 初始化默认出账参数表失败，账单任务功能可能不可用: %v", err)
		}
	}

	go cleanupOldJobs()
	go cleanupSessions()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", withCORS(handleLogin))
	mux.HandleFunc("/api/logout", withCORS(handleLogout))
	mux.HandleFunc("/api/session", withCORS(handleSession))
	mux.HandleFunc("/api/bill", withCORS(requireAuth(handleGenerateBill)))
	mux.HandleFunc("/api/merge-logs", withCORS(requireAuth(handleMergeLogs)))
	mux.HandleFunc("/api/pull-db-prices", withCORS(requireAuth(handlePullDBPrices)))
	mux.HandleFunc("/api/pull-user-discount", withCORS(requireAuth(handlePullUserDiscount)))
	mux.HandleFunc("/api/check-prices", withCORS(requireAuth(handleCheckMissingPrices)))
	mux.HandleFunc("/api/log-groups", withCORS(requireAuth(handleLogGroups)))
	mux.HandleFunc("/api/export-logs", withCORS(requireAuth(handleExportLogs)))
	mux.HandleFunc("/api/data-logs", withCORS(requireAuth(handleDataLogs)))
	mux.HandleFunc("/api/delete-log-file", withCORS(requireAuth(handleDeleteLogFile)))
	mux.HandleFunc("/api/pull-channels", withCORS(requireAuth(handlePullChannels)))
	mux.HandleFunc("/api/channels", withCORS(requireAuth(handleChannels)))
	mux.HandleFunc("/api/channel-ratios", withCORS(requireAuth(handleSaveChannelRatios)))
	mux.HandleFunc("/api/channel-model-billing", withCORS(requireAuth(handleChannelModelBilling)))
	mux.HandleFunc("/api/save-channel-model-billing", withCORS(requireAuth(handleSaveChannelModelBilling)))
	mux.HandleFunc("/api/check-channels", withCORS(requireAuth(handleCheckChannels)))
	mux.HandleFunc("/api/customers", withCORS(requireAuth(handleCustomers)))
	mux.HandleFunc("/api/delete-customer", withCORS(requireAuth(handleDeleteCustomer)))
	mux.HandleFunc("/api/group-discounts", withCORS(requireAuth(handleGroupDiscounts)))
	mux.HandleFunc("/api/save-group-discounts", withCORS(requireAuth(handleSaveGroupDiscounts)))
	mux.HandleFunc("/api/bill-tasks", withCORS(requireAuth(handleBillTasks)))
	mux.HandleFunc("/api/save-bill-task", withCORS(requireAuth(handleSaveBillTask)))
	mux.HandleFunc("/api/validate-bill-tasks", withCORS(requireAuth(handleValidateBillTasks)))
	mux.HandleFunc("/api/run-bill-tasks", withCORS(requireAuth(handleRunBillTasks)))
	mux.HandleFunc("/api/delete-bill-task", withCORS(requireAuth(handleDeleteBillTask)))
	mux.HandleFunc("/api/bill-task-settings", withCORS(requireAuth(handleBillTaskSettings)))
	mux.HandleFunc("/api/download/", withCORS(requireAuth(handleDownload)))
	mux.HandleFunc("/api/browse", withCORS(requireAuth(handleBrowse)))
	mux.HandleFunc("/api/health", withCORS(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	if _, err := os.Stat(frontendDist); err == nil {
		mux.Handle("/", http.FileServer(http.Dir(frontendDist)))
	}

	addr := ":8080"
	if v := os.Getenv("BILL_ADDR"); v != "" {
		addr = v
	}
	log.Printf("监听 %s，数据目录: %s", addr, dataDir)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// initDBConfig 从环境变量读取业务数据库连接信息；BILL_DB_HOST 为空表示未配置，dbConfig 保持 nil。
func initDBConfig() {
	host := os.Getenv("BILL_DB_HOST")
	if host == "" {
		return
	}
	dbConfig = &billing.DBConfig{
		Host:     host,
		Port:     os.Getenv("BILL_DB_PORT"),
		User:     os.Getenv("BILL_DB_USER"),
		Password: os.Getenv("BILL_DB_PASSWORD"),
		DBName:   os.Getenv("BILL_DB_NAME"),
		Table:    os.Getenv("BILL_DB_TABLE"),
		// 消费日志表（通常是 logs），与 options 表同实例、表名独立配置。
		LogTable: os.Getenv("BILL_DB_LOG_TABLE"),
	}
}

// initPGConfig 从环境变量读取本地 PostgreSQL 连接信息（客户折扣快照存储）；
// BILL_PG_HOST 为空表示未配置，pgConfig 保持 nil。这是一个和业务库完全独立的数据库。
func initPGConfig() {
	host := os.Getenv("BILL_PG_HOST")
	if host == "" {
		return
	}
	pgConfig = &billing.PGConfig{
		Host:     host,
		Port:     os.Getenv("BILL_PG_PORT"),
		User:     os.Getenv("BILL_PG_USER"),
		Password: os.Getenv("BILL_PG_PASSWORD"),
		DBName:   os.Getenv("BILL_PG_DBNAME"),
		SSLMode:  os.Getenv("BILL_PG_SSLMODE"),
	}
}

// dbPriceCachePath 手动拉取数据库价格落盘的位置，与 data 目录一起挂载，重启容器后仍可读取。
func dbPriceCachePath() string {
	return filepath.Join(dataDir, "db_price_cache.json")
}

// loadPriceBookForSource 按价格来源加载 PriceBook，GenerateBill 与 handleCheckMissingPrices 共用。
// 第二个返回值为阶梯表达式配置（仅「数据库实时价格」模式有，其余为 nil）。
func loadPriceBookForSource(source billing.PriceSource, priceTablePath string) (*billing.PriceBook, *billing.BillingExprSetting, error) {
	if source == billing.PriceSourceDB {
		book, expr, _, err := billing.LoadDBPriceCache(dbPriceCachePath())
		return book, expr, err
	}
	book, err := billing.LoadPriceBook(priceTablePath)
	if err != nil {
		return nil, nil, fmt.Errorf("加载报价表失败: %w", err)
	}
	return book, nil, nil
}

// handleCheckMissingPrices 出账前预检：列出日志里出现的模型在当前价格来源下有没有找不到定价的，
// 不写任何文件，处理完立即清理临时上传的日志文件。
func handleCheckMissingPrices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}

	jobID := newJobID()
	jobPath := filepath.Join(jobDir, jobID)
	if err := os.MkdirAll(jobPath, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(jobPath)

	inputPath, err := resolveInputFile(r, jobPath)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	form := r.MultipartForm.Value
	priceSource := billing.PriceSource(formValue(form, "priceSource"))
	sheet := formValue(form, "sheet")
	encoding := formValue(form, "encoding")

	priceTablePath := filepath.Join(dataDir, "price_table.xlsx")
	book, exprSetting, err := loadPriceBookForSource(priceSource, priceTablePath)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	preferPriceTable := priceSource == billing.PriceSourcePriceTable || priceSource == billing.PriceSourceDB

	exchangeRate := billing.DefaultExchangeRate
	if v := formValue(form, "exchangeRate"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			exchangeRate = f
		}
	}

	headers, rows, err := billing.LoadLogRows(inputPath, sheet, encoding)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "读取日志失败: "+err.Error())
		return
	}
	models, err := billing.ExtractDistinctModels(headers, rows)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// 阶梯表达式模型在 ModelRatio/ModelPrice 里通常查不到条目，但表达式就是它的定价，
	// 这类模型不该报「缺少定价」，单独列出来供人工核对表达式。
	var missingModels, exprModels []string
	for _, model := range models {
		if exprSetting.HasExpr(model) {
			exprModels = append(exprModels, model)
			continue
		}
		if price, _ := billing.ResolvePrice(model, book, preferPriceTable, exchangeRate); price == nil {
			missingModels = append(missingModels, model)
		}
	}

	// 分组标识同样去重返回：前端按它渲染勾选框，让用户勾出国产/站内定价的分组。
	// 分组名取不到不是致命错误（老日志可能没有 group 列），降级成空列表即可。
	groups, gerr := billing.ExtractDistinctGroups(headers, rows)
	if gerr != nil {
		groups = nil
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"modelCount":    len(models),
		"missingModels": missingModels,
		"exprModels":    exprModels,
		"groups":        groups,
	})
}

// handleLogGroups 只读日志的 group 列并去重返回，供前端在选定日志后直接列出分组勾选。
// 不加载价格、不算聚合，出账前的勾选不该依赖「检查模型价格覆盖」是否点过。
func handleLogGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}

	jobID := newJobID()
	jobPath := filepath.Join(jobDir, jobID)
	if err := os.MkdirAll(jobPath, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(jobPath)

	inputPath, err := resolveInputFile(r, jobPath)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	form := r.MultipartForm.Value
	headers, rows, err := billing.LoadLogRows(inputPath, formValue(form, "sheet"), formValue(form, "encoding"))
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "读取日志失败: "+err.Error())
		return
	}
	groups, err := billing.ExtractDistinctGroups(headers, rows)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"groups":   groups,
		"rowCount": len(rows),
	})
}

// handleExportLogs 按时间段 + 账号从业务库 logs 表导出消费日志为 tsv，
// 等价于在服务器上手动跑 mysql -e "SELECT ... > xxx.tsv"，省去人工导出步骤。
// 导出文件落在 data 目录，可直接作为「生成账单」或「日志合并」的输入。
func handleExportLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置业务数据库连接信息（BILL_DB_HOST 等环境变量）")
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		// 导出表单只有文本字段，不带文件；非 multipart 时退回解析普通表单。
		if perr := r.ParseForm(); perr != nil {
			httpError(w, http.StatusBadRequest, "解析表单失败: "+perr.Error())
			return
		}
	}

	form := formValues(r)
	usernames := splitList(form["usernames"])
	if len(usernames) == 0 && len(splitList(form["userIds"])) == 0 {
		httpError(w, http.StatusBadRequest, "请至少填写一个客户账号或用户 ID")
		return
	}
	userIDs, err := parseIntList(splitList(form["userIds"]))
	if err != nil {
		httpError(w, http.StatusBadRequest, "用户 ID 必须是整数: "+err.Error())
		return
	}

	startRaw := exportTimeField(form, "startAt", "startDate")
	endRaw := exportTimeField(form, "endAt", "endDate")
	if startRaw == "" || endRaw == "" {
		httpError(w, http.StatusBadRequest, "请填写开始时间与结束时间")
		return
	}
	// 时间按北京时间（+08:00）解释，与容器时区无关——容器通常是 UTC，
	// 用 time.Local 会整体偏 8 小时、把客户账期错切一天。
	start, end, err := billing.ResolveExportRange(startRaw, endRaw)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if span := end.Sub(start); span > time.Duration(billing.MaxExportDays)*24*time.Hour {
		httpError(w, http.StatusBadRequest,
			fmt.Sprintf("时间跨度 %.1f 天超过上限 %d 天，请分次导出", span.Hours()/24, billing.MaxExportDays))
		return
	}

	result, err := billing.ExportLogsFromDB(*dbConfig, dataDir, billing.LogExportParams{
		Usernames:     usernames,
		UserIDs:       userIDs,
		IncludeUserID: form["includeUserId"] == "true",
		StartTime:     start,
		EndTime:       end,
	})
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	jobID := newJobID()
	jobsMu.Lock()
	jobs[jobID] = jobRecord{exportedPath: result.Path, createdAt: time.Now()}
	jobsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobId":            jobID,
		"exportedFileName": filepath.Base(result.Path),
		"exportedUrl":      "/api/download/" + jobID + "/exported",
		"exportedPath":     result.Path,
		"rowCount":         result.RowCount,
		"elapsedSeconds":   result.FinishedAt.Sub(result.StartedAt).Seconds(),
	})
}

// exportTimeField 取时间字段：优先用新字段名，同时兼容旧字段名
// （浏览器里缓存着旧版前端时仍会提交 startDate/endDate）。
func exportTimeField(form map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(form[k]); v != "" {
			return v
		}
	}
	return ""
}

// formValues 把 multipart 或普通表单统一成一个 map[string]string。
func formValues(r *http.Request) map[string]string {
	out := map[string]string{}
	if r.MultipartForm != nil {
		for k, v := range r.MultipartForm.Value {
			if len(v) > 0 {
				out[k] = v[0]
			}
		}
	}
	for k, v := range r.Form {
		if _, exists := out[k]; !exists && len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// splitList 把多行/逗号分隔的输入拆成去重后的列表。
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == '，' || r == ' ' || r == '\t'
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		v := strings.TrimSpace(f)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// parseIntList 把字符串列表解析成整数列表。
func parseIntList(items []string) ([]int, error) {
	out := make([]int, 0, len(items))
	for _, s := range items {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("%q 不是整数", s)
		}
		out = append(out, n)
	}
	return out, nil
}

// handleDataLogs 列出 data 目录下由本工具导出的日志文件，供页面查看与清理。
func handleDataLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	files, err := billing.ListDataLogs(dataDir)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files":   files,
		"dataDir": dataDir,
	})
}

// handleDeleteLogFile 删除一个导出的日志文件。
// 只允许删 data 目录下「日志查询_」前缀的 tsv/csv/xlsx，
// 账单模板、报价表、价格缓存不在可删范围内。
func handleDeleteLogFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 必须走 ParseMultipartForm：前端用 FormData 提交，content-type 是
	// multipart/form-data，而 ParseForm 不会解析 multipart body，
	// 那样读出来的 name 永远是空串。ParseMultipartForm 内部会先调 ParseForm，
	// 所以 urlencoded 的提交也一并支持。
	_ = r.ParseMultipartForm(1 << 20)
	_ = r.ParseForm()

	name := strings.TrimSpace(r.FormValue("name"))
	removed, err := billing.DeleteDataLog(dataDir, name)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 把已删文件从任务记录里摘掉，避免下载接口继续指向不存在的路径。
	jobsMu.Lock()
	for id, rec := range jobs {
		if rec.exportedPath == removed {
			delete(jobs, id)
		}
	}
	jobsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": filepath.Base(removed)})
}

// ============================================================
// 客户信息 + 账单导出任务
//
// 这一组接口本身不带文件上传，一律用 JSON body（与 /api/channel-ratios 一致），
// 所以不涉及 ParseMultipartForm。
// ============================================================

// decodeJSONBody 解析 JSON 请求体。带请求大小上限，避免异常请求把内存吃满。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		httpError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return false
	}
	return true
}

// handleCustomers 客户信息：GET 列表，POST 新增或更新。
func handleCustomers(w http.ResponseWriter, r *http.Request) {
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*），客户信息不可用")
		return
	}

	switch r.Method {
	case http.MethodGet:
		customers, err := billing.ListCustomers(*pgConfig)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"customers": customers})

	case http.MethodPost:
		var in billing.Customer
		if !decodeJSONBody(w, r, &in) {
			return
		}
		saved, err := billing.UpsertCustomer(*pgConfig, in)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"customer": saved})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDeleteCustomer 删除客户。任务表上是 ON DELETE CASCADE，
// 该客户的历史任务会一并删除——页面上必须先显示会连带删掉几条。
func handleDeleteCustomer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}

	var in struct {
		ID int64 `json:"id"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	if in.ID <= 0 {
		httpError(w, http.StatusBadRequest, "缺少客户 ID")
		return
	}
	if err := billing.DeleteCustomer(*pgConfig, in.ID); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": in.ID})
}

// handleGroupDiscounts 读「客户 + 分组」的手工折扣，并在给了日志时一并解析出
// 该日志里出现过的分组及各自当前的自动折扣。
//
// 两个用途合成一个接口：结算人员的工作流是「选客户 → 选一份这个时段的日志 →
// 看有哪些分组、自动算成了多少 → 填上实际谈定的折扣」。分成两个接口的话，
// 前端要自己把两份数据按分组名拼起来，而拼接口径（分组名是否带倍率后缀）
// 正是最容易搞错的地方，放在后端做只有一处实现。
func handleGroupDiscounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*），手工折扣功能不可用")
		return
	}

	customerID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("customerId")), 10, 64)
	if err != nil || customerID <= 0 {
		httpError(w, http.StatusBadRequest, "缺少客户 ID")
		return
	}

	saved, err := billing.ListCustomerGroupDiscounts(*pgConfig, customerID)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	manual := make(map[string]float64, len(saved))
	notes := make(map[string]string, len(saved))
	for _, row := range saved {
		manual[row.GroupKey] = row.Discount
		notes[row.GroupKey] = row.Note
	}

	resp := map[string]interface{}{
		"saved":  saved,
		"groups": []billing.GroupDiscountPreview{},
	}

	// 没给日志时只返回已维护的折扣：页面刚打开、还没选日志的情况下也要能显示现状。
	sourcePath := strings.TrimSpace(r.URL.Query().Get("logPath"))
	if sourcePath == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	path, err := resolveInBrowseRoot(sourcePath)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	preview, err := billing.PreviewGroupDiscounts(path, filepath.Join(dataDir, "price_table.xlsx"),
		dbPriceCachePath(), manualDiscountPreviewParams(), manual)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// 备注落在这里而不是 billing 包：它属于「已维护的配置」，不属于从日志算出来的东西。
	for i := range preview {
		preview[i].Note = notes[preview[i].GroupKey]
	}
	resp["groups"] = preview
	writeJSON(w, http.StatusOK, resp)
}

// manualDiscountPreviewParams 解析日志分组时用的参数。
//
// 取任务页保存的默认出账参数，而不是入账时的那一份：预览只需要价表来源与汇率一致，
// 折扣覆盖（全局折扣、手工折扣）都不参与——页面上那一列要展示的正是
// 「不填手工折扣会算出多少」，见 PreviewGroupDiscounts 的说明。
func manualDiscountPreviewParams() billing.Params {
	if pgConfig == nil {
		return billing.Params{ExchangeRate: billing.DefaultExchangeRate}
	}
	s, err := billing.GetSettings(*pgConfig)
	if err != nil {
		return billing.Params{ExchangeRate: billing.DefaultExchangeRate}
	}
	return billing.Params{
		PriceSource:     billing.PriceSource(s.PriceSource),
		ExchangeRate:    s.ExchangeRate,
		DomesticMarkers: s.DomesticMarkerList(),
	}
}

// handleSaveGroupDiscounts 批量保存某个客户的手工折扣（覆盖式）。
func handleSaveGroupDiscounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*），手工折扣功能不可用")
		return
	}

	var body struct {
		CustomerID int64 `json:"customerId"`
		Items      []struct {
			GroupKey string  `json:"groupKey"`
			Discount *string `json:"discount"` // 前端提交的是原始文本，如「6折」
			Note     string  `json:"note"`
		} `json:"items"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.CustomerID <= 0 {
		httpError(w, http.StatusBadRequest, "缺少客户 ID")
		return
	}

	// 折扣文本在服务端解析，复用处账用的同一套 ParseDiscountText：
	// 前端另写一份解析迟早会与后端不一致（「6折」一边当 0.6 一边当 6）。
	items := make([]billing.GroupDiscountInput, 0, len(body.Items))
	for _, it := range body.Items {
		in := billing.GroupDiscountInput{GroupKey: it.GroupKey, Note: it.Note}
		if it.Discount != nil && strings.TrimSpace(*it.Discount) != "" {
			value, ok := billing.ParseDiscountText(*it.Discount)
			if !ok {
				// 文案里的 %% 是转义：这里是 Sprintf 的格式串，单个 % 会被当成动词，
				// 既通不过 vet，运行时也会打出 %!/(MISSING) 这种乱码给用户看。
				httpError(w, http.StatusBadRequest,
					fmt.Sprintf("分组「%s」的折扣「%s」无法识别，请填 6折 / 60%% / 0.6 这类写法（留空表示恢复自动折扣）",
						it.GroupKey, strings.TrimSpace(*it.Discount)))
				return
			}
			in.Discount = &value
		}
		// 提交了空折扣 = 撤销该分组的手工值，回到自动折扣（in.Discount 保持 nil）。
		items = append(items, in)
	}

	if err := billing.UpsertCustomerGroupDiscounts(*pgConfig, body.CustomerID, items); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 回读一遍再返回：前端拿到的是真正落库的权威状态，而不是自己刚提交的东西。
	saved, err := billing.ListCustomerGroupDiscounts(*pgConfig, body.CustomerID)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"saved": saved,
		"count": len(saved),
	})
}

// handleBillTasks 账单任务列表 + 按月汇总。
//
// 汇总在后端算而不是前端：利润口径（用 costed_settle 而非全部结算额）是正确性关键，
// 放在前端每次都要重新实现一遍，迟早某处写错。
func handleBillTasks(w http.ResponseWriter, r *http.Request) {
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	filter := billing.BillTaskFilter{}
	if v := strings.TrimSpace(q.Get("year")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Year = n
		}
	}
	if v := strings.TrimSpace(q.Get("month")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Month = n
		}
	}
	if v := strings.TrimSpace(q.Get("customerId")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.CustomerID = n
		}
	}

	tasks, err := billing.ListBillTasks(*pgConfig, filter)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	// 汇总始终按当前结果集算：筛了月份就只有那一个月，没筛就是全部账期按月分组。
	summaries := billing.SummarizeTasks(tasks)

	// 前端要用来渲染「默认上月」的账期。
	prevYear, prevMonth := billing.PreviousMonth(time.Now())

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tasks":             tasks,
		"summaries":         summaries,
		"defaultYear":       prevYear,
		"defaultMonth":      prevMonth,
		"jobRetentionHours": jobRetentionHours,
	})
}

// taskInput 新建/编辑计划的请求体。
//
// 时段用两个时间字符串：前端是 <input type="datetime-local" step="1">，
// 精确到秒。ParseExportTime 也能接受只有日期的形态（如 "2026-09-30"），
// 那时结束时间会补到当天 23:59:59——与手动导出的区间语义一致。
type taskInput struct {
	ID                int64  `json:"id"` // 0 = 新建
	CustomerID        int64  `json:"customerId"`
	Name              string `json:"name"`
	StartAt           string `json:"startAt"`
	EndAt             string `json:"endAt"`
	GenerateSanitized bool   `json:"generateSanitized"`
	GenerateCost      bool   `json:"generateCost"`
	// CheckCost 用指针：新建时「没传」应当取默认值 true（对应页面上的默认勾选），
	// 而 false 是用户明确取消勾选。用值类型的话两者分不开，
	// 新建出来的计划会默认关掉成本核算——与「默认勾选」正好相反。
	CheckCost *bool `json:"checkCost"`
	// UseManualDiscount 用指针：false 是**有效值**（明确不套用线下折扣），
	// 所以「没传这个字段」与「传了 false」必须区分开。用值类型的话，
	// 老版本前端编辑一次计划就会把用户勾上的线下折扣悄悄取消——
	// 而折扣直接决定收客户多少钱。与 CheckCost 同一个理由。
	UseManualDiscount *bool `json:"useManualDiscount"`
	// ReviewUpstream 用指针：false 是有效值（明确不要每次弹核对窗），
	// 「没传」与「传了 false」必须区分——老版本前端编辑一次计划不带这个字段，
	// 用值类型的话会把用户勾上的核对开关悄悄取消。与 CheckCost 同一个理由。
	ReviewUpstream *bool `json:"reviewUpstream"`
	// StrictPerCall 用指针：false 是有效值，「没传」与「传了 false」必须区分
	// （老前端编辑一次计划不该悄悄取消已勾的开关）。与 ReviewUpstream 同一个理由。
	StrictPerCall *bool `json:"strictPerCall"`
	// BillTemplate 用指针：空串是**有效值**（= 标准模板），所以「没传这个字段」
	// 与「传了空串」必须区分开。否则老版本前端编辑一次计划，就会把用户选的
	// 简易模板重置成标准模板——而且不会有任何提示。
	BillTemplate *string `json:"billTemplate"`
}

// handleSaveBillTask 新建或编辑一条账单计划。
//
// 只写计划字段，不碰上次的执行结果——改个时段或名字不该把已经出过的账清掉。
func handleSaveBillTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}

	var in taskInput
	if !decodeJSONBody(w, r, &in) {
		return
	}
	if in.CustomerID <= 0 {
		httpError(w, http.StatusBadRequest, "请选择客户")
		return
	}

	// 解析时段。两个都空是允许的（先建计划、后补时段），
	// 但只填一个没意义——查库区间缺一端。
	hasStart := strings.TrimSpace(in.StartAt) != ""
	hasEnd := strings.TrimSpace(in.EndAt) != ""
	if hasStart != hasEnd {
		httpError(w, http.StatusBadRequest, "开始时间与结束时间必须同时填写")
		return
	}

	task := billing.BillTask{
		ID:                in.ID,
		CustomerID:        in.CustomerID,
		Name:              strings.TrimSpace(in.Name),
		GenerateSanitized: in.GenerateSanitized,
		GenerateCost:      in.GenerateCost,
		// 没传就默认开启成本核算。这是「默认勾选」的实现点：
		// 前端的勾选框初值是 true，但老版本前端不带这个字段，这里兜底。
		CheckCost: true,
	}
	if in.CheckCost != nil {
		task.CheckCost = *in.CheckCost
	}
	// 没传就默认**不套用**线下折扣（新建计划的默认值）。
	// 手工折扣是人工维护的一套数值，直接决定收款金额，不该默认生效。
	if in.UseManualDiscount != nil {
		task.UseManualDiscount = *in.UseManualDiscount
	}
	// 没传就默认不弹核对窗：它会打断每一次执行，不该在用户没表态时开启。
	if in.ReviewUpstream != nil {
		task.ReviewUpstream = *in.ReviewUpstream
	}
	if in.StrictPerCall != nil {
		task.StrictPerCall = *in.StrictPerCall
	}
	if in.BillTemplate != nil {
		task.BillTemplate = strings.TrimSpace(*in.BillTemplate)
	}

	if hasStart && hasEnd {
		// ResolveExportRange 复用导出日志的同一套解析：一律按 +08:00，
		// 给完整时刻就按秒精确；只给日期则结束日补到当天 23:59:59（含）。
		// 它只校验起止顺序，跨度上限由 billing 侧的 validatePlanRange 兜。
		start, end, err := billing.ResolveExportRange(in.StartAt, in.EndAt)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		task.StartTime = &start
		task.EndTime = &end
		// 归属账期由开始日推导（跨月任务整个计入开始月）。
		task.PeriodYear, task.PeriodMonth = billing.PeriodFromStartTime(start)
	}

	// 客户名存成冗余快照，让计划列表在客户改名后仍读得懂。
	customer, err := billing.GetCustomer(*pgConfig, in.CustomerID)
	if err != nil {
		httpError(w, http.StatusBadRequest, "客户不存在，请先在客户信息页新增")
		return
	}
	task.CustomerName = customer.Name

	if in.ID == 0 {
		saved, err := billing.CreateBillTask(*pgConfig, task)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"task": saved})
		return
	}

	// 编辑：时段为空时不清掉已有时段（前端没传就意味着不改这两项之外的字段）。
	// 但 PeriodYear/Month 必须保留，否则按归属账期筛选会漏掉这条计划。
	if task.StartTime == nil {
		existing, err := billing.GetBillTask(*pgConfig, in.ID)
		if err != nil {
			httpError(w, http.StatusBadRequest, "任务不存在（可能已被删除）")
			return
		}
		task.StartTime = existing.StartTime
		task.EndTime = existing.EndTime
		task.PeriodYear = existing.PeriodYear
		task.PeriodMonth = existing.PeriodMonth
	}
	// 模板同理：没传就保留库里的值，别把用户选的简易模板悄悄重置成标准模板。
	// 成本核算开关一起处理：没传时若走默认值 true，会把用户明确取消的勾选又打开。
	if in.BillTemplate == nil || in.CheckCost == nil || in.UseManualDiscount == nil || in.ReviewUpstream == nil || in.StrictPerCall == nil {
		if existing, err := billing.GetBillTask(*pgConfig, in.ID); err == nil {
			if in.BillTemplate == nil {
				task.BillTemplate = existing.BillTemplate
			}
			if in.CheckCost == nil {
				task.CheckCost = existing.CheckCost
			}
			if in.UseManualDiscount == nil {
				task.UseManualDiscount = existing.UseManualDiscount
			}
			if in.ReviewUpstream == nil {
				task.ReviewUpstream = existing.ReviewUpstream
			}
			if in.StrictPerCall == nil {
				task.StrictPerCall = existing.StrictPerCall
			}
		}
	}
	if err := billing.UpdateBillTask(*pgConfig, task); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := billing.GetBillTask(*pgConfig, in.ID)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"task": updated})
}

// loadTasksAndCustomers 取计划与客户表，供预检与批量执行共用。
//
// 客户一次查好装进 map：预检要逐条看客户有没有配账号，
// 每条各查一次库就是 N+1。
func loadTasksAndCustomers(ids []int64) ([]billing.BillTask, map[int64]billing.Customer, error) {
	tasks := make([]billing.BillTask, 0, len(ids))
	for _, id := range ids {
		t, err := billing.GetBillTask(*pgConfig, id)
		if err != nil {
			return nil, nil, fmt.Errorf("任务 %d 不存在（可能已被删除）", id)
		}
		tasks = append(tasks, t)
	}

	list, err := billing.ListCustomers(*pgConfig)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]billing.Customer, len(list))
	for _, c := range list {
		byID[c.ID] = c
	}
	return tasks, byID, nil
}

// handleValidateBillTasks 批量执行前的整体预检。
//
// 一次把问题列全再让用户决定：批量执行是同步的，跑到第 4 条才报错，
// 前 3 条已经真地导了日志、出了账。
func handleValidateBillTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}

	var in struct {
		TaskIDs []int64 `json:"taskIds"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	if len(in.TaskIDs) == 0 {
		httpError(w, http.StatusBadRequest, "请先勾选要执行的任务")
		return
	}

	tasks, customers, err := loadTasksAndCustomers(in.TaskIDs)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	// 渠道倍率只影响「勾了成本利润表」的计划，读不到时按空表处理即可——
	// 读失败不该让整个预检挂掉，那会变成「因为读不到倍率所以什么都不让跑」。
	ratios, err := billing.ChannelRatioMap(*pgConfig)
	if err != nil {
		log.Printf("警告: 预检时读取渠道倍率失败，成本相关检查将按「未维护」处理: %v", err)
		ratios = map[int]float64{}
	}

	validations := billing.ValidateTasksForRun(tasks, customers, ratios)
	blocked := 0
	for _, v := range validations {
		if !v.OK() {
			blocked++
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"validations": validations,
		"runnable":    len(validations) - blocked,
		"blocked":     blocked,
	})
}

// handleRunBillTasks 批量执行：一次请求顺序跑完所有勾选的任务。
//
// 同步执行（可能几分钟），不引入任务队列：出账结果是用户马上要下载的东西，
// 排到后台再回来找反而更麻烦。
//
// **单个任务失败不影响其他任务**：逐条跑，失败的把原因记进该条结果，
// 已成功的那几条照常落库。这也是「失败不落库」约定的延伸——
// 失败的那条保持原样，不会把上次的好数字冲掉。
func handleRunBillTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}
	if dbConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置业务数据库连接信息（BILL_DB_HOST 等环境变量），无法自动导出日志")
		return
	}

	var in struct {
		TaskIDs []int64 `json:"taskIds"`
		// SkipUpstreamReview 本次请求里的任务都跳过「核对上游倍率与国模标识」。
		// 前端只在用户已经在核对弹窗里点过「继续」之后的那次重跑里置位——
		// 否则重跑会再次停在同一个弹窗上。它只对这一次请求有效，不落库。
		SkipUpstreamReview bool `json:"skipUpstreamReview"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	if len(in.TaskIDs) == 0 {
		httpError(w, http.StatusBadRequest, "请先勾选要执行的任务")
		return
	}

	results := make([]map[string]interface{}, 0, len(in.TaskIDs))
	okCount, failCount := 0, 0

	for _, id := range in.TaskIDs {
		one := map[string]interface{}{"taskId": id}

		task, err := billing.GetBillTask(*pgConfig, id)
		if err != nil {
			one["ok"] = false
			one["error"] = fmt.Sprintf("任务 %d 不存在（可能已被删除）", id)
			results = append(results, one)
			failCount++
			continue
		}
		one["taskName"] = task.DisplayName()

		res, err := runOneBillTask(id, in.SkipUpstreamReview)
		if err != nil {
			one["ok"] = false
			one["error"] = err.Error()
			results = append(results, one)
			failCount++
			continue
		}

		// 成本核算预检拦下：没有产物，但**也不是失败**——用户补录倍率后重跑即可。
		// 单独一个 ok=false + needsChannelRatios 的形态，前端据此渲染补录界面，
		// 而不是混进「执行失败」里让用户以为任务坏了。
		//
		// 「核对上游倍率与国模标识」同理：是用户主动要求的人工确认，不是失败，
		// 但与「缺倍率被拦」是两个不同的界面（前者弹窗列出全部渠道，后者只列缺的），
		// 所以用独立的 needsUpstreamReview 标记，前端靠它分流。
		if res.channelCheck != nil {
			one["ok"] = false
			if res.channelCheck.NeedsUpstreamReview {
				one["needsUpstreamReview"] = true
			} else {
				one["needsChannelRatios"] = true
			}
			one["channelCheck"] = res.channelCheck
			// customerId 必须带上：就地补录线下折扣是按「客户 + 分组」存的，
			// 页面没有客户 ID 就没法提交（见 saveBlockedDiscounts）。
			one["customerId"] = res.task.CustomerID
			one["customerName"] = res.task.CustomerName
			// 弹窗里要说明是哪一条计划、哪个账期：同一批里可能有好几条同时停在核对上。
			one["periodYear"] = res.task.PeriodYear
			one["periodMonth"] = res.task.PeriodMonth
			results = append(results, one)
			// 不计入 failCount：这不是失败，是等待用户输入。
			continue
		}

		one["ok"] = true
		one["task"] = res.task
		one["jobId"] = res.jobID
		one["billFileName"] = res.billFileName
		one["billUrl"] = "/api/download/" + res.jobID + "/bill"
		one["logPath"] = res.logPath
		one["logRowCount"] = res.logRowCount
		one["summary"] = res.summary
		one["summaryPersisted"] = res.persistErr == nil
		if res.sanitizedFileName != "" {
			one["sanitizedFileName"] = res.sanitizedFileName
			one["sanitizedUrl"] = "/api/download/" + res.jobID + "/sanitized"
		}
		if res.costFileName != "" {
			one["costFileName"] = res.costFileName
			one["costUrl"] = "/api/download/" + res.jobID + "/cost"
			one["costSummary"] = res.costSummary
		}
		// 简易账单的可复制文字，与成本摘要互斥（见 handleGenerateBill 的说明）。
		if res.billSummary != "" {
			one["billSummary"] = res.billSummary
		}
		if res.costBlocked {
			one["costBlocked"] = true
			one["missingChannels"] = res.missingChannels
		}
		if len(res.unknownChannelIDs) > 0 {
			one["unknownChannelIds"] = res.unknownChannelIDs
		}
		results = append(results, one)
		okCount++
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results":   results,
		"okCount":   okCount,
		"failCount": failCount,
		// 前端算「折合官方折扣」用：海外渠道 倍率÷这个数，国模渠道直接是倍率。
		// 由后端给而不是页面里再写一个裸 7——换算基数改了，页面不会还停在旧值上。
		"discountBaseFactor": billing.DiscountBaseFactor,
	})
}

// taskRunOutcome 单条任务的执行产出，供批量循环拼响应。
type taskRunOutcome struct {
	jobID             string
	task              billing.BillTask
	billFileName      string
	sanitizedFileName string
	costFileName      string
	costSummary       string
	billSummary       string
	channelCheck      *billing.ChannelCheckResult
	logPath           string
	logRowCount       int64
	summary           billing.Summary
	costBlocked       bool
	missingChannels   []billing.ChannelInfo
	unknownChannelIDs []int
	persistErr        error
}

// runOneBillTask 跑一条计划：出账 → 登记下载 → 落库。
//
// skipUpstreamReview 为真时跳过「核对上游倍率与国模标识」（见 TaskRunDeps.SkipUpstreamReview）。
// 失败时清掉 job 目录并返回 error，**不落库**（上一次的结果保持原样）。
func runOneBillTask(taskID int64, skipUpstreamReview bool) (*taskRunOutcome, error) {
	jobID := newJobID()
	jobPath := filepath.Join(jobDir, jobID)
	if err := os.MkdirAll(jobPath, 0o755); err != nil {
		return nil, err
	}

	result, err := billing.RunBillExportTask(billing.TaskRunDeps{
		DB:                 *dbConfig,
		PG:                 *pgConfig,
		DataDir:            dataDir,
		JobDir:             jobPath,
		TemplatePath:       filepath.Join(dataDir, "bill_template.xlsx"),
		PriceTablePath:     filepath.Join(dataDir, "price_table.xlsx"),
		DBPriceCachePath:   dbPriceCachePath(),
		TaskID:             taskID,
		SkipUpstreamReview: skipUpstreamReview,
	})
	if err != nil {
		_ = os.RemoveAll(jobPath)
		return nil, err
	}

	// 成本核算预检拦下：**没有出账**，只是一个可继续的中间态。
	// 这里既不登记下载（没有产物）、也不落库（结果字段全是零值，
	// 落库会把上一次跑出来的金额覆盖成空），直接把检查结果交回页面补录。
	if result.ChannelCheck != nil {
		_ = os.RemoveAll(jobPath)
		return &taskRunOutcome{
			task:         result.Task,
			channelCheck: result.ChannelCheck,
		}, nil
	}

	// 登记下载。产物在 jobDir 里，6 小时后由 cleanupOldJobs 连同记录一起清掉——
	// 这正是「没下载就要重新执行」的语义。
	jobsMu.Lock()
	jobs[jobID] = jobRecord{
		billPath:      result.BillPath,
		sanitizedPath: result.SanitizedPath,
		costPath:      result.CostPath,
		createdAt:     time.Now(),
	}
	jobsMu.Unlock()

	// 落库：只更新结果列，计划字段保持用户设置的样子。
	task := result.Task
	task.JobID = jobID
	persistErr := billing.SaveBillTaskResult(*pgConfig, task)
	if persistErr != nil {
		// 产物已经生成好了，落库失败不该让用户白跑一趟：
		// 文件链接照常返回，只提示统计没记上。
		log.Printf("警告: 账单任务 %d 落库失败（产物已生成）: %v", taskID, persistErr)
	}

	out := &taskRunOutcome{
		jobID:             jobID,
		task:              task,
		billFileName:      filepath.Base(result.BillPath),
		costSummary:       result.CostSummary,
		billSummary:       result.BillSummary,
		logPath:           result.LogPath,
		logRowCount:       result.LogRowCount,
		summary:           result.Summary,
		costBlocked:       result.CostBlocked,
		missingChannels:   result.MissingChannelInfos,
		unknownChannelIDs: result.UnknownChannelIDs,
		persistErr:        persistErr,
	}
	if result.SanitizedPath != "" {
		out.sanitizedFileName = filepath.Base(result.SanitizedPath)
	}
	if result.CostPath != "" {
		out.costFileName = filepath.Base(result.CostPath)
	}
	return out, nil
}

// handleDeleteBillTask 删除一条任务记录。**只删记录，不删文件**：
// 产物在 jobDir 里由 6 小时清理兜底，源日志在 dataDir 里由「已导出文件」列表管理。
func handleDeleteBillTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}

	var in struct {
		ID int64 `json:"id"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	if in.ID <= 0 {
		httpError(w, http.StatusBadRequest, "缺少任务 ID")
		return
	}
	if err := billing.DeleteBillTask(*pgConfig, in.ID); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": in.ID})
}

// handleBillTaskSettings 默认出账参数：GET 读，POST 存。
func handleBillTaskSettings(w http.ResponseWriter, r *http.Request) {
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s, err := billing.GetSettings(*pgConfig)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"settings": s})

	case http.MethodPost:
		var s billing.BillTaskSettings
		if !decodeJSONBody(w, r, &s) {
			return
		}
		if err := billing.SaveSettings(*pgConfig, s); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		saved, err := billing.GetSettings(*pgConfig)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"settings": saved})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePullChannels 从业务库拉取渠道清单落本地 PostgreSQL。
//
// 业务库只读：这条路径只 SELECT channels，写入全部落在本地 PG。
func handlePullChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置业务数据库连接信息（BILL_DB_HOST 等环境变量）")
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*），渠道清单无处保存")
		return
	}

	pulled, err := billing.PullChannelsFromDB(*dbConfig, *pgConfig)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	channels, err := billing.ListChannels(*pgConfig)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pulled":   pulled,
		"channels": channels,
	})
}

// handleChannels 返回本地渠道清单 + 各自的上游倍率维护状态。
func handleChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*），渠道清单不可用")
		return
	}
	channels, err := billing.ListChannels(*pgConfig)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	// 未维护倍率的数量直接给出，页面不用自己数。
	missing := 0
	for _, c := range channels {
		if c.UpstreamRatio == nil {
			missing++
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"channels":     channels,
		"missingCount": missing,
		// 页面算「折合官方折扣」用，见 handleRunBillTasks 里同名字段的说明。
		"discountBaseFactor": billing.DiscountBaseFactor,
	})
}

// handleChannelModelBilling 读出全部已维护的上游计费方式（渠道 + 模型维度）。
func handleChannelModelBilling(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}
	items, err := billing.ListChannelModelBilling(*pgConfig)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	// 带上渠道名，页面不用再去对渠道清单。
	names := map[int]string{}
	if channels, err := billing.ListChannels(*pgConfig); err == nil {
		for _, c := range channels {
			names[c.ChannelID] = c.Name
		}
	}
	type row struct {
		billing.ChannelModelBillingRow
		ChannelName string `json:"channelName"`
	}
	out := make([]row, 0, len(items))
	for _, it := range items {
		name := names[it.ChannelID]
		if name == "" {
			name = fmt.Sprintf("渠道 %d", it.ChannelID)
		}
		out = append(out, row{it, name})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

// handleSaveChannelModelBilling 保存或删除上游计费方式。
//
// delete 为真时删除该项（回到「未维护」）；否则按 mode 保存。
// 按次的单次费用必须大于 0（见 ChannelModelBillingInput.Validate）。
func handleSaveChannelModelBilling(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}
	var body struct {
		Items []struct {
			billing.ChannelModelBillingInput
			Delete bool `json:"delete"`
		} `json:"items"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if len(body.Items) == 0 {
		httpError(w, http.StatusBadRequest, "没有要保存的项")
		return
	}
	var saves []billing.ChannelModelBillingInput
	for _, it := range body.Items {
		if it.Delete {
			if err := billing.DeleteChannelModelBilling(*pgConfig, it.ChannelID, it.Model); err != nil {
				httpError(w, http.StatusBadGateway, err.Error())
				return
			}
			continue
		}
		saves = append(saves, it.ChannelModelBillingInput)
	}
	if err := billing.UpsertChannelModelBilling(*pgConfig, saves); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"saved": len(saves), "deleted": len(body.Items) - len(saves)})
}

// handleSaveChannelRatios 批量保存渠道上游倍率。
func handleSaveChannelRatios(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}

	var body struct {
		Items []billing.ChannelRatioInput `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.Items) == 0 {
		httpError(w, http.StatusBadRequest, "没有要保存的渠道倍率")
		return
	}
	for _, it := range body.Items {
		if it.UpstreamRatio != nil && *it.UpstreamRatio < 0 {
			httpError(w, http.StatusBadRequest,
				fmt.Sprintf("渠道 %d 的倍率不能为负数", it.ChannelID))
			return
		}
	}
	if err := billing.UpsertChannelRatios(*pgConfig, body.Items); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"saved": len(body.Items)})
}

// handleCheckChannels 出账前预检：读一遍日志，列出用到的渠道及其倍率维护情况。
//
// 与 /api/check-prices 同属「出账前预检」：不写任何账单文件，处理完立即清理临时上传的日志。
// 存在的意义是把「渠道没维护倍率」这个结论提前到出账之前——
// 否则要等账单生成完才在响应里看到 costBlocked，白跑一遍出账。
func handleCheckChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*），无法检查渠道倍率")
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}

	jobID := newJobID()
	jobPath := filepath.Join(jobDir, jobID)
	if err := os.MkdirAll(jobPath, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(jobPath)

	inputPath, err := resolveInputFile(r, jobPath)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	form := r.MultipartForm.Value
	headers, rows, err := billing.LoadLogRows(inputPath, formValue(form, "sheet"), formValue(form, "encoding"))
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "读取日志失败: "+err.Error())
		return
	}

	// 用 ExtractChannelUsage 而不是 ExtractChannelIDs：后者的渠道号只能来自
	// channel_id 列，而手工用 SQL 导出的日志没有那一列，渠道号藏在
	// other.admin_info.use_channel 里。走前者两条来源都能认，页面少一次「重新导出」。
	usage, hasChannelInfo := billing.ExtractChannelUsage(headers, rows)
	if !hasChannelInfo {
		// 两处都取不到渠道号：这不是「检查失败」，而是这份日志做不了成本估算。
		// 明确说清原因与下一步动作，页面才好引导用户重新导出。
		// noChannelInfo 与 hasChannelColumn 分开报：前者是「这份日志没渠道号」，
		// 后者是「连 channel_id 列都没有」——页面据此选提示语。
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"hasChannelColumn":      false,
			"noChannelInfo":         true,
			"missingGroupRatioRows": billing.CountMissingGroupRatioRows(headers, rows),
			"totalRows":             len(rows),
			"message": "这份日志里取不到渠道号（既没有 channel_id 列，other 里也没有" +
				"use_channel）：无法检查上游倍率。请用「导出日志明细」重新导出一份带 channel_id 的日志。",
		})
		return
	}

	channelIDs := billing.UsedChannelIDs(usage)

	groupChannels := billing.GroupChannelMap(usage)

	ratios, domestic, err := billing.ChannelUpstreamConfig(*pgConfig)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	channelList, err := billing.ListChannels(*pgConfig)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	infoMap := make(map[int]billing.ChannelInfo, len(channelList))
	for _, c := range channelList {
		infoMap[c.ChannelID] = c.ChannelInfo
	}
	// 日志里出现、但本地渠道清单里没有的渠道号，也要能就地补录，
	// 否则用户只能先去拉一次清单；这里把它们按「未知渠道」补进清单供填写。
	for _, id := range channelIDs {
		if _, ok := infoMap[id]; !ok {
			infoMap[id] = billing.ChannelInfo{ChannelID: id, Name: fmt.Sprintf("渠道 %d（渠道清单里没有）", id)}
		}
	}

	// 按**行**判据统计（与出账、与任务预检共用 RowCostReason）。
	//
	// 从前这里只遍历「有渠道号的行」（ExtractChannelUsage 会把没有渠道号的行跳过），
	// 于是这些行对检查完全隐形：页面显示「都维护好了」，出账时却报
	// 「392 行缺少渠道倍率或分组倍率」。用户按页面提示补完再来查还是这句话。
	rowCounts, missingRatios, rowsPerChannel := billing.CountRowCostReasons(headers, rows, ratios)
	if rowCounts[billing.SkipNoUpstreamRatio] > 0 {
		// 缺倍率的渠道要能在页面上就地补，所以补进 infoMap 供填写。
		for _, id := range missingRatios {
			if _, ok := infoMap[id]; !ok {
				infoMap[id] = billing.ChannelInfo{ChannelID: id, Name: fmt.Sprintf("渠道 %d（不在渠道清单里）", id)}
			}
		}
	}

	// 渠道观测事实（行数、金额、分组、模型）：页面据此提示「这个渠道看起来是国模渠道」。
	obs := billing.ObserveChannels(headers, rows)

	// 待补录清单直接用 CheckChannelRatios：它从**倍率表**推，
	// 因而包含渠道清单里查不到的那些（它们照样能填倍率），
	// 而 CheckUpstreamRatios 会把它们当成「补不了」排除掉。
	check := billing.CheckChannelRatios(usage, ratios, domestic, infoMap, rowsPerChannel)
	billing.EnrichIssues(check.Missing, obs)
	// 兼容旧字段：missingChannels 从前是 []ChannelInfo，前端与既有测试都按那个形状读。
	// 新的 missingIssues 带 groups / known / rowCount，页面按分组分节展示用它。
	missingInfos := make([]billing.ChannelInfo, 0, len(check.Missing))
	for _, m := range check.Missing {
		missingInfos = append(missingInfos, billing.ChannelInfo{
			ChannelID: m.ChannelID, Name: m.Name, ChannelGroup: m.ChannelGroup,
		})
	}

	// 把「日志里用到的渠道」整份返回（含已维护的），页面可直接就地编辑补录。
	uncostable := map[string]int{}
	uncostableTotal := 0
	for reason, n := range rowCounts {
		if reason == billing.SkipNone || reason == billing.SkipZeroDelta || n == 0 {
			continue
		}
		uncostable[string(reason)] = n
		uncostableTotal += n
	}

	used := make([]billing.ChannelWithRatio, 0, len(channelIDs))
	for _, id := range channelIDs {
		info := infoMap[id]
		cw := billing.ChannelWithRatio{ChannelInfo: info, IsDomestic: domestic[id]}
		if v, ok := ratios[id]; ok {
			ratio := v
			cw.UpstreamRatio = &ratio
		}
		used = append(used, cw)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"hasChannelColumn":  true,
		"usedChannels":      used,
		"maintainedCount":   len(check.Maintained),
		"missingCount":      len(check.Missing),
		"unknownCount":      len(check.UnknownChannelIDs),
		"missingChannels":   missingInfos,
		"missingIssues":     check.Missing,
		"unknownChannelIds": check.UnknownChannelIDs,
		"channelTableEmpty": len(channelList) == 0,
		// 日志里用到的全部渠道 + 观测事实（模型、金额、国模提示依据），按金额降序。
		// 页面里要让人判断「这是不是国模渠道」，光有渠道号与名称不够。
		"reviewChannels": billing.BuildChannelReview(obs, ratios, domestic, infoMap),
		// 页面算「折合官方折扣」用，见 handleRunBillTasks 里同名字段的说明。
		"discountBaseFactor": billing.DiscountBaseFactor,
		// 按分组归类的渠道（页面分节展示用，见上面的 groupChannels）。
		"groupChannels": groupChannels,
		// group_ratio 缺失的行数：这些行的官方刊例反推不出来，成本列会留空。
		// 与「渠道没维护倍率」是两件事（一个补倍率、一个查日志），所以分开报。
		"missingGroupRatioRows": rowCounts[billing.SkipNoGroupRatio],
		// 算不出成本的行数按原因分类，页面据此说清「还差什么」。
		"uncostableRows":  uncostable,
		"uncostableTotal": uncostableTotal,
		"totalRows":       len(rows),
	})
}

// handlePullDBPrices 触发一次数据库价格拉取并落盘，供「数据库实时价格」出账模式使用。
func handlePullDBPrices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置数据库连接信息（BILL_DB_HOST 等环境变量）")
		return
	}
	count, fetchedAt, err := billing.PullPriceBookFromDB(*dbConfig, dbPriceCachePath())
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"modelCount": count,
		"fetchedAt":  fetchedAt.Format(time.RFC3339),
	})
}

// handlePullUserDiscount 按用户名或用户 ID 拉取一次分组倍率、换算成折扣，
// 写入本地 PostgreSQL 留存快照，再连同该用户最近几次的拉取记录一并返回。
// 每次调用都会连一次业务 MySQL 库（只读查询），但不会写业务库；写的是本地 Postgres。
func handlePullUserDiscount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置业务数据库连接信息（BILL_DB_HOST 等环境变量）")
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL 连接信息（BILL_PG_HOST 等环境变量）")
		return
	}

	var body struct {
		UserID   *int   `json:"userId"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.UserID == nil && strings.TrimSpace(body.Username) == "" {
		httpError(w, http.StatusBadRequest, "请提供用户名或用户 ID")
		return
	}

	current, err := billing.FetchUserGroupDiscount(*dbConfig, body.UserID, body.Username)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := billing.SaveUserDiscountPull(*pgConfig, *current); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	history, err := billing.RecentUserDiscountPulls(*pgConfig, current.UserID, 10)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"current": current,
		"history": history,
	})
}

func withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func handleGenerateBill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}

	jobID := newJobID()
	jobPath := filepath.Join(jobDir, jobID)
	if err := os.MkdirAll(jobPath, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 支持两种输入来源：上传的 file 字段，或服务器本地路径 serverPath 字段。
	inputPath, err := resolveInputFile(r, jobPath)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	form := r.MultipartForm.Value
	params := billing.Params{ExchangeRate: billing.DefaultExchangeRate, SanitizedLog: true}
	// 出账模板：空值等同标准模板，老前端不带这个字段时行为与改动前一致。
	params.BillTemplate = formValue(form, "billTemplate")
	if v := formValue(form, "month"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Month = n
		}
	}
	if v := formValue(form, "year"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Year = n
		}
	}
	if v := formValue(form, "exchangeRate"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			params.ExchangeRate = f
		}
	}
	if v := formValue(form, "discount"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			params.Discount = &f
		}
	}
	params.PriceSource = billing.PriceSource(formValue(form, "priceSource"))
	if params.PriceSource == "" && formValue(form, "preferPriceTable") == "true" {
		params.PriceSource = billing.PriceSourcePriceTable // 兼容旧版前端的 preferPriceTable 复选框
	}
	params.KeepLog = formValue(form, "keepLog") == "true"
	if v := formValue(form, "sanitizedLog"); v != "" {
		params.SanitizedLog = v == "true"
	}
	params.SanitizedFormat = formValue(form, "sanitizedFormat")
	params.IncludeBillingParams = formValue(form, "includeBillingParams") == "true"
	params.Sheet = formValue(form, "sheet")
	params.Encoding = formValue(form, "encoding")
	if v := formValue(form, "manualPrices"); v != "" {
		var manual map[string]billing.ManualPriceInput
		if err := json.Unmarshal([]byte(v), &manual); err != nil {
			httpError(w, http.StatusBadRequest, "解析手动补全价格失败: "+err.Error())
			return
		}
		params.ManualPrices = manual
	}
	// 国产/站内定价标识：一行一个，前端用多行文本框填写；空行自动丢弃。
	params.DomesticMarkers = parseMarkers(formValue(form, "domesticMarkers"))

	// 成本利润表：勾选时从本地 PG 装载渠道倍率与渠道名。
	// 不在这里做「有没有未维护渠道」的判断——那件事需要先读日志里的渠道集合，
	// 由 billing.GenerateBill 在解析完日志后统一检查，避免把日志读两遍。
	//
	// 简易账单（模板二）不产出成本利润表，这里直接不置位：它的金额来自站点额度，
	// 与上游成本无关。不跳过的话，只是想出一张简易账单的用户会被要求先配 PostgreSQL，
	// 而他根本用不到渠道倍率。
	params.GenerateCost = !billing.IsSimpleBillTemplate(params.BillTemplate) &&
		formValue(form, "generateCost") == "true"
	if params.GenerateCost {
		if pgConfig == nil {
			httpError(w, http.StatusBadRequest, "生成成本利润表需要先配置 PostgreSQL（BILL_PG_*）并拉取渠道清单")
			return
		}
		ratios, domestic, err := billing.ChannelUpstreamConfig(*pgConfig)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		channels, err := billing.ListChannels(*pgConfig)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		params.ChannelUpstreamRatios = ratios
		// 国模标识与倍率成对使用（折扣 = f(倍率, 是否国模)），漏传会让国模渠道成本偏低 7 倍。
		params.ChannelDomestic = domestic
		params.ChannelNames = make(map[int]string, len(channels))
		params.ChannelInfos = make(map[int]billing.ChannelInfo, len(channels))
		for _, c := range channels {
			params.ChannelNames[c.ChannelID] = c.Name
			params.ChannelInfos[c.ChannelID] = c.ChannelInfo
		}
	}

	templatePath := filepath.Join(dataDir, "bill_template.xlsx")
	priceTablePath := filepath.Join(dataDir, "price_table.xlsx")

	result, err := billing.GenerateBill(inputPath, templatePath, priceTablePath, dbPriceCachePath(), jobPath, params)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	jobsMu.Lock()
	jobs[jobID] = jobRecord{
		billPath: result.BillPath, sanitizedPath: result.SanitizedPath,
		costPath: result.CostPath, createdAt: time.Now(),
	}
	jobsMu.Unlock()

	resp := map[string]interface{}{
		"jobId":        jobID,
		"billFileName": filepath.Base(result.BillPath),
		"billUrl":      "/api/download/" + jobID + "/bill",
		"summary":      result.Summary,
	}
	if result.SanitizedPath != "" {
		resp["sanitizedFileName"] = filepath.Base(result.SanitizedPath)
		resp["sanitizedUrl"] = "/api/download/" + jobID + "/sanitized"
	}
	if result.CostPath != "" {
		resp["costFileName"] = filepath.Base(result.CostPath)
		resp["costUrl"] = "/api/download/" + jobID + "/cost"
		// 结果区要展示/复制的成本利润摘要：数字由后端按与表内公式同源的口径算好，
		// 前端只负责渲染与复制，不再自己拼金额。
		resp["costSummary"] = result.CostSummary
		resp["costTotals"] = result.CostTotals
	}
	// 简易账单的可复制文字。它与成本利润摘要分开两个键：内容口径不同
	// （站点实收额度 vs 刊例×折扣），前端按「哪个键有值」决定显示哪种说明。
	// 简易账单不产成本利润表，所以这两个键不会同时有值。
	if result.BillSummary != "" {
		resp["billSummary"] = result.BillSummary
	}
	// 被拦下的情形**不是错误**：账单已经生成并登记好了，只是成本利润表没出，
	// 把待补录的渠道清单交给页面，用户补完倍率再勾一次即可。
	if result.CostBlocked {
		resp["costBlocked"] = true
		resp["missingChannels"] = result.MissingChannelInfos
	}
	// 未知渠道无论是否被拦下都要告诉页面：成本利润表里它们的成本列是空的，
	// 用户得知道是哪几个渠道号空着。
	if len(result.UnknownChannelIDs) > 0 {
		resp["unknownChannelIds"] = result.UnknownChannelIDs
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMergeLogs 把多个日志文件合并成一个，结果按用户选择的格式写到 data 目录，
// 同时登记成本次任务，前端可直接下载（data 目录对浏览器不可见，只能走下载接口）。
func handleMergeLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}

	jobID := newJobID()
	jobPath := filepath.Join(jobDir, jobID)
	if err := os.MkdirAll(jobPath, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(jobPath)

	inputPaths, err := resolveInputFiles(r, jobPath)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(inputPaths) < 2 {
		httpError(w, http.StatusBadRequest, "至少需要选择两个日志文件才能合并")
		return
	}

	form := r.MultipartForm.Value
	result, err := billing.MergeLogs(inputPaths, billing.MergeParams{
		Sheet:    formValue(form, "sheet"),
		Encoding: formValue(form, "encoding"),
		Format:   formValue(form, "format"),
		Dedupe:   formValue(form, "dedupe") == "true",
		OutDir:   dataDir,
	})
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	jobsMu.Lock()
	jobs[jobID] = jobRecord{mergedPath: result.Path, createdAt: time.Now()}
	jobsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobId":          jobID,
		"mergedFileName": filepath.Base(result.Path),
		"mergedUrl":      "/api/download/" + jobID + "/merged",
		"mergedPath":     result.Path,
		"inputCount":     result.InputCount,
		"inputRows":      result.InputRows,
		"rowCount":       result.RowCount,
		"droppedRows":    result.DroppedRows,
		// 被剔除的列（脱敏日志合并时的 channel_id）：页面要告诉用户「输入里有、结果里没有」。
		"droppedColumns": result.DroppedColumns,
		"headers":        result.Headers,
		"format":         result.Format,
	})
}

func formValue(form map[string][]string, key string) string {
	if v, ok := form[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// parseMarkers 把多行文本拆成标识列表：按换行切分、逐条去空白、丢掉空行，
// 并去掉重复项（同一标识写两遍没有额外含义）。逗号也当分隔符，
// 便于用户顺手把一列标识粘进来。
func parseMarkers(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == '，'
	})
	seen := map[string]bool{}
	var markers []string
	for _, f := range fields {
		m := strings.TrimSpace(f)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		markers = append(markers, m)
	}
	return markers
}

// handleDownload 仅按已生成任务的内存记录取路径，避免用户输入直接拼接文件路径。
func handleDownload(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/download/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	jobID, kind := parts[0], parts[1]
	jobsMu.Lock()
	rec, ok := jobs[jobID]
	jobsMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	var path string
	switch kind {
	case "bill":
		path = rec.billPath
	case "sanitized":
		path = rec.sanitizedPath
	case "merged":
		path = rec.mergedPath
	case "exported":
		path = rec.exportedPath
	case "cost":
		path = rec.costPath
	}
	if path == "" {
		http.NotFound(w, r)
		return
	}

	name := filepath.Base(path)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="download%s"; filename*=UTF-8''%s`, filepath.Ext(name), url.PathEscape(name)))
	http.ServeFile(w, r, path)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cleanupOldJobs 定期清理超过 6 小时的任务文件，避免临时目录无限增长。
// jobRetentionHours 任务产物的保留时长（小时），由 cleanupOldJobs 执行清理。
//
// 抽成常量是因为它出现在两个地方：清理逻辑本身，以及「账单任务」接口返回给页面的
// jobRetentionHours（页面据此告诉用户「文件只保留 N 小时」）。
// 两处各写一个数字迟早会漂移——而漂移的方向若是「页面说 6 小时、实际 3 小时就清了」，
// 用户会按页面的说法从容等待，然后发现文件没了。
const jobRetentionHours = 6

func cleanupOldJobs() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-time.Duration(jobRetentionHours) * time.Hour)
		jobsMu.Lock()
		for id, rec := range jobs {
			if rec.createdAt.Before(cutoff) {
				os.RemoveAll(filepath.Join(jobDir, id))
				delete(jobs, id)
			}
		}
		jobsMu.Unlock()
	}
}
