package external

import (
	"testing"
)

// ValidateURL 协议白名单（防 javascript: 等注入）。
func TestValidateURL(t *testing.T) {
	valid := []string{"http://localhost:3000", "https://grafana.example.com/path?x=1", "http://127.0.0.1:9090"}
	for _, u := range valid {
		if err := ValidateURL(u); err != nil {
			t.Fatalf("%s 应合法: %v", u, err)
		}
	}
	invalid := []string{"javascript:alert(1)", "ftp://x.com", "file:///etc/passwd", "http://", "//x.com"}
	for _, u := range invalid {
		if err := ValidateURL(u); err == nil {
			t.Fatalf("%s 应被拒绝", u)
		}
	}
}

// ValidType 类型白名单。
func TestValidType(t *testing.T) {
	for _, typ := range []string{TypeGrafana, TypePrometheus, TypeLogs, TypeOther} {
		if !ValidType(typ) {
			t.Fatalf("%s 应合法", typ)
		}
	}
	if ValidType("kibana") || ValidType("") {
		t.Fatal("未知类型应拒绝")
	}
}
