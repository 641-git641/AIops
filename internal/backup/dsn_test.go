package backup

import "testing"

func TestParseDSN(t *testing.T) {
	tgt, err := ParseDSN("host=127.0.0.1 user=aiops password=aiops dbname=aiops port=5432 sslmode=disable TimeZone=Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Host != "127.0.0.1" || tgt.User != "aiops" || tgt.Password != "aiops" ||
		tgt.DBName != "aiops" || tgt.Port != "5432" {
		t.Fatalf("parse wrong: %+v", tgt)
	}
	// 引号值 + 缺 port 兜底
	tgt, err = ParseDSN("host='db.example.com' user=u password='p a s s' dbname=d")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Host != "db.example.com" || tgt.Password != "p a s s" || tgt.Port != "5432" {
		t.Fatalf("quoted parse wrong: %+v", tgt)
	}
	// 缺关键字段报错
	if _, err := ParseDSN("host=h user=u"); err == nil {
		t.Fatal("missing dbname must error")
	}
	// hostaddr 兼容
	tgt, err = ParseDSN("hostaddr=10.0.0.1 user=u dbname=d")
	if err != nil || tgt.Host != "10.0.0.1" {
		t.Fatalf("hostaddr wrong: %+v, %v", tgt, err)
	}
}

func TestPgArgs(t *testing.T) {
	tgt := PgTarget{Host: "h", Port: "5433", User: "u", DBName: "d"}
	args := tgt.pgArgs()
	if len(args) != 6 || args[1] != "h" || args[3] != "5433" || args[5] != "u" {
		t.Fatalf("pgArgs wrong: %v", args)
	}
}
