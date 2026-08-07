package notifier

import (
	"time"
)

// SupportedChannel 支持的渠道名。
const (
	ChannelDingTalk = "dingtalk"
	ChannelFeishu   = "feishu"
	ChannelWecom    = "wecom"
	ChannelHTTP     = "http"
)

// supportedChannels 渠道名集合（合法渠道）。
var supportedChannels = map[string]bool{
	ChannelDingTalk: true,
	ChannelFeishu:   true,
	ChannelWecom:    true,
	ChannelHTTP:     true,
}

// Supported 判断渠道名是否受支持。
func Supported(name string) bool { return supportedChannels[name] }

// ChannelConfig 通知渠道配置（DB 持久化，env 仅作首启种子，之后 DB 为准）。
// 字段与 Message 无关，仅描述「渠道启用 + webhook 地址」。
type ChannelConfig struct {
	Name      string    `gorm:"primaryKey;size:16" json:"name"`
	Enabled   bool      `json:"enabled"`
	Webhook   string    `gorm:"size:512" json:"webhook"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TableName 表名。
func (ChannelConfig) TableName() string { return "notification_channels" }
