package billing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
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

// dbPriceCacheTTL 数据库价格缓存有效期，避免每次出账都查询生产库。
const dbPriceCacheTTL = 5 * time.Minute

var dbPriceCacheMu sync.Mutex
var dbPriceCacheBook *PriceBook
var dbPriceCacheAt time.Time

// LoadPriceBookFromDB 从业务库 options 表读取 ModelRatio/CompletionRatio，
// 换算为 USD/MTok 单价；`dbPriceCacheTTL` 内的重复调用直接返回缓存结果。
func LoadPriceBookFromDB(cfg DBConfig) (*PriceBook, error) {
	dbPriceCacheMu.Lock()
	defer dbPriceCacheMu.Unlock()

	if dbPriceCacheBook != nil && time.Since(dbPriceCacheAt) < dbPriceCacheTTL {
		return dbPriceCacheBook, nil
	}

	book, err := fetchPriceBookFromDB(cfg)
	if err != nil {
		return nil, err
	}

	dbPriceCacheBook = book
	dbPriceCacheAt = time.Now()
	return book, nil
}

func fetchPriceBookFromDB(cfg DBConfig) (*PriceBook, error) {
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

	book := NewPriceBook()
	for model, ratio := range modelRatio {
		cr, ok := completionRatio[model]
		if !ok {
			continue // 缺输出倍率则跳过，交由其他价格来源兜底
		}
		inputPerM := ratio * 2 // $/1K tokens = ratio * 0.002 → $/MTok = ratio * 2
		book.ByModel[model] = ModelPrice{
			InputPerM: inputPerM, OutputPerM: inputPerM * cr, Currency: "USD",
			Source: "option_table_db", Category: "数据库实时", Channel: "options",
		}
	}
	return book, nil
}
