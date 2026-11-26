package controller

import (
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCommunityLotteryConsumption(t *testing.T) {
	db, dialect := openTaskDialectDatabase(t, &model.User{})
	oldDB, oldDialect := model.DB, common.MainDatabaseType()
	model.DB = db
	common.SetDatabaseTypes(dialect, common.LogDatabaseType())
	t.Cleanup(func() {
		model.DB = oldDB
		common.SetDatabaseTypes(oldDialect, common.LogDatabaseType())
	})
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	secret := strings.Repeat("lottery-consumption-test", 3)
	t.Setenv("COMMUNITY_LOTTERY_SECRET", secret)
	user := model.User{Username: "consumption-user", AffCode: "consumption-user", Status: common.UserStatusEnabled, Quota: 5000000}
	other := model.User{Username: "consumption-other", AffCode: "consumption-other", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	require.NoError(t, db.Create(&other).Error)
	call := func(query, credential string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/community-sso/lottery"+query, nil)
		ctx.Request.Header.Set("Authorization", "Bearer "+credential)
		CommunityLotteryCredit(ctx)
		return recorder
	}
	queryFor := func(id int) string {
		return "?" + url.Values{"client_id": {communityTestOrigin}, "user_id": {strconv.Itoa(id)}}.Encode()
	}
	response := call("", secret)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var readiness map[string]any
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &readiness))
	assert.Equal(t, map[string]any{"ready": true, "client_id": communityTestOrigin, "reward_kinds": []any{"balance", "charity"}, "quota_per_point": float64(100), "exact_points": true, "quota_per_usd": float64(500000)}, readiness)
	assert.Equal(t, http.StatusUnauthorized, call(queryFor(user.Id), "invalid").Code)
	for _, query := range []string{
		"?client_id=" + url.QueryEscape(communityTestOrigin),
		"?user_id=1",
		queryFor(user.Id) + "&user_id=2",
		queryFor(user.Id) + "&client_id=" + url.QueryEscape(communityTestOrigin),
		queryFor(user.Id) + "&extra=1",
		"?client_id=https%3A%2F%2Funtrusted.test&user_id=1",
		"?client_id=%&user_id=1",
	} {
		assert.Equal(t, http.StatusBadRequest, call(query, secret).Code, query)
	}
	for _, id := range []string{"", "0", "-1", "1.5", "+1", "01", "9007199254740992", "9223372036854775808"} {
		query := "?" + url.Values{"client_id": {communityTestOrigin}, "user_id": {id}}.Encode()
		assert.Equal(t, http.StatusBadRequest, call(query, secret).Code, id)
	}
	assert.Equal(t, http.StatusBadRequest, call(queryFor(other.Id+1), secret).Code)
	assert.Equal(t, http.StatusBadRequest, call(queryFor(common.MaxWalletQuota), secret).Code, "safe IDs reach the enabled-user check")
	require.NoError(t, db.Model(&other).Update("status", common.UserStatusDisabled).Error)
	assert.Equal(t, http.StatusBadRequest, call(queryFor(other.Id), secret).Code)
	require.NoError(t, db.Model(&other).Update("status", common.UserStatusEnabled).Error)

	type consumptionCase struct {
		name   string
		used   int
		rate   string
		fen    int64
		has    bool
		status int
	}
	cases := []consumptionCase{
		{name: "balance without consumption", rate: "500000"},
		{name: "historical consumption without topups", used: 1234567, rate: "500000", fen: 246, has: true},
		{name: "less than one fen", used: 1, rate: "500000", has: true},
		{name: "floor fractional fen", used: 9999, rate: "500000", fen: 1, has: true},
		{name: "usage reset after refund", rate: "500000"},
		{name: "negative usage after refund", used: -1, rate: "500000"},
		{name: "smallest valid rate", used: math.MaxInt32, rate: "5000", fen: 42949672, has: true},
		{name: "largest valid rate", used: math.MaxInt32, rate: "9007199254740000", has: true},
	}
	if dialect == common.DatabaseTypeSQLite {
		cases = append(cases,
			consumptionCase{name: "minimum int64 usage", used: math.MinInt64, rate: "500000"},
			consumptionCase{name: "raw usage above safe integer", used: common.MaxWalletQuota + 1, rate: "500000", fen: 1801439850948, has: true},
			consumptionCase{name: "maximum int64 usage", used: math.MaxInt64, rate: "500000", fen: 1844674407370955, has: true},
			consumptionCase{name: "maximum safe converted fen", used: common.MaxWalletQuota*50 + 49, rate: "5000", fen: common.MaxWalletQuota, has: true},
			consumptionCase{name: "converted fen overflow", used: (common.MaxWalletQuota + 1) * 50, rate: "5000", status: http.StatusServiceUnavailable},
		)
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", test.rate)
			require.NoError(t, db.Model(&user).Update("used_quota", test.used).Error)
			response := call(queryFor(user.Id), secret)
			if test.status != 0 {
				assert.Equal(t, test.status, response.Code, response.Body.String())
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var consumption struct {
				Ready             bool     `json:"ready"`
				ClientID          string   `json:"client_id"`
				UserID            int      `json:"user_id"`
				Basis             string   `json:"basis"`
				ConsumptionCNYFen int64    `json:"consumption_cny_fen"`
				HasConsumption    bool     `json:"has_consumption"`
				RewardKinds       []string `json:"reward_kinds"`
				QuotaPerPoint     int64    `json:"quota_per_point"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &consumption))
			assert.True(t, consumption.Ready)
			assert.Equal(t, communityTestOrigin, consumption.ClientID)
			assert.Equal(t, user.Id, consumption.UserID)
			assert.Equal(t, "consumption", consumption.Basis)
			assert.Equal(t, test.fen, consumption.ConsumptionCNYFen)
			assert.Equal(t, test.has, consumption.HasConsumption)
			assert.Equal(t, []string{"balance", "charity"}, consumption.RewardKinds)
			rate, err := strconv.ParseInt(test.rate, 10, 64)
			require.NoError(t, err)
			assert.Equal(t, rate/5000, consumption.QuotaPerPoint)
			assert.NotContains(t, response.Body.String(), `"support_cny_fen"`)
			assert.NotContains(t, response.Body.String(), `"has_support"`)
		})
	}
	t.Setenv("UNIFIED_WALLET_ENABLED", "false")
	response = call("", secret)
	require.Equal(t, http.StatusOK, response.Code)
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &readiness))
	assert.Equal(t, []any{"balance"}, readiness["reward_kinds"])
	assert.Equal(t, float64(100), readiness["quota_per_point"])
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	require.NoError(t, db.Model(&user).Update("used_quota", 0).Error)
	for _, rate := range []string{"", "0", "-5000", "5001", "9007199254740991", "9007199254745000", "9223372036854775808"} {
		t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", rate)
		assert.Equal(t, http.StatusServiceUnavailable, call(queryFor(user.Id), secret).Code, rate)
		response = call("", secret)
		require.Equal(t, http.StatusOK, response.Code, rate)
		var unavailable map[string]any
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &unavailable))
		assert.Equal(t, []any{"balance"}, unavailable["reward_kinds"])
		assert.NotContains(t, unavailable, "quota_per_point")
	}
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	require.NoError(t, db.Delete(&user).Error)
	assert.Equal(t, http.StatusBadRequest, call(queryFor(user.Id), secret).Code)
}

func TestCommunityLotterySupport(t *testing.T) {
	db, dialect := openTaskDialectDatabase(t, &model.User{}, &model.TopUp{}, &model.UnifiedWalletPaymentReceipt{}, &model.Log{})
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(dialect, dialect)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMain, oldLog)
	})
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	secret := strings.Repeat("lottery-support-test", 3)
	t.Setenv("COMMUNITY_LOTTERY_SECRET", secret)
	user := model.User{Username: "support-user", AffCode: "support-user", Status: common.UserStatusEnabled, Quota: 5000000, UsedQuota: 5000000}
	other := model.User{Username: "support-other", AffCode: "support-other", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	require.NoError(t, db.Create(&other).Error)
	query := "?" + url.Values{"client_id": {communityTestOrigin}, "user_id": {strconv.Itoa(user.Id)}, "basis": {"support"}}.Encode()
	call := func(query, credential string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/community-sso/lottery"+query, nil)
		ctx.Request.Header.Set("Authorization", "Bearer "+credential)
		CommunityLotteryCredit(ctx)
		return response
	}
	assert.Equal(t, http.StatusUnauthorized, call(query, "wrong").Code)
	for _, invalid := range []string{
		query + "&basis=support", query + "&extra=1",
		strings.Replace(query, "basis=support", "basis=consumption", 1),
		strings.Replace(query, "basis=support", "basis=", 1),
		"?basis=support", "?basis=support&user_id=1",
	} {
		assert.Equal(t, http.StatusBadRequest, call(invalid, secret).Code, invalid)
	}
	legacy := func(provider, method string, money float64) model.TopUp {
		return model.TopUp{PaymentProvider: provider, PaymentMethod: method, Money: money}
	}
	verified := func(currency, reference string, principal, refunded int64) model.TopUp {
		return model.TopUp{PaymentProvider: model.PaymentProviderEpay, PaymentCurrency: currency, PaymentReference: reference,
			PaymentVerified: true, PaymentPositive: true, PaymentPrincipalFen: principal, PaymentRefundedFen: refunded, Money: 99999}
	}
	adminLog := func(request, action, mode string, from, to any) model.Log {
		data, err := common.Marshal(model.AuditOther{
			AdminInfo: &model.AuditAdminInfo{AdminID: 99, AdminRole: common.RoleRootUser},
			Op: &model.AuditOperation{Action: action, Params: model.AuditFields{
				"target_user_id": user.Id, "mode": mode, "from": from, "to": to,
			}},
		})
		require.NoError(t, err)
		return model.Log{UserId: user.Id, Type: model.LogTypeTopup, RequestId: request, Other: string(data)}
	}
	failedLog := adminLog("failed", "user.quota_add", "add", 0, 500000)
	var failedMetadata model.AuditOther
	require.NoError(t, common.UnmarshalJsonStr(failedLog.Other, &failedMetadata))
	failedMetadata.AuditInfo = &model.AuditRequestInfo{Success: false, Status: 500}
	failedData, err := common.Marshal(failedMetadata)
	require.NoError(t, err)
	failedLog.Other = string(failedData)
	wrongRoleLog := adminLog("wrong-role", "user.quota_add", "add", 0, 500000)
	var wrongRoleMetadata model.AuditOther
	require.NoError(t, common.UnmarshalJsonStr(wrongRoleLog.Other, &wrongRoleMetadata))
	wrongRoleMetadata.AdminInfo.AdminRole = common.RoleCommonUser
	wrongRoleData, err := common.Marshal(wrongRoleMetadata)
	require.NoError(t, err)
	wrongRoleLog.Other = string(wrongRoleData)
	noPositive := verified("CNY", "non-positive", 500, 0)
	noPositive.PaymentPositive = false
	tests := []struct {
		name     string
		payments []model.TopUp
		logs     []model.Log
		orphan   bool
		fen      int64
		has      bool
		status   int
	}{
		{name: "balance and usage do not imply support"},
		{name: "historical and manually completed Epay orders accumulate", payments: []model.TopUp{
			legacy(model.PaymentProviderEpay, "alipay", 3), legacy(model.PaymentProviderEpay, "wxpay", 7),
		}, fen: 1000, has: true},
		{name: "old providerless Chinese payment methods", payments: []model.TopUp{
			legacy("", "alipay", 2), legacy("", "wxpay", 3), legacy("", "unknown", 999),
		}, fen: 500, has: true},
		{name: "verified principal less refunds ignores order money", payments: []model.TopUp{verified("CNY", "partial-refund", 2500, 500)}, fen: 2000, has: true},
		{name: "fully refunded payment", payments: []model.TopUp{verified("CNY", "full-refund", 100, 100)}},
		{name: "orphaned verification fields", payments: []model.TopUp{verified("CNY", "missing-receipt", 100, 0)}, orphan: true},
		{name: "duplicate provider reference", payments: []model.TopUp{verified("CNY", "same", 100, 0), verified("CNY", "same", 100, 0)}, fen: 100, has: true},
		{name: "foreign currencies and nonpayment rewards", payments: []model.TopUp{
			verified("USD", "usd", 100, 0), noPositive, verified("CNY", "", 500, 0),
			legacy("community_lottery", "community_lottery", 999), legacy(model.PaymentProviderBalance, "balance", 999),
			legacy(model.PaymentProviderStripe, "stripe", 999),
			{PaymentProvider: model.PaymentProviderEpay, PaymentCurrency: "USD", Money: 999},
			{PaymentProvider: model.PaymentProviderEpay, PaymentCurrency: "CNY", Money: 999},
			{PaymentProvider: model.PaymentProviderEpay, Money: 999, Status: common.TopUpStatusPending},
		}},
		{name: "administrator add and positive override", logs: []model.Log{
			adminLog("add", "user.quota_add", "add", 0, 15000),
			adminLog("override", "user.quota_override", "override", 500, 35500),
		}, fen: 10, has: true},
		{name: "administrator fractions accumulate before rounding", logs: []model.Log{
			adminLog("first", "user.quota_add", "add", 0, 2000),
			adminLog("second", "user.quota_override", "override", 1000, 4000),
		}, fen: 1, has: true},
		{name: "positive adjustment below one fen", logs: []model.Log{adminLog("tiny", "user.quota_add", "add", 0, 1)}, has: true},
		{name: "administrator request is counted once", logs: []model.Log{
			adminLog("same-request", "user.quota_add", "add", 0, 5000),
			adminLog("same-request", "user.quota_add", "add", 0, 5000),
		}, fen: 1, has: true},
		{name: "failed unrelated negative and malformed adjustments", logs: []model.Log{
			failedLog, wrongRoleLog,
			adminLog("subtract", "user.quota_subtract", "subtract", 0, 500000),
			adminLog("lower", "user.quota_override", "override", 10000, 5000),
			adminLog("unchanged", "user.quota_override", "override", 5000, 5000),
			adminLog("mismatch", "user.quota_add", "override", 0, 500000),
			adminLog("decimal", "user.quota_add", "add", 0, 1.5),
			adminLog("missing", "user.quota_add", "add", nil, 500000),
			adminLog("overflow", "user.quota_add", "add", 0, "9223372036854775808"),
		}},
		{name: "maximum safe support", payments: []model.TopUp{verified("CNY", "safe", common.MaxWalletQuota, 0)}, fen: common.MaxWalletQuota, has: true},
		{name: "sum overflow fails closed", payments: []model.TopUp{verified("CNY", "large", common.MaxWalletQuota, 0), verified("CNY", "one", 1, 0)}, status: http.StatusServiceUnavailable},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, db.Where("user_id = ?", user.Id).Delete(&model.TopUp{}).Error)
			require.NoError(t, db.Where("user_id = ?", user.Id).Delete(&model.Log{}).Error)
			for paymentIndex, payment := range test.payments {
				payment.UserId = user.Id
				payment.TradeNo = "support-" + strconv.Itoa(index) + "-" + strconv.Itoa(paymentIndex)
				payment.CreateTime, payment.CompleteTime = 1, 1
				if payment.Status == "" {
					payment.Status = common.TopUpStatusSuccess
				}
				require.NoError(t, db.Create(&payment).Error)
				if payment.PaymentVerified && !test.orphan {
					require.NoError(t, db.Create(&model.UnifiedWalletPaymentReceipt{ID: payment.TradeNo, TopUpID: payment.Id}).Error)
				}
			}
			if len(test.logs) > 0 {
				require.NoError(t, db.Create(&test.logs).Error)
			}
			response := call(query, secret)
			if test.status != 0 {
				assert.Equal(t, test.status, response.Code, response.Body.String())
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result map[string]any
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.Equal(t, "support", result["basis"])
			assert.Equal(t, float64(test.fen), result["support_cny_fen"])
			assert.Equal(t, test.has, result["has_support"])
			assert.Equal(t, []any{"balance", "charity"}, result["reward_kinds"])
			assert.Equal(t, float64(100), result["quota_per_point"])
			assert.NotContains(t, result, "consumption_cny_fen")
			assert.NotContains(t, result, "has_consumption")
		})
	}
	t.Run("other users and unavailable log storage", func(t *testing.T) {
		require.NoError(t, db.Where("user_id = ?", user.Id).Delete(&model.TopUp{}).Error)
		require.NoError(t, db.Where("user_id = ?", user.Id).Delete(&model.Log{}).Error)
		payment := legacy(model.PaymentProviderEpay, "alipay", 999)
		payment.UserId, payment.Status, payment.TradeNo = other.Id, common.TopUpStatusSuccess, "other-user-order"
		require.NoError(t, db.Create(&payment).Error)
		log := adminLog("wrong-target", "user.quota_add", "add", 0, 500000)
		log.UserId = other.Id
		require.NoError(t, db.Create(&log).Error)
		response := call(query, secret)
		require.Equal(t, http.StatusOK, response.Code)
		assert.Contains(t, response.Body.String(), `"has_support":false`)
		model.LOG_DB = nil
		assert.Equal(t, http.StatusServiceUnavailable, call(query, secret).Code)
		model.LOG_DB = db
		require.NoError(t, db.Model(&user).Update("status", common.UserStatusDisabled).Error)
		assert.Equal(t, http.StatusBadRequest, call(query, secret).Code)
	})
}

func TestCommunityReferralServiceAuthorizationAndConfig(t *testing.T) {
	user, _ := setupCommunitySSOTest(t)
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	secret := strings.Repeat("referral-service-test", 3)
	t.Setenv("COMMUNITY_LOTTERY_SECRET", secret)
	oldInviter, oldInvitee, oldMinimum := common.QuotaForInviter, common.QuotaForInvitee, common.ReferralMinTopUpCNY
	payment := operation_setting.GetPaymentSetting()
	oldPayment := *payment
	common.QuotaForInviter, common.QuotaForInvitee, common.ReferralMinTopUpCNY = 500000, 250000, "10.29"
	payment.ComplianceConfirmed, payment.ComplianceTermsVersion = true, operation_setting.CurrentComplianceTermsVersion
	t.Cleanup(func() {
		common.QuotaForInviter, common.QuotaForInvitee, common.ReferralMinTopUpCNY = oldInviter, oldInvitee, oldMinimum
		*payment = oldPayment
	})
	require.NoError(t, model.DB.AutoMigrate(&model.CommunityReferralConfig{}, &model.CommunityReferralCode{}, &model.CommunityReferral{}))
	call := func(credential string, body map[string]any) *httptest.ResponseRecorder {
		data, err := common.Marshal(body)
		require.NoError(t, err)
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest("POST", "/api/community-sso/referrals", strings.NewReader(string(data)))
		ctx.Request.Header.Set("Authorization", "Bearer "+credential)
		ctx.Request.Header.Set("Content-Type", "application/json")
		CommunityReferral(ctx)
		return response
	}
	body := map[string]any{"client_id": communityTestOrigin, "action": "config"}
	assert.Equal(t, http.StatusUnauthorized, call("wrong", body).Code)
	body["client_id"] = "https://untrusted.test"
	assert.Equal(t, http.StatusBadRequest, call(secret, body).Code)
	body["client_id"] = communityTestOrigin
	response := call(secret, body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var config model.UnifiedReferralConfig
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &config))
	assert.Equal(t, float64(5000), config.InviterPoints)
	assert.Equal(t, float64(2500), config.InviteePoints)
	assert.Equal(t, 10.29, config.MinTopUpCNY)
	body = map[string]any{"client_id": communityTestOrigin, "action": "configure", "version": 1, "enabled": true, "inviterPoints": 6000, "inviteePoints": 7000, "user_id": user.Id}
	assert.Equal(t, http.StatusBadRequest, call(secret, body).Code, "the community cannot edit centralized reward settings")
	assert.Equal(t, 500000, common.QuotaForInviter)
	response = call(secret, map[string]any{"client_id": communityTestOrigin, "action": "status", "user_id": 0})
	require.Equal(t, http.StatusOK, response.Code)
	assert.NotContains(t, response.Body.String(), secret)
	var public model.CommunityReferralOverview
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &public))
	assert.Empty(t, public.Code)
	assert.Empty(t, public.Items)
	assert.Nil(t, public.AsInvitee)
	self := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(self)
	ctx.Set("id", user.Id)
	ctx.Request = httptest.NewRequest("GET", "/api/user/self/referrals?user_id=999", nil)
	GetSelfReferrals(ctx)
	require.Equal(t, http.StatusOK, self.Code)
	var envelope struct {
		Success bool                            `json:"success"`
		Data    model.CommunityReferralOverview `json:"data"`
	}
	require.NoError(t, common.Unmarshal(self.Body.Bytes(), &envelope))
	assert.True(t, envelope.Success)
	assert.Equal(t, config, *envelope.Data.Config)
	assert.Empty(t, envelope.Data.Items, "the overview uses only the authenticated user")
	for _, value := range []string{"-1", "1.5", "invalid", "9999999999999999999999"} {
		self = httptest.NewRecorder()
		ctx, _ = gin.CreateTestContext(self)
		ctx.Set("id", user.Id)
		ctx.Request = httptest.NewRequest("GET", "/api/user/self/referrals?cursor="+value, nil)
		GetSelfReferrals(ctx)
		var invalid struct {
			Success bool `json:"success"`
		}
		require.NoError(t, common.Unmarshal(self.Body.Bytes(), &invalid))
		assert.False(t, invalid.Success)
	}
}

func TestCommunityLotteryCredit(t *testing.T) {
	user, _ := setupCommunitySSOTest(t)
	t.Setenv("UNIFIED_WALLET_ENABLED", "false")
	previousRate := common.QuotaPerUnit
	t.Cleanup(func() { common.QuotaPerUnit = previousRate })
	common.QuotaPerUnit = 500000
	require.NoError(t, model.DB.AutoMigrate(&model.TopUp{}))
	require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	secret := strings.Repeat("lottery-test-only", 3)
	t.Setenv("COMMUNITY_LOTTERY_SECRET", secret)
	call := func(method, credential string, body map[string]any) *httptest.ResponseRecorder {
		data, err := common.Marshal(body)
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(method, "/api/community-sso/lottery", strings.NewReader(string(data)))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("Authorization", "Bearer "+credential)
		CommunityLotteryCredit(c)
		return recorder
	}
	body := map[string]any{"client_id": communityTestOrigin, "user_id": user.Id, "reference": "community_lottery_fixture_0", "amount_cents": 123}
	assert.Equal(t, http.StatusUnauthorized, call("POST", "invalid", body).Code)
	assert.Equal(t, http.StatusOK, call("GET", secret, nil).Code)
	body["client_id"] = "https://wrong.example"
	assert.Equal(t, http.StatusBadRequest, call("POST", secret, body).Code)
	body["client_id"] = communityTestOrigin
	for _, invalid := range []any{-1, 0, 1000001, 1.5} {
		body["amount_cents"] = invalid
		assert.Equal(t, http.StatusBadRequest, call("POST", secret, body).Code)
	}
	body["amount_cents"] = 123
	for i := 0; i < 2; i++ {
		response := call("POST", secret, body)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result struct {
			Credited bool
			Replayed bool
			Receipt  struct {
				ID          int    `json:"id"`
				QuotaAdded  int    `json:"quota_added"`
				QuotaPerUSD string `json:"quota_per_usd"`
			}
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		assert.True(t, result.Credited)
		assert.Equal(t, i == 1, result.Replayed)
		assert.Positive(t, result.Receipt.ID)
		assert.Equal(t, 615000, result.Receipt.QuotaAdded)
		assert.Equal(t, "500000", result.Receipt.QuotaPerUSD)
		// A lost response must replay the original conversion even after a configuration change.
		common.QuotaPerUnit = 1000000
	}
	var stored model.User
	require.NoError(t, model.DB.First(&stored, user.Id).Error)
	assert.Equal(t, 615000, stored.Quota)
	var receipt model.TopUp
	require.NoError(t, model.DB.Where("trade_no = ?", body["reference"]).First(&receipt).Error)
	assert.EqualValues(t, 123, receipt.Amount)
	assert.Zero(t, receipt.Money)
	require.NotNil(t, receipt.LotteryCredit)
	assert.Equal(t, 615000, receipt.LotteryCredit.QuotaAdded)
	assert.Equal(t, "500000", receipt.LotteryCredit.QuotaPerUSD)
	body["amount_cents"] = 124
	assert.Equal(t, http.StatusConflict, call("POST", secret, body).Code)

	other := model.User{Username: "lottery-other", Password: "unused", Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "lottery-other"}
	require.NoError(t, model.DB.Create(&other).Error)
	type outcome struct {
		replayed bool
		err      error
	}
	for _, differentRecipients := range []bool{false, true} {
		ids := []int{user.Id, user.Id}
		reference := "community_lottery_same_0"
		if differentRecipients {
			ids[1] = other.Id
			reference = "community_lottery_conflict_0"
		}
		results := make(chan outcome, 2)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for _, id := range ids {
			workers.Go(func() {
				<-start
				_, replayed, err := model.CreditCommunityLottery(id, reference, 50)
				results <- outcome{replayed, err}
			})
		}
		close(start)
		workers.Wait()
		close(results)
		created, replayed, conflicts := 0, 0, 0
		for result := range results {
			if result.err != nil {
				require.ErrorIs(t, result.err, model.ErrLotteryCreditConflict)
				conflicts++
			} else if result.replayed {
				replayed++
			} else {
				created++
			}
		}
		assert.Equal(t, 1, created)
		if differentRecipients {
			assert.Equal(t, 1, conflicts)
		} else {
			assert.Equal(t, 1, replayed)
		}
		var count int64
		require.NoError(t, model.DB.Model(&model.TopUp{}).Where("trade_no = ?", reference).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	}
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("quota", common.MaxWalletQuota).Error)
	body["amount_cents"], body["reference"] = 50, "community_lottery_ceiling_0"
	assert.Equal(t, http.StatusBadRequest, call("POST", secret, body).Code)
	var count int64
	require.NoError(t, model.DB.Model(&model.TopUp{}).Where("trade_no = ?", body["reference"]).Count(&count).Error)
	assert.Zero(t, count, "failed credit rolls back its receipt")
	require.NoError(t, model.DB.First(&stored, user.Id).Error)
	assert.Equal(t, common.MaxWalletQuota, stored.Quota)

	for _, rate := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		common.QuotaPerUnit = rate
		body["reference"] = "community_lottery_invalidrate_0"
		assert.Equal(t, http.StatusBadRequest, call("POST", secret, body).Code)
	}
	// Existing historical receipts have no trustworthy conversion snapshot.
	legacy := model.TopUp{UserId: user.Id, Amount: 1, TradeNo: "community_lottery_legacy_0", PaymentProvider: "community_lottery", Status: common.TopUpStatusSuccess}
	require.NoError(t, model.DB.Create(&legacy).Error)
	body["reference"], body["amount_cents"] = legacy.TradeNo, 1
	response := call("POST", secret, body)
	require.Equal(t, http.StatusOK, response.Code)
	var legacyResult map[string]any
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &legacyResult))
	assert.True(t, legacyResult["replayed"].(bool))
	assert.Nil(t, legacyResult["receipt"])
}

func TestCommunityLotteryReceiptUpgrade(t *testing.T) {
	user, _ := setupCommunitySSOTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.TopUp{}))
	require.NoError(t, model.DB.Migrator().DropColumn(&model.TopUp{}, "LotteryCredit"))
	legacy := model.TopUp{UserId: user.Id, Amount: 1, TradeNo: "community_lottery_upgrade_0", PaymentProvider: "community_lottery", Status: common.TopUpStatusSuccess}
	require.NoError(t, model.DB.Omit("LotteryCredit").Create(&legacy).Error)
	for range 2 {
		require.NoError(t, model.DB.AutoMigrate(&model.TopUp{}))
	}
	var restored model.TopUp
	require.NoError(t, model.DB.First(&restored, legacy.Id).Error)
	assert.Equal(t, legacy.TradeNo, restored.TradeNo)
	assert.Equal(t, legacy.Amount, restored.Amount)
	assert.Nil(t, restored.LotteryCredit)
	receipt, replayed, err := model.CreditCommunityLottery(user.Id, legacy.TradeNo, 1)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Nil(t, receipt.LotteryCredit)
	require.NoError(t, model.DB.First(user, user.Id).Error)
	assert.Zero(t, user.Quota, "migration and replay never invent historical credits")
	duplicate := legacy
	duplicate.Id = 0
	assert.Error(t, model.DB.Create(&duplicate).Error, "upgrade preserves unique receipt numbers")
}

const communityLotteryCharitySecret = "community-lottery-charity-test-service-secret"

func TestCommunityLotteryExactPoints(t *testing.T) {
	user := setupCommunityLotteryWalletTest(t)
	body := map[string]any{"client_id": communityTestOrigin, "user_id": user.Id, "reference": "community_lottery_checkin_0", "amount_cents": 0, "points": 617, "reward_kind": "charity", "validity_hours": 24}
	response := communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Replayed bool                                 `json:"replayed"`
		Receipt  model.CommunityLotteryCreditSnapshot `json:"receipt"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	assert.Equal(t, "617", result.Receipt.Points)
	assert.EqualValues(t, 617, result.Receipt.RequestedPoints)
	assert.Equal(t, 61700, result.Receipt.QuotaAdded)
	assert.EqualValues(t, 86400, result.Receipt.ExpiresAt-result.Receipt.CreditedAt)
	var receipt model.TopUp
	require.NoError(t, model.DB.Where("trade_no = ?", body["reference"]).First(&receipt).Error)
	assert.Zero(t, receipt.Amount)
	assert.Zero(t, receipt.Money)
	// A changed conversion rate and an expired lot cannot change an already granted reward.
	common.QuotaPerUnit = 1000000
	require.NoError(t, model.DB.Model(&model.UnifiedWalletLot{}).Where("id = ?", result.Receipt.LotID).Update("expires_at", 1).Error)
	replay := communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	var again struct {
		Replayed bool                                 `json:"replayed"`
		Receipt  model.CommunityLotteryCreditSnapshot `json:"receipt"`
	}
	require.NoError(t, common.Unmarshal(replay.Body.Bytes(), &again))
	assert.True(t, again.Replayed)
	assert.Equal(t, result.Receipt, again.Receipt)
	body["points"] = 618
	assert.Equal(t, http.StatusConflict, communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body).Code)
	body["points"] = 617
	body["validity_hours"] = 25
	assert.Equal(t, http.StatusConflict, communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body).Code)
	body["validity_hours"] = 24
	for _, value := range []any{0, -1, 1.5, "617", 1000000001} {
		body["points"] = value
		assert.Equal(t, http.StatusBadRequest, communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body).Code)
	}
	body["points"], body["amount_cents"] = 617, 1
	assert.Equal(t, http.StatusBadRequest, communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body).Code)
	delete(body, "points")
	assert.Equal(t, http.StatusConflict, communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body).Code)
	var lots, grants int64
	require.NoError(t, model.DB.Model(&model.UnifiedWalletLot{}).Count(&lots).Error)
	require.NoError(t, model.DB.Model(&model.UnifiedWalletEntry{}).Where("event = ?", "grant").Count(&grants).Error)
	assert.EqualValues(t, 1, lots)
	assert.EqualValues(t, 1, grants)
}

func setupCommunityLotteryWalletTest(t *testing.T) *model.User {
	t.Helper()
	db, dialect := openTaskDialectDatabase(t, &model.User{}, &model.TopUp{}, &model.UnifiedWalletAccount{}, &model.UnifiedWalletLot{}, &model.UnifiedWalletEntry{}, &model.UnifiedWalletOperation{}, &model.UnifiedWalletPaymentReceipt{}, &model.Log{})
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
	oldRate, oldRedis, oldMemory := common.QuotaPerUnit, common.RedisEnabled, common.MemoryCacheEnabled
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(dialect, dialect)
	common.QuotaPerUnit, common.RedisEnabled, common.MemoryCacheEnabled = 500000, false, false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMain, oldLog)
		common.QuotaPerUnit, common.RedisEnabled, common.MemoryCacheEnabled = oldRate, oldRedis, oldMemory
	})
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	t.Setenv("COMMUNITY_LOTTERY_SECRET", communityLotteryCharitySecret)
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	t.Setenv("QXCHAT_SHARED_WALLET_ENABLED", "0")
	user := &model.User{Username: "lottery-charity", AffCode: "lottery-charity", Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(user).Error)
	require.NoError(t, db.Create(&model.UnifiedWalletAccount{UserID: user.Id}).Error)
	return user
}

func communityLotteryRewardCall(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := common.Marshal(body)
	require.NoError(t, err)
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("Authorization", "Bearer "+communityLotteryCharitySecret)
	CommunityLotteryCredit(ctx)
	return response
}

func TestCommunityLotteryCharityCredit(t *testing.T) {
	user := setupCommunityLotteryWalletTest(t)
	require.NoError(t, model.DB.Create(&model.TopUp{TradeNo: "lottery-receipt-sequence-fixture"}).Error)
	body := map[string]any{"client_id": communityTestOrigin, "user_id": user.Id, "reference": "community_lottery_charity_0", "amount_cents": 123, "reward_kind": "charity", "validity_hours": 168}
	response := communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	type lotteryResult struct {
		Credited bool `json:"credited"`
		Replayed bool `json:"replayed"`
		Receipt  struct {
			ID int `json:"id"`
			model.CommunityLotteryCreditSnapshot
		} `json:"receipt"`
	}
	var result lotteryResult
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	assert.True(t, result.Credited)
	assert.False(t, result.Replayed)
	assert.Equal(t, "charity", result.Receipt.RewardKind)
	assert.Equal(t, 168, result.Receipt.ValidityHours)
	assert.Equal(t, 615000, result.Receipt.QuotaAdded)
	assert.Equal(t, "500000", result.Receipt.QuotaPerUSD)
	assert.Equal(t, "6150", result.Receipt.Points)
	assert.EqualValues(t, 100, result.Receipt.QuotaPerPoint)
	assert.Positive(t, result.Receipt.ID)
	assert.Positive(t, result.Receipt.LotID)
	assert.LessOrEqual(t, result.Receipt.LotID, int64(common.MaxWalletQuota))
	assert.NotEqualValues(t, result.Receipt.ID, result.Receipt.LotID, "receipt and lot are separate identities")
	assert.InDelta(t, time.Now().Unix(), result.Receipt.CreditedAt, 5)
	assert.EqualValues(t, 168*3600, result.Receipt.ExpiresAt-result.Receipt.CreditedAt)
	var lot model.UnifiedWalletLot
	require.NoError(t, model.DB.First(&lot, result.Receipt.LotID).Error)
	assert.Equal(t, user.Id, lot.UserID)
	assert.Equal(t, model.WalletCharity, lot.Kind)
	assert.Equal(t, "community_lottery", lot.Source)
	assert.Equal(t, model.WalletReference("community_lottery", body["reference"].(string)), lot.Reference)
	assert.EqualValues(t, 615000, lot.InitialQuota)
	assert.EqualValues(t, 615000, lot.RemainingQuota)
	assert.EqualValues(t, 6150, lot.InitialPoints)
	assert.Equal(t, result.Receipt.CreditedAt, lot.CreatedAt)
	assert.Equal(t, result.Receipt.ExpiresAt, lot.ExpiresAt)
	var receipt model.TopUp
	require.NoError(t, model.DB.First(&receipt, result.Receipt.ID).Error)
	assert.Zero(t, receipt.Money)
	require.NotNil(t, receipt.LotteryCredit)
	assert.Equal(t, result.Receipt.CommunityLotteryCreditSnapshot, *receipt.LotteryCredit)
	var grant model.UnifiedWalletEntry
	require.NoError(t, model.DB.Where("lot_id = ? AND event = ?", lot.ID, "grant").First(&grant).Error)
	assert.EqualValues(t, 615000, grant.QuotaDelta)
	assert.Equal(t, lot.Reference, grant.OperationID)
	var account model.UnifiedWalletAccount
	require.NoError(t, model.DB.First(&account, "user_id = ?", user.Id).Error)
	assert.Zero(t, account.LegacyQuota)
	assert.Zero(t, account.PaidQuota)
	require.NoError(t, model.DB.First(user, user.Id).Error)
	assert.Zero(t, user.Quota)
	var payments int64
	require.NoError(t, model.DB.Model(&model.UnifiedWalletPaymentReceipt{}).Count(&payments).Error)
	assert.Zero(t, payments, "a lottery reward is never a verified payment")
	wallet, err := model.GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "6150", wallet.CharityPoints)
	assert.Equal(t, "0", wallet.PermanentPoints)
	assert.Zero(t, wallet.TotalAPIQuota)
	assert.ErrorIs(t, model.BeginWalletOperation(user.Id, model.WalletReference("lottery-api-isolation"), model.WalletAPI, 1, 0), model.ErrWalletInsufficient)

	common.QuotaPerUnit = math.NaN()
	t.Setenv("UNIFIED_WALLET_ENABLED", "false")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "invalid")
	response = communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var replay lotteryResult
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &replay))
	assert.True(t, replay.Replayed)
	assert.Equal(t, result.Receipt, replay.Receipt)

	for _, change := range []struct {
		key   string
		value any
	}{{"user_id", user.Id + 1}, {"amount_cents", 124}, {"validity_hours", 169}} {
		if change.key == "user_id" {
			require.NoError(t, model.DB.Create(&model.User{Id: user.Id + 1, Username: "lottery-other", AffCode: "lottery-other", Status: common.UserStatusEnabled}).Error)
		}
		original := body[change.key]
		body[change.key] = change.value
		assert.Equal(t, http.StatusConflict, communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body).Code, change.key)
		body[change.key] = original
	}
	delete(body, "reward_kind")
	delete(body, "validity_hours")
	assert.Equal(t, http.StatusConflict, communityLotteryRewardCall(t, "/api/community-sso/lottery", body).Code, "charity cannot replay as permanent")
	body["reward_kind"], body["validity_hours"], body["reference"] = "charity", 24, "community_lottery_charity_1"
	common.QuotaPerUnit = 500000
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	response = communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var second lotteryResult
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &second))
	assert.NotEqual(t, result.Receipt.LotID, second.Receipt.LotID)
	assert.EqualValues(t, 24*3600, second.Receipt.ExpiresAt-second.Receipt.CreditedAt)
	var count int64
	require.NoError(t, model.DB.Model(&model.UnifiedWalletLot{}).Count(&count).Error)
	assert.EqualValues(t, 2, count)
}

func TestCommunityLotteryCharityValidation(t *testing.T) {
	user := setupCommunityLotteryWalletTest(t)
	for _, test := range []struct {
		name   string
		fields map[string]any
	}{
		{"missing both", nil},
		{"missing hours", map[string]any{"reward_kind": "charity"}},
		{"missing kind", map[string]any{"validity_hours": 24}},
		{"null kind", map[string]any{"reward_kind": nil, "validity_hours": 24}},
		{"null hours", map[string]any{"reward_kind": "charity", "validity_hours": nil}},
		{"both null", map[string]any{"reward_kind": nil, "validity_hours": nil}},
		{"permanent kind", map[string]any{"reward_kind": "balance", "validity_hours": 24}},
		{"object kind", map[string]any{"reward_kind": map[string]any{}, "validity_hours": 24}},
		{"array hours", map[string]any{"reward_kind": "charity", "validity_hours": []int{24}}},
		{"text hours", map[string]any{"reward_kind": "charity", "validity_hours": "24"}},
		{"bool hours", map[string]any{"reward_kind": "charity", "validity_hours": true}},
		{"fractional hours", map[string]any{"reward_kind": "charity", "validity_hours": 1.5}},
		{"zero hours", map[string]any{"reward_kind": "charity", "validity_hours": 0}},
		{"negative hours", map[string]any{"reward_kind": "charity", "validity_hours": -1}},
		{"excess hours", map[string]any{"reward_kind": "charity", "validity_hours": 8761}},
		{"overflow hours", map[string]any{"reward_kind": "charity", "validity_hours": uint64(math.MaxUint64)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{"client_id": communityTestOrigin, "user_id": user.Id, "reference": "community_lottery_invalid_0", "amount_cents": 1}
			for key, value := range test.fields {
				body[key] = value
			}
			assert.Equal(t, http.StatusBadRequest, communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body).Code)
			if test.fields != nil {
				assert.Equal(t, http.StatusBadRequest, communityLotteryRewardCall(t, "/api/community-sso/lottery", body).Code)
			}
		})
	}
	var receipts, lots, entries int64
	require.NoError(t, model.DB.Model(&model.TopUp{}).Count(&receipts).Error)
	require.NoError(t, model.DB.Model(&model.UnifiedWalletLot{}).Count(&lots).Error)
	require.NoError(t, model.DB.Model(&model.UnifiedWalletEntry{}).Count(&entries).Error)
	assert.Zero(t, receipts)
	assert.Zero(t, lots)
	assert.Zero(t, entries)
	for index, hours := range []int{1, 8760} {
		body := map[string]any{"client_id": communityTestOrigin, "user_id": user.Id, "reference": "community_lottery_bound" + strconv.Itoa(index) + "_0", "amount_cents": 1, "reward_kind": "charity", "validity_hours": hours}
		assert.Equal(t, http.StatusOK, communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body).Code)
	}
}

func TestCommunityLotteryCharityConversionAndRollback(t *testing.T) {
	user := setupCommunityLotteryWalletTest(t)
	body := map[string]any{"client_id": communityTestOrigin, "user_id": user.Id, "reference": "community_lottery_conversion_0", "amount_cents": 1, "reward_kind": "charity", "validity_hours": 24}
	for _, test := range []struct {
		name    string
		usdRate float64
		cnyRate string
		enabled string
		status  int
	}{
		{"disabled wallet", 500000, "500000", "false", http.StatusServiceUnavailable},
		{"missing point rate", 500000, "", "true", http.StatusServiceUnavailable},
		{"fractional point rate", 500000, "500001", "true", http.StatusServiceUnavailable},
		{"fractional raw quota", 100.5, "5000", "true", http.StatusBadRequest},
		{"fractional point", 500100, "500000", "true", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			common.QuotaPerUnit = test.usdRate
			t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", test.cnyRate)
			t.Setenv("UNIFIED_WALLET_ENABLED", test.enabled)
			response := communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
			assert.Equal(t, test.status, response.Code, response.Body.String())
		})
	}
	common.QuotaPerUnit = 500000
	oversized := model.UnifiedWalletLot{UserID: user.Id, Kind: model.WalletCharity, Reference: model.WalletReference("lottery-limit-fixture"), Source: "fixture", InitialQuota: int64(common.MaxWalletQuota), RemainingQuota: int64(common.MaxWalletQuota), InitialPoints: 1, ExpiresAt: time.Now().Unix() + 3600}
	require.NoError(t, model.DB.Create(&oversized).Error)
	response := communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.NoError(t, model.DB.Delete(&oversized).Error)
	// A receipt insert failure must also roll back the earlier lot and grant entry.
	callback := "test:reject-community-lottery-receipt"
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*model.TopUp); ok {
			tx.AddError(model.ErrWalletUnavailable)
		}
	}))
	response = communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
	require.NoError(t, model.DB.Callback().Create().Remove(callback))
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	var receipts, lots, entries int64
	require.NoError(t, model.DB.Model(&model.TopUp{}).Count(&receipts).Error)
	require.NoError(t, model.DB.Model(&model.UnifiedWalletLot{}).Count(&lots).Error)
	require.NoError(t, model.DB.Model(&model.UnifiedWalletEntry{}).Count(&entries).Error)
	assert.Zero(t, receipts)
	assert.Zero(t, lots)
	assert.Zero(t, entries)
	response = communityLotteryRewardCall(t, "/api/community-sso/lottery/charity", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestCommunityLotteryCharityRefundExpiryAndLegacy(t *testing.T) {
	user := setupCommunityLotteryWalletTest(t)
	receipt, replayed, err := model.CreditCommunityLottery(user.Id, "community_lottery_refund_0", 1, model.CommunityLotteryReward{Kind: model.WalletCharity, ValidityHours: 24})
	require.NoError(t, err)
	assert.False(t, replayed)
	require.NotNil(t, receipt.LotteryCredit)
	lotID, expiry := receipt.LotteryCredit.LotID, receipt.LotteryCredit.ExpiresAt
	operation := model.WalletReference("lottery-refund-operation")
	require.NoError(t, model.BeginWalletOperation(user.Id, operation, model.WalletCommunity, 4000, 0))
	require.NoError(t, model.ChangeWalletOperation(user.Id, operation, 1000, "reserved", true))
	var lot model.UnifiedWalletLot
	require.NoError(t, model.DB.First(&lot, lotID).Error)
	assert.EqualValues(t, 4000, lot.RemainingQuota)
	assert.Equal(t, expiry, lot.ExpiresAt)
	require.NoError(t, model.ChangeWalletOperation(user.Id, operation, 0, "refunded", true))
	require.NoError(t, model.ChangeWalletOperation(user.Id, operation, 0, "refunded", true))
	require.NoError(t, model.DB.First(&lot, lotID).Error)
	assert.EqualValues(t, 5000, lot.RemainingQuota)
	assert.Equal(t, expiry, lot.ExpiresAt)
	expiringOperation := model.WalletReference("lottery-expired-refund-operation")
	require.NoError(t, model.BeginWalletOperation(user.Id, expiringOperation, model.WalletCommunity, 5000, 0))
	past := time.Now().Unix() - 1
	require.NoError(t, model.DB.Model(&lot).Update("expires_at", past).Error)
	require.NoError(t, model.ChangeWalletOperation(user.Id, expiringOperation, 0, "refunded", true))
	require.NoError(t, model.DB.First(&lot, lotID).Error)
	assert.EqualValues(t, 5000, lot.RemainingQuota)
	assert.Equal(t, past, lot.ExpiresAt)
	wallet, err := model.GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "0", wallet.CharityPoints)
	assert.Zero(t, wallet.TotalAPIQuota)
	assert.ErrorIs(t, model.BeginWalletOperation(user.Id, model.WalletReference("expired-charity-reuse"), model.WalletCommunity, 1, 0), model.ErrWalletInsufficient)
	assert.ErrorIs(t, model.BeginWalletOperation(user.Id, model.WalletReference("expired-charity-api"), model.WalletAPI, 1, 0), model.ErrWalletInsufficient)

	permanent, replayed, err := model.CreditCommunityLottery(user.Id, "community_lottery_pending_0", 1)
	require.NoError(t, err)
	assert.False(t, replayed)
	require.NotNil(t, permanent.LotteryCredit)
	assert.Empty(t, permanent.LotteryCredit.RewardKind)
	require.NoError(t, model.DB.First(user, user.Id).Error)
	assert.Equal(t, 5000, user.Quota, "old pending requests keep their permanent balance behavior")
	var account model.UnifiedWalletAccount
	require.NoError(t, model.DB.First(&account, "user_id = ?", user.Id).Error)
	assert.EqualValues(t, 5000, account.LegacyQuota)
	assert.Zero(t, account.PaidQuota)
	_, _, err = model.CreditCommunityLottery(user.Id, permanent.TradeNo, 1, model.CommunityLotteryReward{Kind: model.WalletCharity, ValidityHours: 24})
	assert.ErrorIs(t, err, model.ErrLotteryCreditConflict)
	var paid, payments int64
	require.NoError(t, model.DB.Model(&model.UnifiedWalletLot{}).Where("kind = ?", model.WalletPaid).Count(&paid).Error)
	require.NoError(t, model.DB.Model(&model.UnifiedWalletPaymentReceipt{}).Count(&payments).Error)
	assert.Zero(t, paid)
	assert.Zero(t, payments)
}

func TestCommunityLotteryCharityConcurrency(t *testing.T) {
	user := setupCommunityLotteryWalletTest(t)
	if common.MainDatabaseType() != common.DatabaseTypeSQLite {
		connections, err := model.DB.DB()
		require.NoError(t, err)
		connections.SetMaxOpenConns(4)
	}
	other := model.User{Username: "charity-race", AffCode: "charity-race", Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&other).Error)
	for _, differentRecipients := range []bool{false, true} {
		reference := "community_lottery_charitysame_0"
		ids := []int{user.Id, user.Id}
		if differentRecipients {
			reference, ids[1] = "community_lottery_charityrace_0", other.Id
		}
		type outcome struct {
			replayed bool
			err      error
		}
		results := make(chan outcome, 2)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for _, id := range ids {
			workers.Go(func() {
				<-start
				_, replayed, err := model.CreditCommunityLottery(id, reference, 1, model.CommunityLotteryReward{Kind: model.WalletCharity, ValidityHours: 24})
				results <- outcome{replayed, err}
			})
		}
		close(start)
		workers.Wait()
		close(results)
		created, replayed, conflicts := 0, 0, 0
		for result := range results {
			switch {
			case result.err != nil:
				require.ErrorIs(t, result.err, model.ErrLotteryCreditConflict)
				conflicts++
			case result.replayed:
				replayed++
			default:
				created++
			}
		}
		assert.Equal(t, 1, created)
		if differentRecipients {
			assert.Equal(t, 1, conflicts)
		} else {
			assert.Equal(t, 1, replayed)
		}
	}
	var receipts, lots, grants int64
	require.NoError(t, model.DB.Model(&model.TopUp{}).Count(&receipts).Error)
	require.NoError(t, model.DB.Model(&model.UnifiedWalletLot{}).Count(&lots).Error)
	require.NoError(t, model.DB.Model(&model.UnifiedWalletEntry{}).Where("event = ?", "grant").Count(&grants).Error)
	assert.EqualValues(t, 2, receipts)
	assert.EqualValues(t, 2, lots)
	assert.EqualValues(t, 2, grants)
	require.NoError(t, model.DB.First(user, user.Id).Error)
	require.NoError(t, model.DB.First(&other, other.Id).Error)
	assert.Zero(t, user.Quota)
	assert.Zero(t, other.Quota)
}
