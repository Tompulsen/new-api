package model

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
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

// Exercise the real signed transport and SQL wallet; only the remote QX ledger is a fixture.
func TestQXChatSharedWalletReservations(t *testing.T) {
	unifiedWalletDatabase(t)
	t.Setenv("QXCHAT_SHARED_WALLET_ENABLED", "1")
	t.Setenv("QXCHAT_IDENTITY_ENABLED", "1")
	t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "500000")
	secret := "shared-wallet-source-test-secret-32-characters"
	t.Setenv("QXCHAT_IDENTITY_SECRET", secret)
	user := createReserveTestUser(t, 5000000)
	identity := QXChatIdentity{SourceID: "52b34e97-7a0f-4e80-85a8-d511ed61aef5", UserID: user.Id, State: QXChatIdentityActive, AuthorityVersion: 1}
	require.NoError(t, DB.Create(&identity).Error)
	var mu sync.Mutex
	available := int64(150000)
	invalidSignature := false
	var reservations []QXChatWalletReservationStatus
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(500)
			return
		}
		digest := sha256.Sum256(body)
		canonical := strings.Join([]string{"qxchat-identity-v1", "POST", common.QXChatSharedWalletPath, r.Header.Get("X-QXChat-Timestamp"), r.Header.Get("X-QXChat-Nonce"), hex.EncodeToString(digest[:])}, "\n")
		if !assert.Equal(t, common.GenerateHMACWithKey([]byte(secret), canonical), r.Header.Get("X-QXChat-Signature")) {
			w.WriteHeader(401)
			return
		}
		var input struct {
			Action        string `json:"action"`
			SourceID      string `json:"source_id"`
			UserID        int    `json:"newapi_user_id"`
			Version       int64  `json:"authority_version"`
			ReservationID string `json:"reservation_id"`
			OperationID   string `json:"operation_id"`
			Quota         int64  `json:"quota"`
			Rate          int64  `json:"quota_per_point"`
		}
		if !assert.NoError(t, common.Unmarshal(body, &input)) {
			w.WriteHeader(400)
			return
		}
		assert.Equal(t, identity.SourceID, input.SourceID)
		assert.Equal(t, user.Id, input.UserID)
		assert.Equal(t, int64(1), input.Version)
		var output any
		if input.Action == "balance" {
			output = map[string]any{"source_id": identity.SourceID, "newapi_user_id": user.Id, "points": strconv.FormatInt(available/100, 10), "expires_at": nil}
		} else {
			assert.Equal(t, "reserve", input.Action)
			assert.Equal(t, int64(100), input.Rate)
			take := min(input.Quota, available)
			available -= take
			reservations = append(reservations, QXChatWalletReservationStatus{ReservationID: input.ReservationID, OperationID: input.OperationID, UserID: user.Id})
			output = map[string]any{"reservation_id": input.ReservationID, "quota": take}
		}
		response, err := common.Marshal(output)
		if !assert.NoError(t, err) {
			w.WriteHeader(500)
			return
		}
		hash := sha256.Sum256(response)
		proof := strings.Join([]string{"qxchat-identity-response-v1", r.Header.Get("X-QXChat-Nonce"), "200", hex.EncodeToString(hash[:])}, "\n")
		if !invalidSignature {
			w.Header().Set("X-QXChat-Signature", common.GenerateHMACWithKey([]byte(secret), proof))
		}
		_, _ = w.Write(response)
	}))
	defer source.Close()
	t.Setenv("QXCHAT_IDENTITY_SOURCE_URL", source.URL)
	view, err := GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "1500", view.CharityPoints)
	assert.Equal(t, "50000", view.PermanentPoints)
	mu.Lock()
	available = 100000
	mu.Unlock()
	view, err = GetUnifiedWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "1000", view.CharityPoints)
	assert.Equal(t, "50000", view.PermanentPoints)

	// Community first exhausts QX, then consumes the existing New API balance.
	id := WalletReference("shared-community")
	require.NoError(t, BeginWalletOperation(user.Id, id, WalletCommunity, 100001, 0))
	assert.Equal(t, 4999999, getUserQuotaFromDB(t, user.Id))
	require.Len(t, reservations, 1)
	status, err := GetQXChatWalletReservationStatus(reservations[0])
	require.NoError(t, err)
	assert.Equal(t, int64(100000), status.Quota)
	assert.False(t, status.Terminal)
	require.NoError(t, ChangeWalletOperation(user.Id, id, 99999, "reserved", false))
	assert.Equal(t, 5000000, getUserQuotaFromDB(t, user.Id))
	status, err = GetQXChatWalletReservationStatus(reservations[0])
	require.NoError(t, err)
	assert.Equal(t, int64(99999), status.Quota)
	// A later increase takes a new reservation; the first target never grows.
	mu.Lock()
	available = 1
	mu.Unlock()
	require.NoError(t, ChangeWalletOperation(user.Id, id, 100000, "reserved", false))
	require.Len(t, reservations, 2)
	assert.NotEqual(t, reservations[0].ReservationID, reservations[1].ReservationID)
	status, err = GetQXChatWalletReservationStatus(reservations[0])
	require.NoError(t, err)
	assert.Equal(t, int64(99999), status.Quota)
	require.NoError(t, ChangeWalletOperation(user.Id, id, 0, "refunded", false))
	require.NoError(t, ChangeWalletOperation(user.Id, id, 0, "refunded", false))
	for _, request := range reservations {
		status, err = GetQXChatWalletReservationStatus(request)
		require.NoError(t, err)
		assert.Zero(t, status.Quota)
		assert.True(t, status.Terminal)
	}

	// A failed token debit rolls back the local operation; the source can safely release its hold.
	token := unifiedWalletToken(t, user.Id, 0)
	mu.Lock()
	available = 100
	mu.Unlock()
	failedID := WalletReference("shared-rollback")
	assert.ErrorIs(t, BeginWalletOperation(user.Id, failedID, WalletCommunity, 50, token.Id), ErrWalletInsufficient)
	status, err = GetQXChatWalletReservationStatus(reservations[len(reservations)-1])
	require.NoError(t, err)
	assert.Zero(t, status.Quota)
	assert.True(t, status.Terminal)
	assert.Equal(t, 5000000, getUserQuotaFromDB(t, user.Id))

	// Invalid signatures must never silently fall back to spending permanent credit.
	mu.Lock()
	invalidSignature = true
	mu.Unlock()
	assert.ErrorIs(t, BeginWalletOperation(user.Id, WalletReference("shared-unsigned"), WalletCommunity, 1, 0), ErrWalletUnavailable)
	assert.Equal(t, 5000000, getUserQuotaFromDB(t, user.Id))
	mu.Lock()
	invalidSignature = false
	mu.Unlock()
	count := len(reservations)
	require.NoError(t, BeginWalletOperation(user.Id, WalletReference("shared-ordinary-api"), WalletAPI, 1, 0))
	assert.Len(t, reservations, count, "ordinary API calls retain their original billing source")

	t.Run("lottery precedes other welfare and permanent credit", func(t *testing.T) {
		unifiedWalletPayment(t, user.Id, 25000, 5)
		permanentBefore := getUserQuotaFromDB(t, user.Id)
		sharedCallsBefore := len(reservations)
		expiry := time.Now().Add(time.Hour).Unix()
		ordinary := unifiedWalletCharity(t, user.Id, 10000, expiry-1800)
		unlimited := unifiedWalletCharity(t, user.Id, 10000, 0)
		later := unifiedWalletCharity(t, user.Id, 30000, expiry+3600)
		soon := unifiedWalletCharity(t, user.Id, 20000, expiry)
		expired := unifiedWalletCharity(t, user.Id, 40000, expiry-7200)
		require.NoError(t, DB.Model(&UnifiedWalletLot{}).Where("id IN ?", []int64{later.ID, soon.ID, expired.ID}).Update("source", "community_lottery").Error)
		mu.Lock()
		available = 100000
		mu.Unlock()
		requestToken := unifiedWalletToken(t, user.Id, 200000)
		operationID := WalletReference("lottery-priority")
		require.NoError(t, BeginWalletOperation(user.Id, operationID, WalletCommunity, 10000, requestToken.Id))
		assert.Len(t, reservations, sharedCallsBefore, "lottery-only usage must not reserve shared welfare")
		require.NoError(t, DB.First(&soon, soon.ID).Error)
		assert.Equal(t, int64(10000), soon.RemainingQuota, "earliest lottery batch is consumed first")
		require.NoError(t, DB.First(&ordinary, ordinary.ID).Error)
		assert.Equal(t, ordinary.InitialQuota, ordinary.RemainingQuota, "even earlier ordinary welfare follows lottery rewards")
		assert.Equal(t, permanentBefore, getUserQuotaFromDB(t, user.Id))

		require.NoError(t, ChangeWalletOperation(user.Id, operationID, 30000, "reserved", true))
		require.NoError(t, DB.First(&later, later.ID).Error)
		assert.Equal(t, int64(20000), later.RemainingQuota)
		assert.Len(t, reservations, sharedCallsBefore)
		exhausted, err := ReserveWalletRealtimeUsage(user.Id, operationID, 180000, 180000)
		require.NoError(t, err)
		assert.False(t, exhausted)
		assert.Equal(t, permanentBefore-10000, getUserQuotaFromDB(t, user.Id))
		assert.Equal(t, 20000, getTokenFromDB(t, requestToken.Id).RemainQuota)
		require.Len(t, reservations, sharedCallsBefore+1)
		sharedRequest := reservations[len(reservations)-1]
		sharedStatus, err := GetQXChatWalletReservationStatus(sharedRequest)
		require.NoError(t, err)
		assert.Equal(t, int64(100000), sharedStatus.Quota)
		for _, lot := range []UnifiedWalletLot{soon, later, ordinary, unlimited} {
			require.NoError(t, DB.First(&lot, lot.ID).Error)
			assert.Zero(t, lot.RemainingQuota)
		}
		require.NoError(t, DB.First(&expired, expired.ID).Error)
		assert.Equal(t, expired.InitialQuota, expired.RemainingQuota)

		require.NoError(t, ChangeWalletOperation(user.Id, operationID, 40000, "reserved", false))
		assert.Equal(t, permanentBefore, getUserQuotaFromDB(t, user.Id))
		assert.Equal(t, 160000, getTokenFromDB(t, requestToken.Id).RemainQuota)
		sharedStatus, err = GetQXChatWalletReservationStatus(sharedRequest)
		require.NoError(t, err)
		assert.Zero(t, sharedStatus.Quota)
		require.NoError(t, DB.First(&later, later.ID).Error)
		assert.Equal(t, int64(10000), later.RemainingQuota, "reduced usage retains lottery spending")
		for _, lot := range []UnifiedWalletLot{ordinary, unlimited} {
			require.NoError(t, DB.First(&lot, lot.ID).Error)
			assert.Equal(t, lot.InitialQuota, lot.RemainingQuota)
		}

		// A refund preserves the original expiry, including a batch that just expired.
		soon.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		require.NoError(t, DB.Model(&soon).Update("expires_at", soon.ExpiresAt).Error)
		require.NoError(t, ChangeWalletOperation(user.Id, operationID, 0, "refunded", false))
		require.NoError(t, ChangeWalletOperation(user.Id, operationID, 0, "refunded", false))
		assert.Equal(t, 200000, getTokenFromDB(t, requestToken.Id).RemainQuota)
		sharedStatus, err = GetQXChatWalletReservationStatus(sharedRequest)
		require.NoError(t, err)
		assert.Zero(t, sharedStatus.Quota)
		assert.True(t, sharedStatus.Terminal)

		// Both local lottery debits and remote holds roll back when token charging fails.
		mu.Lock()
		available = 100000
		mu.Unlock()
		sharedCallsBefore = len(reservations)
		assert.ErrorIs(t, BeginWalletOperation(user.Id, WalletReference("lottery-token-rollback"), WalletCommunity, 80000, token.Id), ErrWalletInsufficient)
		require.Len(t, reservations, sharedCallsBefore+1)
		sharedStatus, err = GetQXChatWalletReservationStatus(reservations[len(reservations)-1])
		require.NoError(t, err)
		assert.Zero(t, sharedStatus.Quota)
		assert.True(t, sharedStatus.Terminal)
		mu.Lock()
		invalidSignature = true
		mu.Unlock()
		assert.ErrorIs(t, BeginWalletOperation(user.Id, WalletReference("lottery-source-rollback"), WalletCommunity, 80000, 0), ErrWalletUnavailable)
		mu.Lock()
		invalidSignature = false
		mu.Unlock()
		for _, original := range []UnifiedWalletLot{soon, later, ordinary, unlimited, expired} {
			var stored UnifiedWalletLot
			require.NoError(t, DB.First(&stored, original.ID).Error)
			assert.Equal(t, original.InitialQuota, stored.RemainingQuota)
			assert.Equal(t, original.ExpiresAt, stored.ExpiresAt)
		}
		assert.Equal(t, permanentBefore, getUserQuotaFromDB(t, user.Id))
		sharedCallsBefore = len(reservations)
		require.NoError(t, BeginWalletOperation(user.Id, WalletReference("lottery-ordinary-api"), WalletAPI, 1, 0))
		assert.Len(t, reservations, sharedCallsBefore)
		require.NoError(t, DB.First(&later, later.ID).Error)
		assert.Equal(t, later.InitialQuota, later.RemainingQuota, "ordinary API calls cannot consume lottery welfare")
	})

	// Rebinding and soft deletion do not erase historical committed refund targets.
	require.NoError(t, DB.Model(&identity).Update("state", QXChatIdentityPending).Error)
	require.NoError(t, DB.Delete(&user).Error)
	status, err = GetQXChatWalletReservationStatus(reservations[0])
	require.NoError(t, err)
	assert.Zero(t, status.Quota)
	require.NoError(t, DB.Unscoped().Delete(&user).Error)
	_, err = GetQXChatWalletReservationStatus(reservations[0])
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}
