package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func unifiedWalletDatabase(t *testing.T) {
	t.Helper()
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "5000")
	useScheduledChannelDatabase(t)
	resetBatchUpdateTestState(t)
	oldMemory := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = oldMemory })
	tables := []any{&User{}, &Token{}, &TopUp{}, &Task{}, &Midjourney{}, &QXChatIdentity{}, &UnifiedWalletAccount{}, &UnifiedWalletLot{}, &UnifiedWalletOperation{}, &UnifiedWalletEntry{}, &UnifiedWalletReceipt{}, &UnifiedWalletNonce{}, &UnifiedWalletPaymentReceipt{}, &UnifiedWalletPaymentReversal{}}
	require.NoError(t, DB.AutoMigrate(&User{}, &Token{}, &TopUp{}, &Task{}, &Midjourney{}))
	require.NoError(t, MigrateQXChatIdentity(DB))
	require.NoError(t, MigrateUnifiedWallet(DB))
	require.NoError(t, MigrateUnifiedWallet(DB))
	t.Cleanup(func() { require.NoError(t, DB.Migrator().DropTable(tables...)) })
}

func unifiedWalletPayment(t *testing.T, userID, quota int, fen int64) TopUp {
	t.Helper()
	topup := TopUp{UserId: userID, TradeNo: "wallet-paid-" + common.GetRandomString(12), PaymentProvider: PaymentProviderStripe, PaymentCurrency: "CNY", PaymentPrincipalFen: fen, PaymentVerified: true, PaymentReference: "wallet-payment-" + common.GetRandomString(12), Status: common.TopUpStatusSuccess}
	require.NoError(t, DB.Create(&topup).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error { return CreditVerifiedPaymentTx(tx, userID, quota, &topup, nil) }))
	return topup
}

func unifiedWalletCharity(t *testing.T, userID int, quota int64, expiry int64) UnifiedWalletLot {
	t.Helper()
	lot := UnifiedWalletLot{UserID: userID, Kind: WalletCharity, Reference: WalletReference(common.NewRequestId()), Source: "test-import", InitialQuota: quota, RemainingQuota: quota, InitialPoints: quota, ExpiresAt: expiry, CreatedAt: time.Now().Unix()}
	require.NoError(t, DB.Create(&lot).Error)
	return lot
}

func unifiedWalletToken(t *testing.T, userID, quota int) Token {
	t.Helper()
	token := Token{UserId: userID, Key: common.GetRandomString(48), RemainQuota: quota, ExpiredTime: -1, Status: common.TokenStatusEnabled}
	require.NoError(t, DB.Create(&token).Error)
	return token
}

func TestUnifiedWalletSourceIsolationAndOriginalRefund(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 100)
	unifiedWalletPayment(t, user.Id, 100, 2)
	charity := unifiedWalletCharity(t, user.Id, 50, time.Now().Add(time.Hour).Unix())
	communityID, apiID := WalletReference("community"), WalletReference("api")
	require.NoError(t, BeginWalletOperation(user.Id, communityID, WalletCommunity, 80, 0))
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "0", view.CharityPoints)
	assert.Equal(t, int64(100), view.LegacyAPIQuota)
	assert.Equal(t, int64(70), view.PaidAPIQuota)
	require.NoError(t, BeginWalletOperation(user.Id, apiID, WalletAPI, 50, 0))
	require.NoError(t, ChangeWalletOperation(user.Id, apiID, 50, "settled", false))
	require.NoError(t, ChangeWalletOperation(user.Id, communityID, 0, "refunded", false))
	require.NoError(t, ChangeWalletOperation(user.Id, communityID, 0, "refunded", false))
	assert.ErrorIs(t, BeginWalletOperation(user.Id, communityID, WalletCommunity, 80, 0), ErrWalletDuplicate)
	view, err = GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "50", view.CharityPoints)
	assert.Equal(t, "150", view.PermanentPoints)
	assert.Equal(t, int64(150), view.TotalAPIQuota)
	require.NoError(t, DB.First(&charity, charity.ID).Error)
	assert.Equal(t, int64(50), charity.RemainingQuota)
	// All existing available New API credit is permanent community credit.
	legacy := createReserveTestUser(t, 100)
	require.NoError(t, BeginWalletOperation(legacy.Id, WalletReference("legacy-community"), WalletCommunity, 1, 0))
	assert.Equal(t, 99, getUserQuotaFromDB(t, legacy.Id))
}

func TestUnifiedWalletCharityExpiryDetailsAndPagination(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 500)
	other := createReserveTestUser(t, 0)
	expiry := time.Now().Add(time.Hour).Unix()
	first := unifiedWalletCharity(t, user.Id, 100, expiry)
	require.NoError(t, DB.Model(&first).Update("remaining_quota", 40).Error)
	unifiedWalletCharity(t, user.Id, 25, expiry)
	permanent := unifiedWalletCharity(t, user.Id, 300, 0)
	unifiedWalletCharity(t, user.Id, 80, expiry+3600)
	unifiedWalletCharity(t, user.Id, 50, time.Now().Add(-time.Minute).Unix())
	unifiedWalletCharity(t, other.Id, 90, expiry)
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "445", view.CharityPoints)
	assert.Equal(t, "500", view.PermanentPoints)
	assert.Equal(t, &expiry, view.CharityExpiringAt)
	require.NotNil(t, view.CharityExpiringPoints)
	assert.Equal(t, "65", *view.CharityExpiringPoints)
	assert.True(t, view.CharityDetailsAvailable)

	details, err := GetUnifiedWalletLots(user.Id, 0)
	require.NoError(t, err)
	require.Len(t, details.Items, 4)
	assert.Nil(t, details.NextCursor)
	assert.Equal(t, "0", details.SharedPoints)
	for _, item := range details.Items {
		if item.ID == strconv.FormatInt(permanent.ID, 10) {
			assert.Nil(t, item.ExpiresAt)
			assert.Equal(t, "300", item.Points)
		}
	}
	details, err = GetUnifiedWalletLots(other.Id, 0)
	require.NoError(t, err)
	require.Len(t, details.Items, 1)
	assert.Equal(t, "90", details.Items[0].Points)

	paged := createReserveTestUser(t, 0)
	lots := make([]UnifiedWalletLot, 51)
	for i := range lots {
		lots[i] = UnifiedWalletLot{UserID: paged.Id, Kind: WalletCharity, Source: "community_lottery", Reference: WalletReference("page", strconv.Itoa(i)), InitialQuota: 10, RemainingQuota: 10, InitialPoints: 10, ExpiresAt: expiry, CreatedAt: time.Now().Unix()}
	}
	require.NoError(t, DB.Create(&lots).Error)
	details, err = GetUnifiedWalletLots(paged.Id, 0)
	require.NoError(t, err)
	require.Len(t, details.Items, 50)
	require.NotNil(t, details.NextCursor)
	before, err := strconv.ParseInt(*details.NextCursor, 10, 64)
	require.NoError(t, err)
	last, err := GetUnifiedWalletLots(paged.Id, before)
	require.NoError(t, err)
	require.Len(t, last.Items, 1)
	assert.Nil(t, last.NextCursor)
	assert.Equal(t, strconv.FormatInt(lots[0].ID, 10), last.Items[0].ID)
	_, err = GetUnifiedWalletLots(paged.Id, -1)
	assert.ErrorIs(t, err, ErrWalletConflict)

	t.Setenv("QXCHAT_SHARED_WALLET_ENABLED", "1")
	t.Setenv("QXCHAT_IDENTITY_ENABLED", "1")
	secret := strings.Repeat("wallet-expiry-source", 3)
	t.Setenv("QXCHAT_IDENTITY_SECRET", secret)
	identity := QXChatIdentity{SourceID: "52b34e97-7a0f-4e80-85a8-d511ed61aef5", UserID: user.Id, State: QXChatIdentityActive, AuthorityVersion: 1}
	require.NoError(t, DB.Create(&identity).Error)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, err := common.Marshal(map[string]any{"source_id": identity.SourceID, "newapi_user_id": user.Id, "points": "100", "expires_at": expiry})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		digest := sha256.Sum256(response)
		proof := strings.Join([]string{"qxchat-identity-response-v1", r.Header.Get("X-QXChat-Nonce"), "200", hex.EncodeToString(digest[:])}, "\n")
		w.Header().Set("X-QXChat-Signature", common.GenerateHMACWithKey([]byte(secret), proof))
		_, _ = w.Write(response)
	}))
	defer source.Close()
	t.Setenv("QXCHAT_IDENTITY_SOURCE_URL", source.URL)
	view, err = GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "545", view.CharityPoints)
	assert.Equal(t, &expiry, view.CharityExpiringAt)
	assert.Nil(t, view.CharityExpiringPoints, "shared funds with the same earliest expiry have no known expiring amount")
	details, err = GetUnifiedWalletLots(user.Id, 0)
	require.NoError(t, err)
	assert.Equal(t, "100", details.SharedPoints)
}

func TestUnifiedWalletCharityOnlyAndTokenAtomicity(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 0)
	lot := unifiedWalletCharity(t, user.Id, 20, time.Now().Add(time.Hour).Unix())
	token := unifiedWalletToken(t, user.Id, 5)
	assert.ErrorIs(t, BeginWalletOperation(user.Id, WalletReference("ordinary"), WalletAPI, 1, token.Id), ErrWalletInsufficient)
	assert.ErrorIs(t, BeginWalletOperation(user.Id, WalletReference("token-limit"), WalletCommunity, 6, token.Id), ErrWalletInsufficient)
	require.NoError(t, DB.First(&lot, lot.ID).Error)
	assert.Equal(t, int64(20), lot.RemainingQuota)
	assert.Equal(t, 5, getTokenFromDB(t, token.Id).RemainQuota)
	id := WalletReference("charity-only")
	require.NoError(t, BeginWalletOperation(user.Id, id, WalletCommunity, 5, token.Id))
	require.NoError(t, DB.Model(&lot).Update("expires_at", time.Now().Add(-time.Minute).Unix()).Error)
	require.NoError(t, ChangeWalletOperation(user.Id, id, 0, "refunded", false))
	assert.Equal(t, 5, getTokenFromDB(t, token.Id).RemainQuota)
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "0", view.CharityPoints)
	assert.Zero(t, view.TotalAPIQuota)
	require.NoError(t, DB.First(&lot, lot.ID).Error)
	assert.Equal(t, int64(20), lot.RemainingQuota, "expiry survives the original-source refund")
}

func TestUnifiedWalletConcurrentAPIAndCommunity(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 0)
	unifiedWalletPayment(t, user.Id, 100, 2)
	lot := unifiedWalletCharity(t, user.Id, 20, 0)
	token := unifiedWalletToken(t, user.Id, 1000)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, mode := range []string{WalletAPI, WalletCommunity} {
		workers.Go(func() {
			<-start
			results <- BeginWalletOperation(user.Id, WalletReference("concurrent", mode), mode, 75, token.Id)
		})
	}
	close(start)
	workers.Wait()
	close(results)
	successful := 0
	for err := range results {
		if err == nil {
			successful++
		} else {
			assert.ErrorIs(t, err, ErrWalletInsufficient)
		}
	}
	assert.Equal(t, 1, successful)
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	require.NoError(t, DB.First(&lot, lot.ID).Error)
	assert.Equal(t, int64(45), view.TotalAPIQuota+lot.RemainingQuota)
	assert.Equal(t, 925, getTokenFromDB(t, token.Id).RemainQuota)
	assert.Equal(t, view.TotalAPIQuota, view.PaidAPIQuota)
}

func TestUnifiedWalletImportIdempotencyAndExpiry(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 7)
	identity := QXChatIdentity{SourceID: "52b34e97-7a0f-4e80-85a8-d511ed61aef5", UserID: user.Id, State: QXChatIdentityActive, AuthorityVersion: 1}
	require.NoError(t, DB.Create(&identity).Error)
	expired := time.Now().Add(-time.Second).Unix()
	request := WalletImportRequest{TransferID: "transfer-00000000000001", SourceUserID: identity.SourceID, NewAPIUserID: user.Id, BindingVersion: 1, Lots: []WalletImportLot{{SourceLotID: "permanent", Points: 100}, {SourceLotID: "expired", Points: 30, ExpiresAt: &expired}}}
	digest := strings.Repeat("a", 64)
	receipt, err := ImportQXChatWallet(request, digest)
	require.NoError(t, err)
	assert.Equal(t, int64(130), receipt.Points)
	assert.Equal(t, int64(30), receipt.ExpiredPoints)
	again, err := ImportQXChatWallet(request, digest)
	require.NoError(t, err)
	assert.Equal(t, *receipt, *again)
	_, err = ImportQXChatWallet(request, strings.Repeat("b", 64))
	assert.ErrorIs(t, err, ErrWalletConflict)
	request.TransferID = "transfer-00000000000002"
	request.BindingVersion = 2
	_, err = ImportQXChatWallet(request, digest)
	assert.ErrorIs(t, err, ErrWalletConflict)
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "100", view.CharityPoints)
	assert.Equal(t, int64(7), view.TotalAPIQuota)
	assert.Equal(t, "7", view.PermanentPoints)
}

func TestUnifiedWalletTaskPersistenceAndZeroDelta(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 0)
	unifiedWalletPayment(t, user.Id, 100, 2)
	token := unifiedWalletToken(t, user.Id, 100)
	id := WalletReference("async-task")
	require.NoError(t, BeginWalletOperation(user.Id, id, WalletAPI, 40, token.Id))
	task := Task{UserId: user.Id, Quota: 40, PrivateData: TaskPrivateData{WalletOperationID: id, TokenId: token.Id}}
	require.NoError(t, task.InsertWithContext(context.Background()))
	var operation UnifiedWalletOperation
	require.NoError(t, DB.First(&operation, "id = ?", id).Error)
	assert.Equal(t, task.ID, operation.TaskID)
	delta, applied, err := ChangeWalletTaskQuota(&task, 40, "settled")
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Zero(t, delta)
	delta, applied, err = ChangeWalletTaskQuota(&task, 40, "settled")
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Zero(t, delta)
	_, _, err = ChangeWalletTaskQuota(&task, 0, "refunded")
	assert.ErrorIs(t, err, ErrWalletConflict)
	require.NoError(t, DB.First(&operation, "id = ?", id).Error)
	assert.Equal(t, "settled", operation.State)
	assert.Equal(t, 60, getTokenFromDB(t, token.Id).RemainQuota)
}

func TestUnifiedWalletDebtAndPaymentReversal(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 0)
	topup := unifiedWalletPayment(t, user.Id, 100, 2)
	id := WalletReference("reserved-refund")
	require.NoError(t, BeginWalletOperation(user.Id, id, WalletAPI, 70, 0))
	require.NoError(t, ReverseVerifiedPayment(PaymentProviderStripe, topup.PaymentReference, "refund-one", 1, true))
	require.NoError(t, ReverseVerifiedPayment(PaymentProviderStripe, topup.PaymentReference, "refund-one", 1, true))
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(-20), view.LegacyAPIQuota)
	assert.Zero(t, view.PaidAPIQuota)
	assert.Equal(t, "0", view.PermanentPoints)
	require.NoError(t, ChangeWalletOperation(user.Id, id, 0, "refunded", false))
	var paidRefund, legacyRefund int64
	require.NoError(t, DB.Model(&UnifiedWalletEntry{}).Select("COALESCE(SUM(quota_delta), 0)").Where("operation_id = ? AND event = ? AND kind = ?", id, "refund", WalletPaid).Scan(&paidRefund).Error)
	require.NoError(t, DB.Model(&UnifiedWalletEntry{}).Select("COALESCE(SUM(quota_delta), 0)").Where("operation_id = ? AND event = ? AND kind = ?", id, "refund_reversal_debt", WalletLegacy).Scan(&legacyRefund).Error)
	assert.Equal(t, int64(50), paidRefund)
	assert.Equal(t, int64(20), legacyRefund)
	view, err = GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Zero(t, view.LegacyAPIQuota)
	assert.Equal(t, int64(50), view.PaidAPIQuota)
	assert.Equal(t, "50", view.PermanentPoints)
	require.NoError(t, ReverseVerifiedPayment(PaymentProviderStripe, topup.PaymentReference, "refund-two", 2, true))
	view, err = GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Zero(t, view.TotalAPIQuota)
	// Delivered overrun cannot become a negative charge or disappear.
	require.NoError(t, ApplyWalletUsage(user.Id, 0, WalletReference("delivered"), WalletAPI, 12))
	require.NoError(t, ApplyWalletUsage(user.Id, 0, WalletReference("delivered"), WalletAPI, 12))
	view, err = GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(-12), view.LegacyAPIQuota)
	unifiedWalletPayment(t, user.Id, 10, 1)
	assert.ErrorIs(t, BeginWalletOperation(user.Id, WalletReference("debt-protected"), WalletCommunity, 1, 0), ErrWalletInsufficient)
}

func TestUnifiedWalletPaymentClassificationAndRatio(t *testing.T) {
	unifiedWalletDatabase(t)
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	user := createReserveTestUser(t, 0)
	topup := TopUp{UserId: user.Id, TradeNo: "verified-epay", Amount: 1, Money: 1, Status: common.TopUpStatusPending, PaymentProvider: PaymentProviderEpay}
	require.NoError(t, DB.Create(&topup).Error)
	_, err := RechargeEpay(topup.TradeNo, "alipay", "", VerifiedTopUpPayment{Currency: "CNY", PrincipalFen: 99, Reference: "epay-reference"})
	assert.Error(t, err)
	assert.Zero(t, getUserQuotaFromDB(t, user.Id))
	_, err = RechargeEpay(topup.TradeNo, "alipay", "", VerifiedTopUpPayment{Currency: "CNY", PrincipalFen: 100, Reference: "epay-reference"})
	require.NoError(t, err)
	again, err := RechargeEpay(topup.TradeNo, "alipay", "", VerifiedTopUpPayment{Currency: "CNY", PrincipalFen: 100, Reference: "epay-reference"})
	require.NoError(t, err)
	assert.True(t, again)
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "5000", view.PermanentPoints)
	require.NoError(t, IncreaseUserQuota(user.Id, 11, false))
	view, err = GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(11), view.LegacyAPIQuota)
	other := createReserveTestUser(t, 0)
	unknown := TopUp{UserId: other.Id, TradeNo: "unknown-currency", PaymentProvider: PaymentProviderCreem, PaymentCurrency: "USD", PaymentPrincipalFen: 100, PaymentVerified: true}
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error { return CreditVerifiedPaymentTx(tx, other.Id, 10, &unknown, nil) }))
	view, err = GetUnifiedWallet(other.Id)
	require.NoError(t, err)
	assert.Equal(t, "0.1", view.PermanentPoints)
	assert.Equal(t, int64(10), view.LegacyAPIQuota)
	fractional := createReserveTestUser(t, 0)
	unifiedWalletPayment(t, fractional.Id, 3, 1)
	require.NoError(t, BeginWalletOperation(fractional.Id, WalletReference("fraction"), WalletAPI, 1, 0))
	view, err = GetUnifiedWallet(fractional.Id)
	require.NoError(t, err)
	assert.Equal(t, "0.02", view.PermanentPoints)
	original := common.QuotaPerUnit
	common.QuotaPerUnit = original * 3
	t.Cleanup(func() { common.QuotaPerUnit = original })
	view, err = GetUnifiedWallet(fractional.Id)
	require.NoError(t, err)
	assert.Equal(t, "0.02", view.PermanentPoints)
}

func TestUnifiedWalletMigrationPreservesLegacyBalanceAndUniqueness(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, -5)
	// This user exists without a wallet row, as it would before feature activation.
	require.NoError(t, MigrateUnifiedWallet(DB))
	require.NoError(t, MigrateUnifiedWallet(DB))
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(-5), view.LegacyAPIQuota)
	assert.Equal(t, "0", view.PermanentPoints)
	lot := unifiedWalletCharity(t, user.Id, 10, 0)
	duplicate := lot
	duplicate.ID = 0
	assert.Error(t, DB.Create(&duplicate).Error)
	assert.Equal(t, -5, getUserQuotaFromDB(t, user.Id))
}

func TestUnifiedWalletMidjourneyZeroDeltaAndDuplicateRefund(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 100)
	token := unifiedWalletToken(t, user.Id, 100)
	for _, state := range []string{"settled", "refunded"} {
		id := WalletReference("mj-zero", state)
		task := Midjourney{UserId: user.Id, Quota: 20, TokenId: token.Id, WalletOperationID: id, Status: "SUBMITTED"}
		require.NoError(t, task.Insert())
		quota := 20
		if state == "refunded" {
			quota = 0
		}
		delta, applied, err := ChangeWalletMidjourneyQuota(&task, quota, state)
		require.NoError(t, err)
		require.True(t, applied)
		assert.Equal(t, quota-20, delta)
		_, applied, err = ChangeWalletMidjourneyQuota(&task, quota, state)
		require.NoError(t, err)
		assert.False(t, applied)
		var operation UnifiedWalletOperation
		require.NoError(t, DB.First(&operation, "id = ?", id).Error)
		assert.Equal(t, state, operation.State)
	}
	assert.Equal(t, 80, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 80, getTokenFromDB(t, token.Id).RemainQuota)
}

func TestUnifiedWalletRealtimeStopsAfterDeliveredUsage(t *testing.T) {
	for _, tokenLimited := range []bool{false, true} {
		t.Run(map[bool]string{false: "funds", true: "token"}[tokenLimited], func(t *testing.T) {
			unifiedWalletDatabase(t)
			funds, limit := 10, 100
			if tokenLimited {
				funds, limit = 100, 10
			}
			user := createReserveTestUser(t, funds)
			token := unifiedWalletToken(t, user.Id, limit)
			id := WalletReference("realtime")
			require.NoError(t, BeginWalletOperation(user.Id, id, WalletAPI, 5, token.Id))
			exhausted, err := ReserveWalletRealtimeUsage(user.Id, id, 6, 6)
			require.NoError(t, err)
			assert.False(t, exhausted)
			exhausted, err = ReserveWalletRealtimeUsage(user.Id, id, 12, 12)
			require.NoError(t, err)
			assert.True(t, exhausted)
			require.NoError(t, ChangeWalletOperation(user.Id, id, 12, "settled", false))
			assert.Equal(t, funds-12, getUserQuotaFromDB(t, user.Id))
			assert.Equal(t, limit-12, getTokenFromDB(t, token.Id).RemainQuota)
			assert.ErrorIs(t, BeginWalletOperation(user.Id, WalletReference("next-response"), WalletAPI, 1, token.Id), ErrWalletInsufficient)
		})
	}
}

func TestUnifiedWalletPaymentReferenceCannotFundTwoOrders(t *testing.T) {
	unifiedWalletDatabase(t)
	for i := 0; i < 2; i++ {
		user := createReserveTestUser(t, 0)
		topup := TopUp{UserId: user.Id, TradeNo: "duplicate-provider-" + common.GetRandomString(8), PaymentProvider: PaymentProviderStripe, PaymentCurrency: "CNY", PaymentPrincipalFen: 2, PaymentVerified: true, PaymentReference: "one-provider-payment", Status: common.TopUpStatusSuccess}
		require.NoError(t, DB.Create(&topup).Error)
		err := DB.Transaction(func(tx *gorm.DB) error { return CreditVerifiedPaymentTx(tx, user.Id, 100, &topup, nil) })
		if i == 0 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrWalletConflict)
			assert.Zero(t, getUserQuotaFromDB(t, user.Id))
		}
	}
	_, err := VerifiedPaymentMajor("CNY", "1.00", "")
	assert.Error(t, err)
}

func TestUnifiedWalletImportRechecksAuthorityAndAccount(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 0)
	identity := QXChatIdentity{SourceID: "52b34e97-7a0f-4e80-85a8-d511ed61aef5", UserID: user.Id, State: QXChatIdentityActive, AuthorityVersion: 1}
	require.NoError(t, DB.Create(&identity).Error)
	request := WalletImportRequest{TransferID: "transfer-disabled-account", SourceUserID: identity.SourceID, NewAPIUserID: user.Id, BindingVersion: 1, Lots: []WalletImportLot{{SourceLotID: "one", Points: 1}}}
	require.NoError(t, DB.Model(&user).Update("status", common.UserStatusDisabled).Error)
	_, err := ImportQXChatWallet(request, strings.Repeat("a", 64))
	assert.ErrorIs(t, err, ErrQXChatIdentityInactive)
	require.NoError(t, DB.Model(&user).Update("status", common.UserStatusEnabled).Error)
	require.NoError(t, DB.Model(&identity).Update("state", QXChatIdentityPending).Error)
	_, err = ImportQXChatWallet(request, strings.Repeat("a", 64))
	assert.ErrorIs(t, err, ErrQXChatIdentityInactive)
	var count int64
	require.NoError(t, DB.Model(&UnifiedWalletReceipt{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestUnifiedWalletActivationFence(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 1)
	_, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	t.Setenv("UNIFIED_WALLET_ENABLED", "false")
	require.ErrorContains(t, MigrateUnifiedWallet(DB), "has been activated")
	t.Setenv("UNIFIED_WALLET_ENABLED", "true")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "1")
	require.ErrorIs(t, MigrateUnifiedWallet(DB), ErrWalletUnavailable)
}

// The representative pre-feature top-up schema must preserve balances and
// uniqueness while new columns and wallet tables are installed twice.
func TestUnifiedWalletTopUpSchemaUpgrade(t *testing.T) {
	unifiedWalletDatabase(t)
	require.NoError(t, DB.Migrator().DropTable(&TopUp{}))
	type oldTopUp struct {
		Id              int
		UserId          int
		Amount          int64
		Money           float64
		TradeNo         string `gorm:"unique;type:varchar(255);index"`
		PaymentMethod   string `gorm:"type:varchar(50)"`
		PaymentProvider string `gorm:"type:varchar(50);default:''"`
		CreateTime      int64
		CompleteTime    int64
		Status          string
	}
	table := DB.NamingStrategy.TableName("TopUp")
	require.NoError(t, DB.Table(table).AutoMigrate(&oldTopUp{}))
	user := createReserveTestUser(t, 123)
	previous := oldTopUp{UserId: user.Id, Amount: 2, Money: 2, TradeNo: "before-wallet", Status: common.TopUpStatusSuccess, PaymentProvider: PaymentProviderStripe}
	require.NoError(t, DB.Table(table).Create(&previous).Error)
	for i := 0; i < 2; i++ {
		require.NoError(t, DB.AutoMigrate(&TopUp{}))
		require.NoError(t, MigrateUnifiedWallet(DB))
	}
	var upgraded TopUp
	require.NoError(t, DB.First(&upgraded, previous.Id).Error)
	assert.Equal(t, previous.TradeNo, upgraded.TradeNo)
	assert.False(t, upgraded.PaymentVerified)
	assert.Zero(t, upgraded.PaymentPrincipalFen)
	duplicate := upgraded
	duplicate.Id = 0
	assert.Error(t, DB.Create(&duplicate).Error)
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(123), view.LegacyAPIQuota)
	assert.Equal(t, "123", view.PermanentPoints)
}

func TestUnifiedWalletImportedCharityNeverBecomesAPIQuota(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 0)
	identity := QXChatIdentity{SourceID: "52b34e97-7a0f-4e80-85a8-d511ed61aef5", UserID: user.Id, State: QXChatIdentityActive, AuthorityVersion: 1}
	require.NoError(t, DB.Create(&identity).Error)
	expiry := time.Now().Add(time.Hour).Unix()
	request := WalletImportRequest{TransferID: "charity-only-lifecycle", SourceUserID: identity.SourceID, NewAPIUserID: user.Id, BindingVersion: 1, Lots: []WalletImportLot{{SourceLotID: "charity", Points: 100, ExpiresAt: &expiry}}}
	_, err := ImportQXChatWallet(request, strings.Repeat("c", 64))
	require.NoError(t, err)
	token := unifiedWalletToken(t, user.Id, 100)
	check := func(points string) {
		view, err := GetUnifiedWallet(user.Id)
		require.NoError(t, err)
		assert.Equal(t, points, view.CharityPoints)
		assert.Equal(t, "0", view.PermanentPoints)
		assert.Zero(t, view.PaidAPIQuota)
		assert.Zero(t, view.LegacyAPIQuota)
		assert.Zero(t, view.TotalAPIQuota)
		assert.Zero(t, getUserQuotaFromDB(t, user.Id))
		assert.ErrorIs(t, BeginWalletOperation(user.Id, WalletReference("ordinary", points), WalletAPI, 1, token.Id), ErrWalletInsufficient)
	}
	check("100")
	id := WalletReference("charity-only-consume")
	require.NoError(t, BeginWalletOperation(user.Id, id, WalletCommunity, 40, token.Id))
	check("60")
	require.NoError(t, ChangeWalletOperation(user.Id, id, 0, "refunded", false))
	check("100")
	require.NoError(t, ChangeWalletOperation(user.Id, id, 0, "refunded", false))
	check("100")
	expiredID := WalletReference("charity-before-expiry")
	require.NoError(t, BeginWalletOperation(user.Id, expiredID, WalletCommunity, 30, token.Id))
	require.NoError(t, DB.Model(&UnifiedWalletLot{}).Where("user_id = ? AND kind = ?", user.Id, WalletCharity).Update("expires_at", time.Now().Add(-time.Second).Unix()).Error)
	require.NoError(t, ChangeWalletOperation(user.Id, expiredID, 0, "refunded", false))
	check("0")
	assert.Equal(t, 100, getTokenFromDB(t, token.Id).RemainQuota)
	var paidLots int64
	require.NoError(t, DB.Model(&UnifiedWalletLot{}).Where("user_id = ? AND kind = ?", user.Id, WalletPaid).Count(&paidLots).Error)
	assert.Zero(t, paidLots)
}

func TestUnifiedWalletReceiptRecoveryAfterAccountRevocation(t *testing.T) {
	unifiedWalletDatabase(t)
	user := createReserveTestUser(t, 0)
	identity := QXChatIdentity{SourceID: "52b34e97-7a0f-4e80-85a8-d511ed61aef5", UserID: user.Id, State: QXChatIdentityActive, AuthorityVersion: 1}
	require.NoError(t, DB.Create(&identity).Error)
	request := WalletImportRequest{TransferID: "receipt-after-revocation", SourceUserID: identity.SourceID, NewAPIUserID: user.Id, BindingVersion: 1, Lots: []WalletImportLot{{SourceLotID: "one", Points: 25}}}
	digest := strings.Repeat("d", 64)
	receipt, err := ImportQXChatWallet(request, digest)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&user).Update("status", common.UserStatusDisabled).Error)
	require.NoError(t, DB.Model(&identity).Update("authority_version", 2).Error)
	recovered, err := ImportQXChatWallet(request, digest)
	require.NoError(t, err)
	assert.Equal(t, *receipt, *recovered)
	_, err = ImportQXChatWallet(request, strings.Repeat("e", 64))
	assert.ErrorIs(t, err, ErrWalletConflict)
	request.TransferID = "new-transfer-after-revocation"
	_, err = ImportQXChatWallet(request, digest)
	assert.Error(t, err)
	var count int64
	require.NoError(t, DB.Model(&UnifiedWalletLot{}).Where("user_id = ?", user.Id).Count(&count).Error)
	assert.Equal(t, int64(1), count)
	assert.Zero(t, getUserQuotaFromDB(t, user.Id))
}
