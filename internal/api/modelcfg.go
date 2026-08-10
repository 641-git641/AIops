package api

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"aiops/internal/modelcfg"

	"github.com/gin-gonic/gin"
)

// modelcfg.go：大模型 API 接入管理端点（model_configs 表，DB 为准、env 仅种子）。
// 端点携带 LLM/Embedding 密钥，管理门控独立于 RAG 管理开关（AIOPS_MODELCFGADMINAPIS）。

// handleListModelConfigs 配置列表（固定两行：chat/embedding；apiKey 脱敏为 apiKeySet）。
func (s *Server) handleListModelConfigs(c *gin.Context) {
	if !s.modelCfgAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_MODELCFGADMINAPIS）"})
		return
	}
	items, err := s.mc.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleUpdateModelConfig 保存配置（apiKey 传空 = 保留旧值；保存后重启后端生效）。
func (s *Server) handleUpdateModelConfig(c *gin.Context) {
	if !s.modelCfgAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_MODELCFGADMINAPIS）"})
		return
	}
	key := c.Param("key")
	var in modelcfg.SaveInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	dto, err := s.mc.Save(c.Request.Context(), key, in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, dto)
}

// handleTestModelConfig 连通性测试（表单值优先、DB 兜底；不落库）。
func (s *Server) handleTestModelConfig(c *gin.Context) {
	if !s.modelCfgAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_MODELCFGADMINAPIS）"})
		return
	}
	key := c.Param("key")
	var in modelcfg.SaveInput
	if err := c.ShouldBindJSON(&in); err != nil && !errors.Is(err, io.EOF) {
		// 空 body 允许（等价于测 DB 现值）；畸形 JSON 明确报错
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, _ := s.mc.Test(c.Request.Context(), key, in) // 错误已折进 TestResult.Error
	c.JSON(http.StatusOK, res)
}

// modelCfgAdminAllowed 模型配置管理门控：AIOPS_MODELCFGADMINAPIS=false 一律拒绝；
// apikey 模式仅白名单租户可访问（与 ragAdminAllowed 同构，独立开关）。
func (s *Server) modelCfgAdminAllowed(c *gin.Context) bool {
	if !s.cfg.ModelCfgAdminAPIs {
		return false
	}
	if s.cfg.AuthType == "apikey" {
		cur := tenantID(c)
		for _, t := range strings.Split(s.cfg.ModelCfgAdminTenants, ",") {
			if strings.TrimSpace(t) == cur {
				return true
			}
		}
		return false
	}
	return true
}
