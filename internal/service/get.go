package service

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/hengfeiyang/lsmdb/internal/pkg/db"
)

func Get(c *gin.Context) {
	val, err := db.DB.Query(c.Param("key"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"status": 1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": 0, "value": val})
}
