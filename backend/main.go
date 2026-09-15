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
)

type jobRecord struct {
	billPath      string
	sanitizedPath string
	mergedPath    string
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

	go cleanupOldJobs()
	go cleanupSessions()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", withCORS(handleLogin))
	mux.HandleFunc("/api/logout", withCORS(handleLogout))
	mux.HandleFunc("/api/session", withCORS(handleSession))
	mux.HandleFunc("/api/bill", withCORS(requireAuth(handleGenerateBill)))
	mux.HandleFunc("/api/merge-logs", withCORS(requireAuth(handleMergeLogs)))
	mux.HandleFunc("/api/pull-db-prices", withCORS(requireAuth(handlePullDBPrices)))
	mux.HandleFunc("/api/check-prices", withCORS(requireAuth(handleCheckMissingPrices)))
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
	}
}

// dbPriceCachePath 手动拉取数据库价格落盘的位置，与 data 目录一起挂载，重启容器后仍可读取。
func dbPriceCachePath() string {
	return filepath.Join(dataDir, "db_price_cache.json")
}

// loadPriceBookForSource 按价格来源加载 PriceBook，GenerateBill 与 handleCheckMissingPrices 共用。
func loadPriceBookForSource(source billing.PriceSource, priceTablePath string) (*billing.PriceBook, error) {
	if source == billing.PriceSourceDB {
		book, _, err := billing.LoadPriceBookFromDBCacheFile(dbPriceCachePath())
		return book, err
	}
	book, err := billing.LoadPriceBook(priceTablePath)
	if err != nil {
		return nil, fmt.Errorf("加载报价表失败: %w", err)
	}
	return book, nil
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
	book, err := loadPriceBookForSource(priceSource, priceTablePath)
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

	var missingModels []string
	for _, model := range models {
		if price, _ := billing.ResolvePrice(model, book, preferPriceTable, exchangeRate); price == nil {
			missingModels = append(missingModels, model)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"modelCount":    len(models),
		"missingModels": missingModels,
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

	templatePath := filepath.Join(dataDir, "bill_template.xlsx")
	priceTablePath := filepath.Join(dataDir, "price_table.xlsx")

	result, err := billing.GenerateBill(inputPath, templatePath, priceTablePath, dbPriceCachePath(), jobPath, params)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	jobsMu.Lock()
	jobs[jobID] = jobRecord{billPath: result.BillPath, sanitizedPath: result.SanitizedPath, createdAt: time.Now()}
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
func cleanupOldJobs() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-6 * time.Hour)
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
