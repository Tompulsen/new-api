package controller

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func GetUserUsageLogs(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	for key, values := range c.Request.URL.Query() {
		if len(values) != 1 || (key != "start_timestamp" && key != "end_timestamp" && key != "p" && key != "page_size" && key != "model_name") {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
	}
	start, end, valid := parseFlowQuotaTimeRange(c)
	if !valid {
		return
	}
	if _, err := limitLogTimeRange(start, end); err != nil {
		common.ApiError(c, err)
		return
	}
	page, pageErr := strconv.Atoi(c.DefaultQuery("p", "1"))
	size, sizeErr := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pageErr != nil || sizeErr != nil || page < 1 || page > 1000000 || size < 1 || size > 100 {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	var selected *string
	if names, exists := c.Request.URL.Query()["model_name"]; exists {
		name := names[0]
		if len(name) > 512 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		selected = &name
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	result, err := model.GetUserUsageLogs(ctx, c.GetInt("id"), start, end, selected, (page-1)*size, size)
	if err != nil {
		common.SysError(common.LogText("failed to query log stat: %s", err.Error()))
		common.ApiErrorT(c, "Failed to query statistics")
		return
	}
	common.ApiSuccess(c, result)
}
