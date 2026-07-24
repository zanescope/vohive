package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/pkg/logger"
)

func (s *Server) handleClearLogs(c *gin.Context) {
	if err := logger.ClearAllLogs(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "clear_logs_failed",
			"message": "清空日志失败: " + err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cleared": true})
}
