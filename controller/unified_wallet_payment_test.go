package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedWalletEpayVerifiedCallbackFreezesActualPrincipal(t *testing.T) {
	for _, row := range []struct {
		name    string
		money   float64
		charged string
		points  string
	}{
		{"half-cent-below", 1.005, "1.00", "5000"},
		{"half-cent-above", 2.675, "2.67", "13350"},
	} {
		t.Run(row.name, func(t *testing.T) {
			setupEpayCallbackTest(t)
			db := setupModelListControllerTestDB(t)
			t.Setenv("UNIFIED_WALLET_ENABLED", "true")
			t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
			originalMemory, originalBatch, originalQuota := common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.QuotaPerUnit
			common.MemoryCacheEnabled = false
			common.BatchUpdateEnabled = false
			common.QuotaPerUnit = 500000
			t.Cleanup(func() {
				common.MemoryCacheEnabled = originalMemory
				common.BatchUpdateEnabled = originalBatch
				common.QuotaPerUnit = originalQuota
			})
			operation_setting.GetPaymentSetting().EpaySignType = operation_setting.EpaySignTypeMD5
			operation_setting.EpayKey = "wallet-test-signing-key"
			require.NoError(t, db.AutoMigrate(&model.TopUp{}, &model.Log{}))
			require.NoError(t, model.MigrateUnifiedWallet(db))
			user := model.User{Username: "wallet-epay", Status: common.UserStatusEnabled, Quota: 0}
			require.NoError(t, db.Create(&user).Error)
			order := model.TopUp{UserId: user.Id, TradeNo: "wallet-order", PaymentProvider: model.PaymentProviderEpay, PaymentMethod: "alipay", Amount: 10, Money: row.money, Status: common.TopUpStatusPending}
			require.NoError(t, db.Create(&order).Error)
			client := GetEpayClient()
			require.NotNil(t, client)
			signed, err := client.SignParams(map[string]string{"type": "alipay", "money": row.charged, "name": "TUC10", "out_trade_no": order.TradeNo, "trade_no": "provider-wallet-order", "trade_status": epay.StatusTradeSuccess})
			require.NoError(t, err)
			router := gin.New()
			router.POST("/api/user/epay/notify", EpayNotify)
			call := func(params map[string]string) *httptest.ResponseRecorder {
				values := url.Values{}
				for key, value := range params {
					values.Set(key, value)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/user/epay/notify", strings.NewReader(values.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				return recorder
			}
			tampered := map[string]string{}
			for key, value := range signed {
				tampered[key] = value
			}
			tampered["money"] = "999.00"
			assert.Equal(t, "fail", call(tampered).Body.String())
			var before model.User
			require.NoError(t, db.First(&before, user.Id).Error)
			assert.Zero(t, before.Quota)
			for i := 0; i < 2; i++ {
				response := call(signed)
				require.Equal(t, http.StatusOK, response.Code)
				require.Equal(t, "success", response.Body.String())
			}
			view, err := model.GetUnifiedWallet(user.Id)
			require.NoError(t, err)
			assert.Equal(t, "50000", view.PermanentPoints, "permanent points represent the entire available quota")
			var principal model.UnifiedWalletLot
			require.NoError(t, db.Where("user_id = ? AND kind = ?", user.Id, model.WalletPaid).First(&principal).Error)
			assert.Equal(t, row.points, fmt.Sprint(principal.InitialPoints), "the paid principal remains preserved for payment reversals")
			assert.Equal(t, "0", view.CharityPoints)
			assert.Equal(t, int64(5000000), view.TotalAPIQuota)
			assert.Equal(t, view.TotalAPIQuota, view.PaidAPIQuota)
			assert.Zero(t, view.LegacyAPIQuota)
			var lots int64
			require.NoError(t, db.Model(&model.UnifiedWalletLot{}).Where("kind = ?", model.WalletPaid).Count(&lots).Error)
			assert.Equal(t, int64(1), lots)
			require.NoError(t, db.First(&order, order.Id).Error)
			assert.True(t, order.PaymentVerified)
			assert.Equal(t, "CNY", order.PaymentCurrency)
			assert.Equal(t, "provider-wallet-order", order.PaymentReference)
			assert.Equal(t, row.charged, fmt.Sprintf("%.2f", float64(order.PaymentPrincipalFen)/100))
		})
	}
}

func TestSharedWalletStatusSignsCommittedAmountAndRejectsReplay(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	t.Setenv("QXCHAT_SHARED_WALLET_ENABLED", "1")
	secret := "shared-wallet-callback-test-secret-32-characters"
	t.Setenv("QXCHAT_MIGRATION_SECRET", secret)
	require.NoError(t, model.MigrateUnifiedWallet(db))
	user := model.User{Username: "wallet-callback", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	reservation := model.WalletReference("callback-reservation")
	operation := model.UnifiedWalletOperation{ID: model.WalletReference("callback-operation"), UserID: user.Id,
		Mode: model.WalletCommunity, State: "settled", Quota: 123,
		Allocations: []model.WalletAllocation{{Kind: model.WalletQXChat, Reference: reservation, Quota: 123}}}
	require.NoError(t, db.Create(&operation).Error)
	input := model.QXChatWalletReservationStatus{ReservationID: reservation, OperationID: operation.ID, UserID: user.Id}
	body, err := common.Marshal(input)
	require.NoError(t, err)
	const path = "/api/qxchat/wallet/reservations/status"
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := strings.Repeat("a", 32)
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{"qxchat-wallet-v1", stamp, nonce, "POST", path, hex.EncodeToString(digest[:])}, "\n")
	router := gin.New()
	RegisterUnifiedWalletRoutes(router.Group("/api"))
	call := func(signature string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Migration-Timestamp", stamp)
		request.Header.Set("X-Migration-Nonce", nonce)
		request.Header.Set("X-Migration-Signature", signature)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	assert.Equal(t, http.StatusUnauthorized, call(strings.Repeat("0", 64)).Code)
	signature := common.GenerateHMACWithKey([]byte(secret), canonical)
	response := call(signature)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var status model.QXChatWalletReservationStatus
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &status))
	assert.Equal(t, reservation, status.ReservationID)
	assert.Equal(t, int64(123), status.Quota)
	assert.True(t, status.Terminal)
	digest = sha256.Sum256(response.Body.Bytes())
	canonical = strings.Join([]string{"qxchat-wallet-response-v1", nonce, "200", hex.EncodeToString(digest[:])}, "\n")
	assert.Equal(t, common.GenerateHMACWithKey([]byte(secret), canonical), response.Header().Get("X-Migration-Signature"))
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	assert.Equal(t, http.StatusUnauthorized, call(signature).Code)
}
