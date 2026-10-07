package billing

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这组测试需要一个**真实的 PostgreSQL**，默认跳过。
//
// 为什么不 mock：它们要验证的就是 SQL 本身——ON CONFLICT 里同一个参数出现两次时的类型推断、
// ALTER TABLE ... ADD COLUMN IF NOT EXISTS 在老表上的迁移、COALESCE 保留原值的语义。
// 这些在 mock 里根本跑不出来，而一旦写错，表现是启动时迁移失败，或渠道页「保存不了」。
//
// 运行方式（连一个可随意丢弃的实例；格式 host:port:user:dbname，免密码，即 trust 认证）：
//
//	BILLTOOL_TEST_PG=127.0.0.1:55432:postgres:billtest go test ./billing -run PG -v
//
// 安全：每个测试都在**自己新建的 schema** 里跑，结束时整个 DROP CASCADE，
// 所以即使有人误把它指向一个有数据的开发库，也只会动到那个临时 schema，不碰 public 里的表。

// testPG 返回指向临时 schema 的 PG 配置；没设 BILLTOOL_TEST_PG 就跳过。
func testPG(t *testing.T) PGConfig {
	t.Helper()
	spec := os.Getenv("BILLTOOL_TEST_PG")
	if spec == "" {
		t.Skip("未设置 BILLTOOL_TEST_PG（形如 127.0.0.1:55432:postgres:billtest），跳过需要真实 PostgreSQL 的测试")
	}
	parts := strings.Split(spec, ":")
	require.Len(t, parts, 4, "BILLTOOL_TEST_PG 格式应为 host:port:user:dbname")
	cfg := PGConfig{Host: parts[0], Port: parts[1], User: parts[2], DBName: parts[3]}

	schema := fmt.Sprintf("billtest_%d", time.Now().UnixNano())
	admin, err := sql.Open("pgx", cfg.dsn())
	require.NoError(t, err)
	_, err = admin.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})

	// 此后本测试里所有 sql.Open 的连接都落在这个 schema 里。
	t.Setenv("PGOPTIONS", "-c search_path="+schema)

	// 隔离是否真的生效必须**亲自验证**，不能默认它生效：PGOPTIONS 若没被驱动认，
	// 建表会静默落进 public——那正是这套隔离要防的事。
	probe, err := sql.Open("pgx", cfg.dsn())
	require.NoError(t, err)
	defer probe.Close()
	var current string
	require.NoError(t, probe.QueryRow("SELECT current_schema()").Scan(&current))
	if current != schema {
		t.Fatalf("search_path 没有指向临时 schema（当前 %q，应为 %q）：拒绝继续，以免写进真实表", current, schema)
	}
	return cfg
}

func openPG(t *testing.T, cfg PGConfig) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", cfg.dsn())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func strPtr2(s string) *string { return &s }
func boolPtr(b bool) *bool     { return &b }

// readChannelRow 直接读倍率表里一行，绕开被测代码，作为判定依据。
func readChannelRow(t *testing.T, db *sql.DB, id int) (ratio sql.NullFloat64, note string, domestic bool, found bool) {
	t.Helper()
	err := db.QueryRow(
		`SELECT upstream_ratio, note, is_domestic FROM channel_upstream_ratios WHERE channel_id = $1`, id,
	).Scan(&ratio, &note, &domestic)
	if err == sql.ErrNoRows {
		return ratio, "", false, false
	}
	require.NoError(t, err)
	return ratio, note, domestic, true
}

// TestPGChannelSchemaAddsIsDomesticToOldTable 老库升级：加 is_domestic 列，老数据原样保留。
//
// 关键断言是**老渠道升级后不会被悄悄标成国模**：标识直接决定成本差 7 倍，
// 升级这一步要是把默认值写成 true，所有已维护的海外渠道成本会瞬间变成 7 倍。
func TestPGChannelSchemaAddsIsDomesticToOldTable(t *testing.T) {
	cfg := testPG(t)
	db := openPG(t, cfg)

	// 老库里的表形：没有 is_domestic。
	_, err := db.Exec(`CREATE TABLE channel_upstream_ratios (
		channel_id     INTEGER PRIMARY KEY,
		upstream_ratio DOUBLE PRECISION,
		note           TEXT NOT NULL DEFAULT '',
		updated_at     TIMESTAMPTZ NOT NULL DEFAULT now())`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channel_upstream_ratios (channel_id, upstream_ratio, note) VALUES (7, 0.4, '老数据')`)
	require.NoError(t, err)

	require.NoError(t, EnsureChannelSchema(cfg))
	require.NoError(t, EnsureChannelSchema(cfg), "迁移必须幂等：每次启动都会跑一遍")

	ratio, note, domestic, found := readChannelRow(t, db, 7)
	require.True(t, found)
	assert.Equal(t, 0.4, ratio.Float64, "老数据的倍率不能因升级而变")
	assert.Equal(t, "老数据", note)
	assert.False(t, domestic, "升级后老渠道一律按海外口径，不能被悄悄标成国模")
}

// TestPGUpsertChannelRatiosKeepsUnsentFields 保存时没传的字段保持库里原值。
//
// 这是 Note / IsDomestic 改成指针的全部理由：只改倍率的老页面（不带这两个字段）
// 保存一次，不能把用户设好的国模标识与备注悄悄清掉。
func TestPGUpsertChannelRatiosKeepsUnsentFields(t *testing.T) {
	cfg := testPG(t)
	require.NoError(t, EnsureChannelSchema(cfg))
	db := openPG(t, cfg)
	r := func(v float64) *float64 { return &v }

	// 新建，note 与 isDomestic 都没传 → 取默认（空串 / false）。
	require.NoError(t, UpsertChannelRatios(cfg, []ChannelRatioInput{{ChannelID: 1, UpstreamRatio: r(0.4)}}))
	ratio, note, domestic, found := readChannelRow(t, db, 1)
	require.True(t, found)
	assert.Equal(t, 0.4, ratio.Float64)
	assert.Equal(t, "", note)
	assert.False(t, domestic)

	// 带 note 与国模标识更新。
	require.NoError(t, UpsertChannelRatios(cfg, []ChannelRatioInput{
		{ChannelID: 1, UpstreamRatio: r(0.5), Note: strPtr2("GLM 专线"), IsDomestic: boolPtr(true)},
	}))
	ratio, note, domestic, _ = readChannelRow(t, db, 1)
	assert.Equal(t, 0.5, ratio.Float64)
	assert.Equal(t, "GLM 专线", note)
	assert.True(t, domestic)

	// 老页面只改倍率：note 与国模标识必须保持。
	require.NoError(t, UpsertChannelRatios(cfg, []ChannelRatioInput{{ChannelID: 1, UpstreamRatio: r(0.6)}}))
	ratio, note, domestic, _ = readChannelRow(t, db, 1)
	assert.Equal(t, 0.6, ratio.Float64, "倍率照常更新")
	assert.Equal(t, "GLM 专线", note, "没传 note 就保持原值，不能被清空")
	assert.True(t, domestic, "没传 isDomestic 就保持原值，不能被悄悄取消国模")

	// 显式传 false 是有效值：用户明确取消国模，必须生效。
	require.NoError(t, UpsertChannelRatios(cfg, []ChannelRatioInput{
		{ChannelID: 1, UpstreamRatio: r(0.6), IsDomestic: boolPtr(false)},
	}))
	_, note, domestic, _ = readChannelRow(t, db, 1)
	assert.False(t, domestic, "显式 false 要能取消国模标识")
	assert.Equal(t, "GLM 专线", note, "只动了标识，备注不受影响")

	// 显式传空串也是有效值：用户清空备注。
	require.NoError(t, UpsertChannelRatios(cfg, []ChannelRatioInput{
		{ChannelID: 1, UpstreamRatio: r(0.6), Note: strPtr2("  ")},
	}))
	_, note, _, _ = readChannelRow(t, db, 1)
	assert.Equal(t, "", note, "显式给空白要能清空备注（且 trim 掉）")

	// 倍率传 nil 表示「取消维护」（页面清空输入框的语义，保持不变）。
	require.NoError(t, UpsertChannelRatios(cfg, []ChannelRatioInput{{ChannelID: 1, UpstreamRatio: nil}}))
	ratio, _, _, _ = readChannelRow(t, db, 1)
	assert.False(t, ratio.Valid, "倍率 nil = 取消维护，库里应为 NULL")
}

// TestPGChannelUpstreamConfigReadsFlagWithoutRatio 先标国模、后补倍率的顺序也不能丢标识。
func TestPGChannelUpstreamConfigReadsFlagWithoutRatio(t *testing.T) {
	cfg := testPG(t)
	require.NoError(t, EnsureChannelSchema(cfg))
	r := func(v float64) *float64 { return &v }

	require.NoError(t, UpsertChannelRatios(cfg, []ChannelRatioInput{
		{ChannelID: 10, UpstreamRatio: r(0.4), IsDomestic: boolPtr(true)}, // 倍率 + 国模
		{ChannelID: 11, UpstreamRatio: r(1.8)},                            // 只有倍率（海外）
		{ChannelID: 12, UpstreamRatio: nil, IsDomestic: boolPtr(true)},    // 只标了国模，倍率还没填
		{ChannelID: 13, UpstreamRatio: nil},                               // 什么都没有
	}))

	ratios, domestic, err := ChannelUpstreamConfig(cfg)
	require.NoError(t, err)

	assert.Equal(t, map[int]float64{10: 0.4, 11: 1.8}, ratios, "只含已维护倍率的渠道")
	assert.Equal(t, map[int]bool{10: true, 12: true}, domestic,
		"国模标识要单独读：渠道 12 还没填倍率，但那个勾不能丢")

	// ChannelRatioMap 仍只给倍率（兼容只关心「有没有维护」的调用方）。
	onlyRatios, err := ChannelRatioMap(cfg)
	require.NoError(t, err)
	assert.Equal(t, ratios, onlyRatios)
}

// TestPGListChannelsCarriesIsDomestic 渠道清单页读到的国模标识与库里一致。
func TestPGListChannelsCarriesIsDomestic(t *testing.T) {
	cfg := testPG(t)
	require.NoError(t, EnsureChannelSchema(cfg))
	r := func(v float64) *float64 { return &v }

	require.NoError(t, upsertChannels(cfg, []ChannelInfo{
		{ChannelID: 1, Name: "国模渠道"}, {ChannelID: 2, Name: "海外渠道"}, {ChannelID: 3, Name: "没维护"},
	}))
	require.NoError(t, UpsertChannelRatios(cfg, []ChannelRatioInput{
		{ChannelID: 1, UpstreamRatio: r(0.4), IsDomestic: boolPtr(true)},
		{ChannelID: 2, UpstreamRatio: r(1.8)},
	}))

	list, err := ListChannels(cfg)
	require.NoError(t, err)
	byID := map[int]ChannelWithRatio{}
	for _, c := range list {
		byID[c.ChannelID] = c
	}
	require.Len(t, byID, 3)
	assert.True(t, byID[1].IsDomestic)
	assert.False(t, byID[2].IsDomestic)
	assert.False(t, byID[3].IsDomestic, "LEFT JOIN 没匹配到倍率行的渠道，标识是 false 而不是报错")
	assert.Nil(t, byID[3].UpstreamRatio)
}

// TestPGBillTaskReviewUpstreamRoundTrip 计划上的「核对上游」开关：建、改、读都要对得上。
//
// 加这一个字段要同时改六处 SQL，漏一处不会编译报错（INSERT 少一列只是位错位）。
// 源码级的 TestBillTaskColumnsMatchSchema 只查列名，这里用真实库把读写往返走一遍。
func TestPGBillTaskReviewUpstreamRoundTrip(t *testing.T) {
	cfg := testPG(t)
	require.NoError(t, EnsureCustomerSchema(cfg))
	require.NoError(t, EnsureBillTaskSchema(cfg))
	cust, err := UpsertCustomer(cfg, Customer{Name: "测试客户", Usernames: "u1"})
	require.NoError(t, err)

	base := BillTask{CustomerID: cust.ID, CustomerName: cust.Name, Name: "不核对",
		GenerateSanitized: true, CheckCost: true}

	// 默认（不设）= false：核对会打断每一次执行，不该在用户没表态时开启。
	off, err := CreateBillTask(cfg, base)
	require.NoError(t, err)
	got, err := GetBillTask(cfg, off.ID)
	require.NoError(t, err)
	assert.False(t, got.ReviewUpstream)

	// 勾上 → 读回 true，且别的开关不受影响。
	withReview := base
	withReview.Name = "要核对"
	withReview.ReviewUpstream = true
	on, err := CreateBillTask(cfg, withReview)
	require.NoError(t, err)
	got, err = GetBillTask(cfg, on.ID)
	require.NoError(t, err)
	assert.True(t, got.ReviewUpstream)
	assert.True(t, got.CheckCost, "相邻开关不能被带歪（列错位的典型表现）")
	assert.True(t, got.GenerateSanitized)
	assert.False(t, got.UseManualDiscount)
	assert.Equal(t, "要核对", got.Name)

	// 编辑：关、再开。
	got.ReviewUpstream = false
	require.NoError(t, UpdateBillTask(cfg, got))
	again, err := GetBillTask(cfg, on.ID)
	require.NoError(t, err)
	assert.False(t, again.ReviewUpstream)
	again.ReviewUpstream = true
	require.NoError(t, UpdateBillTask(cfg, again))
	again, err = GetBillTask(cfg, on.ID)
	require.NoError(t, err)
	assert.True(t, again.ReviewUpstream)

	// 执行结果落库只动结果字段，不得改动计划上的这个开关。
	// 传进去的 task 故意带着 ReviewUpstream=false，若 SaveBillTaskResult 误写了它，这里就会翻成 false。
	result := again
	result.ReviewUpstream = false
	result.SettleCNY = fptr(100)
	result.ListCNY = fptr(120)
	result.OverallDiscount = fptr(0.83)
	require.NoError(t, SaveBillTaskResult(cfg, result))
	after, err := GetBillTask(cfg, on.ID)
	require.NoError(t, err)
	assert.True(t, after.ReviewUpstream, "执行结果落库不能动计划字段")
	require.NotNil(t, after.SettleCNY)
	assert.Equal(t, 100.0, *after.SettleCNY, "结果字段照常落库")
}

// TestPGBillTaskSchemaAddsReviewUpstreamToOldTable 老库升级：补列，默认 false，老计划原样。
func TestPGBillTaskSchemaAddsReviewUpstreamToOldTable(t *testing.T) {
	cfg := testPG(t)
	require.NoError(t, EnsureCustomerSchema(cfg))
	require.NoError(t, EnsureBillTaskSchema(cfg))
	db := openPG(t, cfg)
	cust, err := UpsertCustomer(cfg, Customer{Name: "老客户", Usernames: "u1"})
	require.NoError(t, err)

	task, err := CreateBillTask(cfg, BillTask{
		CustomerID: cust.ID, CustomerName: cust.Name, Name: "老计划",
		GenerateSanitized: true, GenerateCost: true, CheckCost: true, UseManualDiscount: true,
	})
	require.NoError(t, err)

	// 模拟升级前的库：这一列还不存在。
	_, err = db.Exec(`ALTER TABLE bill_export_tasks DROP COLUMN review_upstream`)
	require.NoError(t, err)

	require.NoError(t, EnsureBillTaskSchema(cfg), "老库补列的迁移不能报错")
	require.NoError(t, EnsureBillTaskSchema(cfg), "且必须幂等")

	got, err := GetBillTask(cfg, task.ID)
	require.NoError(t, err)
	assert.False(t, got.ReviewUpstream, "老计划升级后不该突然开始每次弹窗打断执行")
	assert.Equal(t, "老计划", got.Name)
	assert.True(t, got.GenerateCost)
	assert.True(t, got.CheckCost)
	assert.True(t, got.UseManualDiscount, "其它开关原样保留")
}
