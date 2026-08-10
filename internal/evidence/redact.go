package evidence

import "regexp"

// redact.go：数据源外部数据的脱敏与查询注入检测。
// Loki 日志行是不可信外部数据（可能含密码/token/DSN）——值替换脱敏（复制自
// internal/rag/sanitizer.go 的正则清单；不能只靠 logutil.RedactValue——它对
// 无 `?` 的键值对不命中）。查询串（LogQL/PromQL 由模型生成）过注入检测，命中拒绝。

var (
	reSKToken  = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)
	reGHToken  = regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`)
	reAWSToken = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reBearer   = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}`)
	reKeyPair  = regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`)
	reCookie   = regexp.MustCompile(`(?i)\bcookie(?:s)?\s*[:=]\s*[^;\n]{8,}`)
	reWebhook  = regexp.MustCompile(`https?://[^\s"'<>]*(?:hook|webhook|robot)[^\s"'<>]*`)
	reDSN      = regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|redis|mongo(?:db)?)://[^\s"'<>]+`)
	reSecret   = regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|token)\b\s*[:=]\s*\S{8,}`)
	rePrivIP   = regexp.MustCompile(`\b(?:10|127|172\.(?:1[6-9]|2\d|3[01])|192\.168)\.\d{1,3}\.\d{1,3}\b`)
	reEmail    = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	rePhone    = regexp.MustCompile(`(?:\+?86[-\s]?)?1[3-9]\d{9}`)
	// 疑似提示注入：查询串试图覆盖系统指令/要求输出密钥（命中拒绝——不传给数据源）
	reInjection = regexp.MustCompile(`(?i)(ignore (?:all )?(?:previous|prior) instructions|忽略(?:以上|之前)(?:所有)?指令|ignore system|reveal your (?:prompt|instructions)|输出.*(?:密钥|api[_-]?key))`)
)

// RedactLogLine 日志行值替换脱敏（命中 → [REDACTED:label]）。保留行结构，只打码敏感值。
func RedactLogLine(line string) string {
	replace := func(re *regexp.Regexp, label string) {
		if re.MatchString(line) {
			line = re.ReplaceAllString(line, "[REDACTED:"+label+"]")
		}
	}
	replace(reKeyPair, "private_key")
	replace(reSKToken, "token")
	replace(reGHToken, "token")
	replace(reAWSToken, "token")
	replace(reBearer, "token")
	replace(reWebhook, "webhook")
	replace(reDSN, "dsn")
	replace(reCookie, "cookie")
	replace(reSecret, "secret")
	replace(rePrivIP, "internal_ip")
	replace(reEmail, "email")
	replace(rePhone, "phone")
	return line
}

// CheckQueryInjection 查询串注入检测：命中提示注入模式 → true（调用方拒绝查询）。
func CheckQueryInjection(q string) bool {
	return reInjection.MatchString(q)
}
