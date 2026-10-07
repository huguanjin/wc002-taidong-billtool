package billing

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ChannelInfo 从业务库 channels 表拉取的渠道标识。
//
// 只取成本估算需要的字段：key / base_url 等敏感信息一概不落本地，
// 渠道名也只为让成本利润表读得懂。
type ChannelInfo struct {
	ChannelID    int    `json:"channelId"`
	Name         string `json:"name"`
	ChannelType  int    `json:"channelType"`
	Status       int    `json:"status"`
	ChannelGroup string `json:"channelGroup"`
}

// ChannelWithRatio 渠道 + 其上游倍率维护状态，供页面展示与编辑。
type ChannelWithRatio struct {
	ChannelInfo
	// UpstreamRatio 为 nil 表示尚未维护（与「维护成 0」是两回事：
	// 0 意味着上游免费，nil 意味着还不知道，成本不能按 0 算）。
	UpstreamRatio *float64   `json:"upstreamRatio"`
	Note          string     `json:"note"`
	UpdatedAt     *time.Time `json:"updatedAt"`
	// IsDomestic 是否国模渠道：该渠道承接的是站点按人民币报价的国产模型，
	// 它的上游倍率按「原价折扣」理解（0.4 = 4 折），而不是海外渠道的「每美金多少元」。
	// 换算规则见 UpstreamDiscountFor。默认 false，与加这个字段之前的口径一致。
	IsDomestic bool `json:"isDomestic"`
	// Stale 表示该渠道本次拉取时已不在业务库 channels 表里（已被硬删除）。
	// 历史账期可能仍引用它，所以不从本地删除，只标记出来提示复核。
	Stale bool `json:"stale"`
}

// ChannelRatioInput 一次批量保存里的单条。
//
// Note 与 IsDomestic 都是指针：nil 表示「这次没传，保持库里原值」。
// 用值类型的话，只改倍率的老页面（不带这两个字段）保存一次，
// 就会把用户设好的国模标识与备注悄悄清掉——而国模标识直接决定成本差 7 倍。
type ChannelRatioInput struct {
	ChannelID     int      `json:"channelId"`
	UpstreamRatio *float64 `json:"upstreamRatio"`
	Note          *string  `json:"note"`
	IsDomestic    *bool    `json:"isDomestic"`
}

// EnsureChannelSchema 建渠道相关表（幂等），启动时调用一次。
func EnsureChannelSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS channels (
			channel_id    INTEGER PRIMARY KEY,
			name          TEXT NOT NULL,
			channel_type  INTEGER NOT NULL DEFAULT 0,
			status        INTEGER NOT NULL DEFAULT 0,
			channel_group TEXT NOT NULL DEFAULT '',
			fetched_at    TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("创建 channels 表失败: %w", err)
	}

	// 每个渠道一个上游倍率。upstream_ratio 允许 NULL 以表达「拉过来了但还没维护」。
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS channel_upstream_ratios (
			channel_id     INTEGER PRIMARY KEY,
			upstream_ratio DOUBLE PRECISION,
			note           TEXT NOT NULL DEFAULT '',
			is_domestic    BOOLEAN NOT NULL DEFAULT false,
			updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("创建 channel_upstream_ratios 表失败: %w", err)
	}
	// 老库补列。默认 false：升级后所有已维护的渠道照旧按海外口径算（倍率 ÷ 7），
	// 数字不会因为升级而变——需要改成国模的渠道由用户显式标记。
	if _, err := db.Exec(`
		ALTER TABLE channel_upstream_ratios
		ADD COLUMN IF NOT EXISTS is_domestic BOOLEAN NOT NULL DEFAULT false
	`); err != nil {
		return fmt.Errorf("迁移 channel_upstream_ratios 表失败: %w", err)
	}
	return nil
}

// PullChannelsFromDB 连业务库读 channels 表（只读），UPSERT 进本地 PG。
//
// 用 UPSERT：已存在的渠道更新名称/类型/状态，新增的插入。
// 业务库里已删除的渠道**不从本地删除**——历史账期可能仍引用它，
// 删了会让那份账期的成本利润表凭空少行；改由 ListChannels 标记 stale 提示复核。
func PullChannelsFromDB(biz DBConfig, pg PGConfig) (int, error) {
	channels, err := fetchChannelsFromDB(biz)
	if err != nil {
		return 0, err
	}
	if err := upsertChannels(pg, channels); err != nil {
		return 0, err
	}
	return len(channels), nil
}

// fetchChannelsFromDB 只读业务库 channels 表。
func fetchChannelsFromDB(biz DBConfig) ([]ChannelInfo, error) {
	db, err := sql.Open("mysql", biz.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接业务数据库失败: %w", err)
	}
	defer db.Close()

	// group 是 MySQL 保留字，必须反引号。
	rows, err := db.Query("SELECT id, name, type, status, `group` FROM channels")
	if err != nil {
		return nil, fmt.Errorf("查询 channels 表失败: %w", err)
	}
	defer rows.Close()

	var out []ChannelInfo
	for rows.Next() {
		var c ChannelInfo
		var name, group sql.NullString
		if err := rows.Scan(&c.ChannelID, &name, &c.ChannelType, &c.Status, &group); err != nil {
			return nil, fmt.Errorf("读取 channels 表数据失败: %w", err)
		}
		c.Name = strings.TrimSpace(name.String)
		if c.Name == "" {
			// 渠道名可能为空串，落库时给个占位，避免成本利润表出现空白渠道名列。
			c.Name = fmt.Sprintf("渠道 %d", c.ChannelID)
		}
		c.ChannelGroup = strings.TrimSpace(group.String)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 channels 表数据失败: %w", err)
	}
	return out, nil
}

// upsertChannels 单事务批量写入，避免逐行各开一次往返。
func upsertChannels(pg PGConfig, channels []ChannelInfo) error {
	if len(channels) == 0 {
		return nil
	}
	db, err := sql.Open("pgx", pg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO channels (channel_id, name, channel_type, status, channel_group, fetched_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (channel_id) DO UPDATE SET
			name = EXCLUDED.name,
			channel_type = EXCLUDED.channel_type,
			status = EXCLUDED.status,
			channel_group = EXCLUDED.channel_group,
			fetched_at = now()
	`)
	if err != nil {
		return fmt.Errorf("准备写入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, c := range channels {
		if _, err := stmt.Exec(c.ChannelID, c.Name, c.ChannelType, c.Status, c.ChannelGroup); err != nil {
			return fmt.Errorf("写入渠道 %d 失败: %w", c.ChannelID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交渠道写入失败: %w", err)
	}
	return nil
}

// ListChannels 读本地渠道清单并左连倍率表，返回每个渠道的维护状态。
//
// 排序把「未维护」放前面，方便用户一眼看到还要补哪些。
func ListChannels(pg PGConfig) ([]ChannelWithRatio, error) {
	db, err := sql.Open("pgx", pg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT c.channel_id, c.name, c.channel_type, c.status, c.channel_group,
		       r.upstream_ratio, COALESCE(r.note, ''), r.updated_at,
		       COALESCE(r.is_domestic, false)
		FROM channels c
		LEFT JOIN channel_upstream_ratios r ON r.channel_id = c.channel_id
		ORDER BY (r.upstream_ratio IS NULL) DESC, c.channel_id
	`)
	if err != nil {
		return nil, fmt.Errorf("查询渠道清单失败: %w", err)
	}
	defer rows.Close()

	var out []ChannelWithRatio
	for rows.Next() {
		var c ChannelWithRatio
		var ratio sql.NullFloat64
		var updatedAt sql.NullTime
		if err := rows.Scan(&c.ChannelID, &c.Name, &c.ChannelType, &c.Status,
			&c.ChannelGroup, &ratio, &c.Note, &updatedAt, &c.IsDomestic); err != nil {
			return nil, fmt.Errorf("读取渠道清单失败: %w", err)
		}
		if ratio.Valid {
			v := ratio.Float64
			c.UpstreamRatio = &v
		}
		if updatedAt.Valid {
			t := updatedAt.Time
			c.UpdatedAt = &t
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取渠道清单失败: %w", err)
	}
	return out, nil
}

// UpsertChannelRatios 批量保存用户维护的上游倍率与国模标识。
//
// Note / IsDomestic 为 nil 时保持库里原值（见 ChannelRatioInput）。参数一律带显式类型转换：
// 同一个占位符在 INSERT 与 ON CONFLICT 两处各出现一次，不写转换时 PostgreSQL
// 要靠第一处推断类型，推断失败会报「could not determine data type of parameter」。
func UpsertChannelRatios(pg PGConfig, items []ChannelRatioInput) error {
	if len(items) == 0 {
		return nil
	}
	db, err := sql.Open("pgx", pg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO channel_upstream_ratios (channel_id, upstream_ratio, note, is_domestic, updated_at)
		VALUES ($1, $2, COALESCE($3::text, ''), COALESCE($4::boolean, false), now())
		ON CONFLICT (channel_id) DO UPDATE SET
			upstream_ratio = EXCLUDED.upstream_ratio,
			note = COALESCE($3::text, channel_upstream_ratios.note),
			is_domestic = COALESCE($4::boolean, channel_upstream_ratios.is_domestic),
			updated_at = now()
	`)
	if err != nil {
		return fmt.Errorf("准备写入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, it := range items {
		var note *string
		if it.Note != nil {
			trimmed := strings.TrimSpace(*it.Note)
			note = &trimmed
		}
		if _, err := stmt.Exec(it.ChannelID, it.UpstreamRatio, note, it.IsDomestic); err != nil {
			return fmt.Errorf("写入渠道 %d 倍率失败: %w", it.ChannelID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交倍率写入失败: %w", err)
	}
	return nil
}

// ChannelUpstreamConfig 读出全部渠道的上游配置：倍率（只含已维护的）与国模标识（只含标了的）。
//
// 一次查询读两样，是因为出账时它们永远成对使用（折扣 = f(倍率, 是否国模)），
// 分两次读的话中间被别处改了一下，就会出现「倍率是新的、标识是旧的」这种谁也没配过的组合。
func ChannelUpstreamConfig(pg PGConfig) (ratios map[int]float64, domestic map[int]bool, err error) {
	db, err := sql.Open("pgx", pg.dsn())
	if err != nil {
		return nil, nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	// 不加 WHERE upstream_ratio IS NOT NULL：倍率还没填、但先标了国模的渠道也要读出来，
	// 否则「先打勾、后补倍率」的顺序会丢掉那个勾。
	rows, err := db.Query(`SELECT channel_id, upstream_ratio, is_domestic FROM channel_upstream_ratios`)
	if err != nil {
		return nil, nil, fmt.Errorf("查询渠道倍率失败: %w", err)
	}
	defer rows.Close()

	ratios = map[int]float64{}
	domestic = map[int]bool{}
	for rows.Next() {
		var id int
		var ratio sql.NullFloat64
		var isDomestic bool
		if err := rows.Scan(&id, &ratio, &isDomestic); err != nil {
			return nil, nil, fmt.Errorf("读取渠道倍率失败: %w", err)
		}
		if ratio.Valid {
			ratios[id] = ratio.Float64
		}
		if isDomestic {
			domestic[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("读取渠道倍率失败: %w", err)
	}
	return ratios, domestic, nil
}

// ChannelRatioMap 渠道 ID → 上游倍率（只含已维护的）。
//
// 只要倍率、不关心国模标识的调用方用它（如只判断「有没有维护」）；
// 凡是要算成本的地方都必须用 ChannelUpstreamConfig，否则会漏掉国模标识。
func ChannelRatioMap(pg PGConfig) (map[int]float64, error) {
	ratios, _, err := ChannelUpstreamConfig(pg)
	return ratios, err
}
