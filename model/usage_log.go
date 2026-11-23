package model

import (
	"context"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// Consumption and refunds are ledger entries, not distinct model requests.
type UsageModelSummary struct {
	ModelName        string `json:"model_name"`
	ConsumeQuota     int64  `json:"consume_quota"`
	RefundQuota      int64  `json:"refund_quota"`
	ConsumeRecords   int64  `json:"consume_records"`
	RefundRecords    int64  `json:"refund_records"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

type UserUsageLogs struct {
	Items  []*Log              `json:"items"`
	Models []UsageModelSummary `json:"models"`
}

func GetUserUsageLogs(ctx context.Context, userID int, start, end int64, modelName *string, offset, size int) (UserUsageLogs, error) {
	result := UserUsageLogs{Items: make([]*Log, 0), Models: make([]UsageModelSummary, 0)}
	base := func() *gorm.DB {
		return LOG_DB.WithContext(ctx).Model(&Log{}).
			Where("user_id = ? AND created_at >= ? AND created_at <= ?", userID, start, end).
			Where("type IN ?", []int{LogTypeConsume, LogTypeRefund})
	}
	modelColumn := "model_name"
	if common.UsingLogDatabase(common.DatabaseTypeMySQL) {
		// Model IDs must remain distinct under case-insensitive MySQL collations.
		modelColumn = "CAST(model_name AS BINARY)"
	}
	// Keep the full model list independent of the selected model and page.
	err := base().Select(modelColumn+` AS model_name,
		SUM(CASE WHEN type = ? THEN quota ELSE 0 END) AS consume_quota,
		SUM(CASE WHEN type = ? THEN quota ELSE 0 END) AS refund_quota,
		SUM(CASE WHEN type = ? THEN 1 ELSE 0 END) AS consume_records,
		SUM(CASE WHEN type = ? THEN 1 ELSE 0 END) AS refund_records,
		SUM(CASE WHEN type = ? THEN prompt_tokens ELSE 0 END) AS prompt_tokens,
		SUM(CASE WHEN type = ? THEN completion_tokens ELSE 0 END) AS completion_tokens`,
		LogTypeConsume, LogTypeRefund, LogTypeConsume, LogTypeRefund, LogTypeConsume, LogTypeConsume).
		Group(modelColumn).Order("consume_quota DESC, model_name ASC").Scan(&result.Models).Error
	if err != nil {
		return result, err
	}
	query := base()
	if modelName != nil {
		// Model IDs are literal, including percent, underscores and slashes.
		query = query.Where(modelColumn+" = ?", *modelName)
	}
	order := "id DESC"
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		order = clickHouseLogOrder("")
	}
	if err = query.Order(order).Offset(offset).Limit(size).Find(&result.Items).Error; err != nil {
		return result, err
	}
	formatUserLogs(result.Items, offset)
	return result, nil
}
