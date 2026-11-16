package openai

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestUnifiedWalletRealtimeErrorDoesNotRepeatDrainedUsage(t *testing.T) {
	oldDB, oldMemory, oldRedis := model.DB, common.MemoryCacheEnabled, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:wallet_realtime_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}))
	model.DB = db
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = oldDB
		common.MemoryCacheEnabled = oldMemory
		common.RedisEnabled = oldRedis
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	usage := &dto.RealtimeUsage{TotalTokens: 3, InputTokens: 1, OutputTokens: 2}
	total := &dto.RealtimeUsage{}
	// A missing owner produces a real billing failure after the response's
	// usage has already been added to total, as exhaustion does in production.
	_, err = preConsumeUsage(ctx, &relaycommon.RelayInfo{UserId: 999}, usage, total)
	require.Error(t, err)
	assert.Equal(t, dto.RealtimeUsage{}, *usage)
	assert.Equal(t, 3, total.TotalTokens)
	if usage.TotalTokens != 0 {
		_, _ = preConsumeUsage(ctx, &relaycommon.RelayInfo{UserId: 999}, usage, total)
	}
	assert.Equal(t, 3, total.TotalTokens)
}
