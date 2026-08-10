package rag

import (
	"errors"
	"regexp"
	"strings"
)

// 脱敏（RAG_PLAN §8.3）：Embedding 前执行字段白名单与敏感信息扫描。
// 移除 Token/密码/Cookie/Webhook/连接串/私钥；内网地址、邮箱、手机号脱敏。
// 对疑似提示注入/未分类密钥文本返回错误——脱敏失败任务进入 failed/dead，
// 不能绕过后继续提交外部模型（§8.3：脱敏失败时任务必须失败）。
//
// 注意：这不是通用安全清洗器，只覆盖知识库字段白名单内的敏感模式；
// 检索正文与向量不得包含此处捕获的原始值。
// 已知盲区（启发式局限，运维手册列明）：无冒号/等号的裸密码（"password xxx"）、
// 值含空格或 <8 字符的密钥、公网出口 IP 等不命中且不报错——高风险租户应使用
// 本地 Embedding 模型或完全禁用外部调用（RAG_PLAN §14.4）。

var (
	reSKToken  = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)
	reGHToken  = regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`)
	reAWSToken = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reBearer   = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}`)
	reKeyPair  = regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`)
	reCookie   = regexp.MustCompile(`(?i)\bcookie(?:s)?\s*[:=]\s*[^;\n]{8,}`)
	reWebhook  = regexp.MustCompile(`https?://[^\s"'<>]*(?:hook|webhook|robot)[^\s"'<>]*`)
	reDSN      = regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|redis|mongo(?:db)?)://[^\s"'<>]+`)
	// 已知键名（password/token/secret 类）：值按 secret 脱敏（键名已知可安全归类）
	reSecret = regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|token)\b\s*[:=]\s*\S{8,}`)
	// 外部服务 key 类名称：值格式不可知（AWS/支付宝等），保守失败而非猜测脱敏
	reSuspiciousKey = regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?key|private[_-]?key|secret[_-]?key)\b`)
	rePrivIP        = regexp.MustCompile(`\b(?:10|127|172\.(?:1[6-9]|2\d|3[01])|192\.168)\.\d{1,3}\.\d{1,3}\b`)
	reEmail         = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	rePhone         = regexp.MustCompile(`(?:\+?86[-\s]?)?1[3-9]\d{9}`)
	// 疑似提示注入：文档试图覆盖系统指令/要求输出密钥/调用工具（仅打风险标签，不阻断）
	reInjection = regexp.MustCompile(`(?i)(ignore (?:all )?(?:previous|prior) instructions|忽略(?:以上|之前)(?:所有)?指令|ignore system|reveal your (?:prompt|instructions)|输出.*(?:密钥|api[_-]?key))`)
)

// SanitizeResult 脱敏结果。
type SanitizeResult struct {
	Text      string   // 脱敏后文本
	Redacted  []string // 命中的敏感类别（观测用）
	Injection bool     // 疑似提示注入风险标记
}

// Sanitize 对知识正文执行脱敏与风险扫描。疑似未分类密钥（如 api key 名出现但值无法归类）
// 返回 error，调用方必须使任务失败而不是继续提交外部模型。
func Sanitize(text string) (SanitizeResult, error) {
	var res SanitizeResult
	if reInjection.MatchString(text) {
		res.Injection = true
	}
	// 未分类疑似密钥：外部服务 key 类名称（值格式不可知）→ 保守失败，
	// 不允许猜测脱敏后把真实密钥送出（§8.3：脱敏失败任务必须失败）
	if reSuspiciousKey.MatchString(text) {
		return res, errors.New("sanitize: 疑似未分类密钥（api/access/private/secret key 类名称），拒绝提交外部模型")
	}
	replace := func(re *regexp.Regexp, label string) {
		if re.MatchString(text) {
			text = re.ReplaceAllString(text, "[REDACTED:"+label+"]")
			res.Redacted = append(res.Redacted, label)
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
	res.Text = strings.TrimSpace(text)
	return res, nil
}
