package model

import (
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const referralTestOrigin = "https://community.test"

func referralDatabase(t *testing.T) {
	t.Helper()
	unifiedWalletDatabase(t)
	tables := []any{&CommunityReferralConfig{}, &CommunityReferralCode{}, &CommunityReferral{}, &LoginIdentifierReservation{}}
	require.NoError(t, DB.AutoMigrate(tables...))
	require.NoError(t, DB.AutoMigrate(tables...))
	t.Cleanup(func() { require.NoError(t, DB.Migrator().DropTable(tables...)) })
	oldQuota := common.QuotaForNewUser
	common.QuotaForNewUser = 0
	t.Cleanup(func() { common.QuotaForNewUser = oldQuota })
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		db, err := DB.DB()
		require.NoError(t, err)
		db.SetMaxOpenConns(2) // One connection is retained by the existing in-memory fixture.
	}
}

func referralNewUser(t *testing.T, code string) User {
	t.Helper()
	user := User{Username: "ref-" + common.GetRandomString(10), Status: common.UserStatusEnabled, CommunityInviteCode: code}
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error { return user.InsertWithTx(tx, 0) }))
	return user
}

func referralInvitation(t *testing.T) (User, string) {
	t.Helper()
	inviter := createReserveTestUser(t, 0)
	view, err := GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(5000), view.Config.InviterPoints)
	assert.Equal(t, int64(5000), view.Config.InviteePoints)
	again, err := GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
	require.NoError(t, err)
	assert.Equal(t, view.Code, again.Code)
	return inviter, view.Code
}

func referralPaidOrder(t *testing.T, user User) TopUp {
	t.Helper()
	order := TopUp{UserId: user.Id, TradeNo: "referral-" + common.GetRandomString(20), Amount: 1, Money: .01,
		PaymentMethod: "alipay", PaymentProvider: PaymentProviderEpay, Status: common.TopUpStatusPending, CreateTime: time.Now().Unix()}
	require.NoError(t, DB.Create(&order).Error)
	payment, err := VerifiedPaymentMajor("CNY", "0.01", "provider-"+order.TradeNo)
	require.NoError(t, err)
	_, err = RechargeEpay(order.TradeNo, "alipay", "", payment)
	require.NoError(t, err)
	return order
}

func TestCommunityReferralRegistrationAndFrozenConfiguration(t *testing.T) {
	referralDatabase(t)
	inviter, code := referralInvitation(t)
	invitee := referralNewUser(t, code)
	assert.Zero(t, getUserQuotaFromDB(t, inviter.Id))
	assert.Zero(t, getUserQuotaFromDB(t, invitee.Id), "registration alone grants no referral points")
	require.NoError(t, creditCommunityReferral(invitee.Id))
	config, err := UpdateCommunityReferralConfig(referralTestOrigin, inviter.Id, 1, false, 8000, 9000)
	require.NoError(t, err)
	assert.Equal(t, int64(2), config.Version)
	_, err = UpdateCommunityReferralConfig(referralTestOrigin, inviter.Id, 1, true, 1, 1)
	assert.ErrorIs(t, err, ErrWalletConflict)
	newer := referralNewUser(t, code)
	var count int64
	require.NoError(t, DB.Model(&CommunityReferral{}).Where("invitee_id = ?", newer.Id).Count(&count).Error)
	assert.Zero(t, count, "disabled configuration stops new bindings")
	referralPaidOrder(t, invitee)
	paidQuota := getUserQuotaFromDB(t, invitee.Id)
	require.NoError(t, creditCommunityReferral(invitee.Id))
	assert.Equal(t, 5000, getUserQuotaFromDB(t, inviter.Id))
	assert.Equal(t, paidQuota+5000, getUserQuotaFromDB(t, invitee.Id))
	view, err := GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), view.Credited)
	assert.Equal(t, int64(5000), view.Earned)
	assert.Equal(t, int64(1), view.Items[0].ConfigVersion)
	assert.Equal(t, "credited", view.Items[0].State)
	// Updating or visiting an existing account never calls the registration binder.
	old := createReserveTestUser(t, 0)
	old.CommunityInviteCode = code
	require.NoError(t, DB.Model(&old).Update("display_name", "Existing account").Error)
	require.NoError(t, DB.Model(&CommunityReferral{}).Where("invitee_id = ?", old.Id).Count(&count).Error)
	assert.Zero(t, count)
	_, err = UpdateCommunityReferralConfig(referralTestOrigin, inviter.Id, 2, true, 0, 5000)
	assert.ErrorIs(t, err, ErrInvalidTopUpQuota)
	_, err = UpdateCommunityReferralConfig(referralTestOrigin, inviter.Id, 2, true, 100000001, 5000)
	assert.ErrorIs(t, err, ErrInvalidTopUpQuota)
}

func TestCommunityReferralAtomicRetriesAndConcurrentDelivery(t *testing.T) {
	referralDatabase(t)
	inviter, code := referralInvitation(t)
	invitee := referralNewUser(t, code)
	referralPaidOrder(t, invitee)
	paidQuota := getUserQuotaFromDB(t, invitee.Id)
	// Force the second credit to exceed the wallet ceiling; the first must roll back.
	require.NoError(t, DB.Model(&invitee).Update("quota", common.MaxWalletQuota).Error)
	require.NoError(t, DB.Model(&UnifiedWalletAccount{}).Where("user_id = ?", invitee.Id).Update("legacy_quota", common.MaxWalletQuota-paidQuota).Error)
	assert.ErrorIs(t, creditCommunityReferral(invitee.Id), ErrWalletQuotaLimitExceeded)
	assert.Zero(t, getUserQuotaFromDB(t, inviter.Id))
	var row CommunityReferral
	require.NoError(t, DB.First(&row, "invitee_id = ?", invitee.Id).Error)
	assert.Equal(t, "pending", row.State)
	assert.Zero(t, row.CreditedAt)
	require.NoError(t, DB.Model(&invitee).Update("quota", paidQuota).Error)
	require.NoError(t, DB.Model(&UnifiedWalletAccount{}).Where("user_id = ?", invitee.Id).Update("legacy_quota", 0).Error)
	var wg sync.WaitGroup
	errorsFound := make(chan error, 4)
	for range 4 {
		wg.Go(func() { errorsFound <- creditCommunityReferral(invitee.Id) })
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		require.NoError(t, err)
	}
	assert.Equal(t, 5000, getUserQuotaFromDB(t, inviter.Id))
	assert.Equal(t, paidQuota+5000, getUserQuotaFromDB(t, invitee.Id))
	var entries int64
	require.NoError(t, DB.Model(&UnifiedWalletEntry{}).Where("operation_id = ?", WalletReference("community-referral", strconv.Itoa(invitee.Id))).Count(&entries).Error)
	assert.Equal(t, int64(2), entries)
	referralPaidOrder(t, invitee)
	require.NoError(t, creditCommunityReferral(invitee.Id))
	assert.Equal(t, 5000, getUserQuotaFromDB(t, inviter.Id), "a second recharge cannot grant a second reward")
}

func TestCommunityReferralPaymentEvidence(t *testing.T) {
	referralDatabase(t)
	inviter, code := referralInvitation(t)
	for _, kind := range []string{"unverified", "zero", "gift", "before-binding", "refunded", "USD", "CNY"} {
		t.Run(kind, func(t *testing.T) {
			invitee := referralNewUser(t, code)
			var row CommunityReferral
			require.NoError(t, DB.First(&row, "invitee_id = ?", invitee.Id).Error)
			order := TopUp{UserId: invitee.Id, TradeNo: common.GetRandomString(24), Status: common.TopUpStatusSuccess, Money: 100,
				CreateTime: row.BoundAt, CompleteTime: row.BoundAt, PaymentProvider: PaymentProviderStripe, PaymentCurrency: "CNY",
				PaymentReference: common.GetRandomString(24), PaymentVerified: true, PaymentPositive: true, PaymentPrincipalFen: 1}
			switch kind {
			case "unverified":
				order.PaymentVerified = false
			case "zero":
				order.PaymentPositive = false
			case "gift":
				order.PaymentProvider = "community_lottery"
			case "before-binding":
				order.CreateTime = row.BoundAt - 1
			case "refunded":
				order.PaymentRefundedFen = 1
			case "USD":
				order.PaymentCurrency = "USD"
				order.PaymentPrincipalFen = 0
			}
			require.NoError(t, DB.Create(&order).Error)
			require.NoError(t, creditCommunityReferral(invitee.Id))
			want := 0
			if kind == "USD" || kind == "CNY" {
				want = 5000
			}
			assert.Equal(t, want, getUserQuotaFromDB(t, invitee.Id))
		})
	}
	assert.Equal(t, 10000, getUserQuotaFromDB(t, inviter.Id))
	for _, amount := range []string{"-1", "NaN", "1e100000"} {
		_, err := VerifiedPaymentMajor("USD", amount, "proof")
		assert.Error(t, err)
	}
	fact, err := VerifiedPaymentMajor("USD", "0.001", "proof")
	require.NoError(t, err)
	assert.Equal(t, "0.001", fact.Amount)
	// A rejected registration transaction leaves neither a user nor an invitation.
	user := User{Username: "rolled-back-registration", CommunityInviteCode: code, Status: common.UserStatusEnabled}
	err = DB.Transaction(func(tx *gorm.DB) error {
		if err := user.InsertWithTx(tx, 0); err != nil {
			return err
		}
		return errors.New("provider binding failed")
	})
	require.Error(t, err)
	var count int64
	require.NoError(t, DB.Model(&CommunityReferral{}).Where("invitee_id = ?", user.Id).Count(&count).Error)
	assert.Zero(t, count)
}

func TestCommunityReferralSchemaUpgrade(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "previous-release"}[upgrade], func(t *testing.T) {
			useScheduledChannelDatabase(t)
			tables := []any{&TopUp{}, &CommunityReferralConfig{}, &CommunityReferralCode{}, &CommunityReferral{}}
			t.Cleanup(func() { require.NoError(t, DB.Migrator().DropTable(tables...)) })
			if upgrade {
				// The previous release has the same TopUp shape except these additive proof fields.
				current := reflect.TypeFor[TopUp]()
				fields := []reflect.StructField{}
				for i := range current.NumField() {
					field := current.Field(i)
					if field.Name != "PaymentAmount" && field.Name != "PaymentPositive" {
						fields = append(fields, field)
					}
				}
				previous := reflect.New(reflect.StructOf(fields)).Interface()
				table := DB.NamingStrategy.TableName("TopUp")
				require.NoError(t, DB.Table(table).AutoMigrate(previous))
				require.NoError(t, DB.Table(table).Create(map[string]any{"trade_no": "existing-order", "user_id": 9, "amount": 100, "money": 1, "status": common.TopUpStatusSuccess, "payment_verified": true, "payment_currency": "CNY", "payment_principal_fen": 100}).Error)
			}
			require.NoError(t, DB.AutoMigrate(tables...))
			require.NoError(t, DB.AutoMigrate(tables...))
			if upgrade {
				var old TopUp
				require.NoError(t, DB.Where("trade_no = ?", "existing-order").First(&old).Error)
				assert.Equal(t, int64(100), old.Amount)
				assert.False(t, old.PaymentPositive, "old evidence is never silently upgraded into eligibility")
				assert.Error(t, DB.Create(&TopUp{TradeNo: "existing-order"}).Error, "payment order uniqueness survives upgrade")
			}
			row := CommunityReferral{InviteeID: 1, InviterID: 2, Origin: referralTestOrigin, State: "waiting_topup"}
			require.NoError(t, DB.Create(&row).Error)
			assert.Error(t, DB.Create(&row).Error, "one invitee has exactly one binding and receipt")
		})
	}
}

func TestCommunityReferralVerifiedPaymentIngress(t *testing.T) {
	referralDatabase(t)
	inviter, code := referralInvitation(t)
	for _, kind := range []string{"major-positive", "minor-positive", "major-zero", "minor-zero"} {
		t.Run(kind, func(t *testing.T) {
			invitee := referralNewUser(t, code)
			order := TopUp{UserId: invitee.Id, TradeNo: common.GetRandomString(24), Status: common.TopUpStatusSuccess,
				CreateTime: time.Now().Unix(), CompleteTime: time.Now().Unix(), PaymentProvider: PaymentProviderStripe}
			fact := VerifiedTopUpPayment{Currency: "USD", PrincipalFen: 1, Reference: "ingress-" + order.TradeNo}
			want := 5000
			if kind == "major-positive" || kind == "major-zero" {
				amount := "0.001"
				if kind == "major-zero" {
					amount, want = "0", 0
				}
				var err error
				fact, err = VerifiedPaymentMajor("USD", amount, fact.Reference)
				require.NoError(t, err)
				order.PaymentProvider = PaymentProviderWaffo
			} else if kind == "minor-zero" {
				fact.PrincipalFen, want = 0, 0
			}
			require.NoError(t, DB.Create(&order).Error)
			require.NoError(t, DB.Transaction(func(tx *gorm.DB) error { return creditCompletedTopUp(tx, &order, 50, nil, fact) }))
			require.NoError(t, DB.First(&order, order.Id).Error)
			assert.Equal(t, want > 0, order.PaymentPositive)
			assert.NotEmpty(t, order.PaymentAmount)
			require.NoError(t, creditCommunityReferral(invitee.Id))
			assert.Equal(t, 50+want, getUserQuotaFromDB(t, invitee.Id), "referral eligibility must not change the original top-up credit")
			if want > 0 {
				second := TopUp{UserId: invitee.Id, TradeNo: common.GetRandomString(24), Status: common.TopUpStatusSuccess, PaymentProvider: order.PaymentProvider}
				require.NoError(t, DB.Create(&second).Error)
				assert.Error(t, DB.Transaction(func(tx *gorm.DB) error { return creditCompletedTopUp(tx, &second, 50, nil, fact) }), "one verified provider payment cannot fund multiple orders")
				assert.Equal(t, 50+want, getUserQuotaFromDB(t, invitee.Id))
			}
		})
	}
	assert.Equal(t, 10000, getUserQuotaFromDB(t, inviter.Id))
}

func TestCommunityReferralConcurrentConfigUpdates(t *testing.T) {
	referralDatabase(t)
	inviter, _ := referralInvitation(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, points := range []int64{6000, 7000} {
		wg.Go(func() {
			_, err := UpdateCommunityReferralConfig(referralTestOrigin, inviter.Id, 1, true, points, points)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, ErrWalletConflict)
			conflicts++
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
}
