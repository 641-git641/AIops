package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// authMiddleware 租户认证：noauth 模式固定 default 租户（本地/开发），
// apikey 模式校验 X-API-Key（sha256 查库）后注入租户。
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tenant := "default"
		switch s.cfg.AuthType {
		case "apikey":
			secret := c.GetHeader("X-API-Key")
			if secret == "" {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing X-API-Key header"})
				return
			}
			tid, err := s.authSvc.Verify(c.Request.Context(), secret)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
				return
			}
			tenant = tid
		}
		c.Set(ctxTenant, tenant)
		c.Next()
	}
}
