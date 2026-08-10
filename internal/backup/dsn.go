package backup

import (
	"fmt"
	"strings"
)

// dsn.go：libpq keyword/value DSN 解析（store.DefaultDSN 同格式）。
// pg_dump/pg_restore 参数由此生成；密码只走 PGPASSWORD 环境变量，绝不进入 argv（防 ps 泄漏）。

// PgTarget 从 DSN 提取的连接目标。
type PgTarget struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
}

// ParseDSN 解析 libpq keyword=value DSN（支持 '引号值'，引号内空白不拆），提取连接目标。
func ParseDSN(dsn string) (PgTarget, error) {
	var t PgTarget
	for _, field := range splitDSNFields(dsn) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := strings.ToLower(kv[0]), kv[1]
		val = strings.Trim(val, "'")
		switch key {
		case "host", "hostaddr":
			t.Host = val
		case "port":
			t.Port = val
		case "user":
			t.User = val
		case "password":
			t.Password = val
		case "dbname":
			t.DBName = val
		}
	}
	if t.Host == "" || t.User == "" || t.DBName == "" {
		return t, fmt.Errorf("backup: DSN 缺少 host/user/dbname（%q）", dsn)
	}
	if t.Port == "" {
		t.Port = "5432"
	}
	return t, nil
}

// pgArgs 构造 pg_dump/pg_restore 的通用连接参数（-h/-p/-U；dbname 单独作为位置参数）。
func (t PgTarget) pgArgs() []string {
	return []string{"-h", t.Host, "-p", t.Port, "-U", t.User}
}

// splitDSNFields 按空白拆分 DSN，但单引号内的空白不拆（libpq 引号值语义）。
func splitDSNFields(dsn string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for _, r := range dsn {
		switch {
		case r == '\'':
			inQuote = !inQuote
			cur.WriteRune(r)
		case (r == ' ' || r == '\t') && !inQuote:
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields
}
