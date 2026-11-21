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

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommunityLotterySupport(t *testing.T) {
	db, dialect := openTaskDialectDatabase(t, &model.User{}, &model.TopUp{}, &model.UnifiedWalletPaymentReceipt{})
	oldDB, oldDialect := model.DB, common.MainDatabaseType()
	model.DB = db
	common.SetDatabaseTypes(dialect, common.LogDatabaseType())
	t.Cleanup(func() {
		model.DB = oldDB
		common.SetDatabaseTypes(oldDialect, common.LogDatabaseType())
	})
	t.Setenv("COMMUNITY_SSO_ORIGIN", communityTestOrigin)
	secret := strings.Repeat("lottery-support-test", 3)
	t.Setenv("COMMUNITY_LOTTERY_SECRET", secret)
	user := model.User{Username: "support-user", AffCode: "support-user", Status: common.UserStatusEnabled}
	other := model.User{Username: "support-other", AffCode: "support-other", Status: common.UserStatusEnabled}
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
	assert.Equal(t, map[string]any{"ready": true, "client_id": communityTestOrigin}, readiness)
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

	var support struct {
		Ready         bool   `json:"ready"`
		ClientID      string `json:"client_id"`
		UserID        int    `json:"user_id"`
		SupportCNYFen int64  `json:"support_cny_fen"`
		HasSupport    bool   `json:"has_support"`
	}
	response = call(queryFor(user.Id), secret)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &support))
	assert.True(t, support.Ready)
	assert.Equal(t, communityTestOrigin, support.ClientID)
	assert.Equal(t, user.Id, support.UserID)
	assert.Zero(t, support.SupportCNYFen)
	assert.False(t, support.HasSupport)

	var refundable model.TopUp
	for _, test := range []struct {
		name, provider, currency, status, reference string
		principal, refunded                         int64
		unverified, zero, noReceipt, otherUser      bool
	}{
		{name: "cny-one", principal: 100},
		{name: "cny-two", principal: 100},
		{name: "partial", principal: 250, refunded: 75},
		{name: "fully-refunded", principal: 100, refunded: 100},
		{name: "over-refunded", principal: 100, refunded: 101},
		{name: "usd", currency: "USD"},
		{name: "creem", provider: model.PaymentProviderCreem, principal: 1},
		{name: "waffo", provider: model.PaymentProviderWaffo, principal: 1},
		{name: "pancake", provider: model.PaymentProviderWaffoPancake, principal: 1},
		{name: "unverified", principal: 1000, unverified: true},
		{name: "zero", principal: 1000, zero: true},
		{name: "pending", principal: 1000, status: common.TopUpStatusPending},
		{name: "gift", principal: 1000, provider: "gift"},
		{name: "invitation", principal: 1000, provider: "community_referral"},
		{name: "lottery", principal: 1000, provider: "community_lottery"},
		{name: "redemption", principal: 1000, provider: "redemption"},
		{name: "admin", principal: 1000, provider: "admin"},
		{name: "balance", principal: 1000, provider: model.PaymentProviderBalance},
		{name: "empty-reference", principal: 1000, reference: "-"},
		{name: "blank-reference", principal: 1000, reference: " "},
		{name: "no-receipt", principal: 1000, noReceipt: true},
		{name: "other-user", principal: 1000, otherUser: true},
		{name: "duplicate", principal: 100, reference: "support-cny-one", noReceipt: true},
		{name: "other-user-receipt", principal: 1000, reference: "support-other-user", noReceipt: true},
	} {
		payment := model.TopUp{UserId: user.Id, TradeNo: "support-" + test.name, Status: common.TopUpStatusSuccess,
			PaymentProvider: model.PaymentProviderEpay, PaymentCurrency: "CNY", PaymentReference: "support-" + test.name,
			PaymentVerified: !test.unverified, PaymentPositive: !test.zero, PaymentPrincipalFen: test.principal, PaymentRefundedFen: test.refunded}
		if test.provider != "" {
			payment.PaymentProvider = test.provider
		}
		if test.currency != "" {
			payment.PaymentCurrency = test.currency
		}
		if test.status != "" {
			payment.Status = test.status
		}
		if test.reference != "" {
			payment.PaymentReference = test.reference
			if test.reference == "-" {
				payment.PaymentReference = ""
			}
		}
		if test.otherUser {
			payment.UserId = other.Id
		}
		require.NoError(t, db.Create(&payment).Error, test.name)
		if !test.noReceipt {
			receipt := model.UnifiedWalletPaymentReceipt{ID: model.WalletReference("payment-receipt", payment.PaymentProvider, payment.PaymentReference), TopUpID: payment.Id}
			require.NoError(t, db.Create(&receipt).Error, test.name)
		}
		if test.name == "cny-one" {
			refundable = payment
		}
	}
	response = call(queryFor(user.Id), secret)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &support))
	assert.EqualValues(t, 378, support.SupportCNYFen, "separate equal payments count twice; receipts exclude duplicate and other-user payments")
	assert.True(t, support.HasSupport)
	require.NoError(t, db.Model(&refundable).Update("payment_refunded_fen", refundable.PaymentPrincipalFen).Error)
	response = call(queryFor(user.Id), secret)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &support))
	assert.EqualValues(t, 278, support.SupportCNYFen, "later queries use the latest net principal")

	require.NoError(t, db.Where("user_id = ?", user.Id).Delete(&model.TopUp{}).Error)
	usd := model.TopUp{UserId: user.Id, TradeNo: "only-usd", Status: common.TopUpStatusSuccess, PaymentProvider: model.PaymentProviderStripe,
		PaymentCurrency: "USD", PaymentReference: "only-usd", PaymentVerified: true, PaymentPositive: true}
	require.NoError(t, db.Create(&usd).Error)
	require.NoError(t, db.Create(&model.UnifiedWalletPaymentReceipt{ID: model.WalletReference("payment-receipt", usd.PaymentProvider, usd.PaymentReference), TopUpID: usd.Id}).Error)
	response = call(queryFor(user.Id), secret)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &support))
	assert.Zero(t, support.SupportCNYFen)
	assert.True(t, support.HasSupport, "verified foreign-currency support satisfies only the zero threshold")
	for index, principal := range []int64{common.MaxWalletQuota, 1} {
		payment := model.TopUp{UserId: user.Id, TradeNo: "large-" + strconv.Itoa(index), Status: common.TopUpStatusSuccess,
			PaymentProvider: model.PaymentProviderStripe, PaymentCurrency: "CNY", PaymentReference: "large-" + strconv.Itoa(index),
			PaymentVerified: true, PaymentPositive: true, PaymentPrincipalFen: principal}
		require.NoError(t, db.Create(&payment).Error)
		require.NoError(t, db.Create(&model.UnifiedWalletPaymentReceipt{ID: model.WalletReference("payment-receipt", payment.PaymentProvider, payment.PaymentReference), TopUpID: payment.Id}).Error)
		response = call(queryFor(user.Id), secret)
		if index == 0 {
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &support))
			assert.EqualValues(t, common.MaxWalletQuota, support.SupportCNYFen)
		} else {
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, "overflow must not return an inexact support amount")
		}
	}
	require.NoError(t, db.Delete(&user).Error)
	assert.Equal(t, http.StatusBadRequest, call(queryFor(user.Id), secret).Code)
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
