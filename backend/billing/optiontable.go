package billing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// DBConfig 业务库 options 表连接信息（key/value 结构，与 one-api/new-api 一致）。
type DBConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
	Table    string // 默认 "options"
}

func (c DBConfig) dsn() string {
	port := c.Port
	if port == "" {
		port = "3306"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&timeout=5s",
		c.User, c.Password, c.Host, port, c.DBName)
}

func (c DBConfig) tableName() string {
	if c.Table == "" {
		return "options"
	}
	return c.Table
}

// dbCachedPrice 单模型价格（USD/MTok），落盘用的精简结构。
type dbCachedPrice struct {
	InputPerM  float64 `json:"inputPerM"`
	OutputPerM float64 `json:"outputPerM"`
}

// dbPriceCacheFile 手动拉取结果的落盘格式，出账时只读此文件，不再连接数据库。
type dbPriceCacheFile struct {
	FetchedAt time.Time                `json:"fetchedAt"`
	Prices    map[string]dbCachedPrice `json:"prices"`
}

// PullPriceBookFromDB 连接数据库拉取一次最新 ModelRatio/CompletionRatio，写入 cachePath 落盘；
// 之后的出账请求直接读文件，既不占用常驻内存，也不用每次都连数据库。
func PullPriceBookFromDB(cfg DBConfig, cachePath string) (modelCount int, fetchedAt time.Time, err error) {
	prices, err := fetchModelPricesFromDB(cfg)
	if err != nil {
		return 0, time.Time{}, err
	}

	fetchedAt = time.Now()
	data, err := json.MarshalIndent(dbPriceCacheFile{FetchedAt: fetchedAt, Prices: prices}, "", "  ")
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("序列化价格缓存失败: %w", err)
	}
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		return 0, time.Time{}, fmt.Errorf("写入价格缓存文件失败: %w", err)
	}
	return len(prices), fetchedAt, nil
}

func fetchModelPricesFromDB(cfg DBConfig) (map[string]dbCachedPrice, error) {
	db, err := sql.Open("mysql", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	defer db.Close()

	query := fmt.Sprintf("SELECT `key`, `value` FROM `%s` WHERE `key` IN ('ModelRatio', 'CompletionRatio')", cfg.tableName())
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("查询 %s 表失败: %w", cfg.tableName(), err)
	}
	defer rows.Close()

	raw := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("读取 %s 表数据失败: %w", cfg.tableName(), err)
		}
		raw[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 %s 表数据失败: %w", cfg.tableName(), err)
	}

	var modelRatio map[string]float64
	if v, ok := raw["ModelRatio"]; ok {
		if err := json.Unmarshal([]byte(v), &modelRatio); err != nil {
			return nil, fmt.Errorf("解析 ModelRatio 失败: %w", err)
		}
	}
	var completionRatio map[string]float64
	if v, ok := raw["CompletionRatio"]; ok {
		if err := json.Unmarshal([]byte(v), &completionRatio); err != nil {
			return nil, fmt.Errorf("解析 CompletionRatio 失败: %w", err)
		}
	}

	prices := map[string]dbCachedPrice{}
	for model, ratio := range modelRatio {
		cr, ok := completionRatio[model]
		if !ok {
			continue // 缺输出倍率则跳过，交由其他价格来源兜底
		}
		inputPerM := ratio * 2 // $/1K tokens = ratio * 0.002 → $/MTok = ratio * 2
		prices[model] = dbCachedPrice{InputPerM: inputPerM, OutputPerM: inputPerM * cr}
	}
	return prices, nil
}

// LoadPriceBookFromDBCacheFile 读取上一次手动拉取生成的本地价格缓存文件；
// 文件不存在时提示先拉取，不会主动连接数据库。
func LoadPriceBookFromDBCacheFile(cachePath string) (*PriceBook, time.Time, error) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, time.Time{}, fmt.Errorf("尚未拉取过数据库价格，请先在页面点击「拉取最新数据库价格」")
		}
		return nil, time.Time{}, fmt.Errorf("读取价格缓存文件失败: %w", err)
	}

	var file dbPriceCacheFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, time.Time{}, fmt.Errorf("解析价格缓存文件失败: %w", err)
	}

	book := NewPriceBook()
	for model, p := range file.Prices {
		book.ByModel[model] = ModelPrice{
			InputPerM: p.InputPerM, OutputPerM: p.OutputPerM, Currency: "USD",
			Source: "option_table_db_file", Category: "数据库实时", Channel: "options",
		}
	}
	return book, file.FetchedAt, nil
}

