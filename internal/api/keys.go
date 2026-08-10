package api

import (
	"net/http"

	"aiops/internal/auth"

	"github.com/gin-gonic/gin"
)

// keys.go：访问密钥管理 + WebSocket ticket + 认证状态（KEEPON 4.3 简单访问保护）。
// 密钥管理端点均在受保护组内（owner key 先于 HTTP 存在，无引导死锁）。

// handleListAPIKeys 列出租户的密钥（不含哈希；含 owner 标记/最后使用时间）。
func (s *Server) handleListAPIKeys(c *gin.Context) {
	keys, err := s.authSvc.List(c.Request.Context(), tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": keys, "total": len(keys)})
}

// handleDeleteAPIKey 撤销密钥（删除后立即失效）。
func (s *Server) handleDeleteAPIKey(c *gin.Context) {
	if err := s.authSvc.Delete(c.Request.Context(), tenantID(c), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// handleRegenerateOwner 重新生成 owner key（旧 owner 撤销，明文仅返回一次）。
func (s *Server) handleRegenerateOwner(c *gin.Context) {
	secret, err := s.authSvc.RegenerateOwner(c.Request.Context(), tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"apiKey": secret, "tenantId": tenantID(c), "isOwner": true})
}

// handleWSTicket 签发 WebSocket 握手 ticket（受保护端点：authMiddleware 已注入租户）。
func (s *Server) handleWSTicket(c *gin.Context) {
	tok, err := s.tickets.Issue(tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ticket": tok, "ttlSec": int(s.tickets.TTL().Seconds())})
}

// handleAuthStatus 认证状态（公共端点，前端路由守卫判定：noauth 直通 / apikey 需解锁）。
func (s *Server) handleAuthStatus(c *gin.Context) {
	mode := "noauth"
	if s.cfg.AuthType == "apikey" {
		mode = "apikey"
	}
	hasKeys := false
	if s.authSvc != nil {
		n, err := s.authSvc.Count(c.Request.Context())
		if err == nil {
			hasKeys = n > 0
		}
	}
	c.JSON(http.StatusOK, gin.H{"mode": mode, "hasKeys": hasKeys})
}

// wsAuthMiddleware WebSocket 握手鉴权：apikey 模式校验 ?ticket=（Redeem 取回租户）；
// noauth 模式直接 default 租户。ticket 由受保护的 /ws/ticket 端点签发（浏览器 WS 无法带 header）。
func (s *Server) wsAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.cfg.AuthType != "apikey" {
			c.Set(ctxTenant, auth.DefaultTenant)
			c.Next()
			return
		}
		tok := c.Query("ticket")
		if tok == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing ticket"})
			return
		}
		tenant, err := s.tickets.Redeem(tok)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid ticket"})
			return
		}
		c.Set(ctxTenant, tenant)
		c.Next()
	}
}
