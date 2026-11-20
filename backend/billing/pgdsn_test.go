package billing

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// 密码含 @ : / # 等字符时，连接串必须仍能被解析回原样，且主机名不受影响。
func TestPGConfigDSNEscapesPassword(t *testing.T) {
	cfg := PGConfig{Host: "postgres", Port: "5432", User: "billtool", Password: "hu14:08@17/#?x..", DBName: "billtool"}
	parsed, err := pgx.ParseConfig(cfg.dsn())
	require.NoError(t, err)
	require.Equal(t, "postgres", parsed.Host)
	require.Equal(t, uint16(5432), parsed.Port)
	require.Equal(t, "billtool", parsed.User)
	require.Equal(t, "hu14:08@17/#?x..", parsed.Password)
	require.Equal(t, "billtool", parsed.Database)
}
