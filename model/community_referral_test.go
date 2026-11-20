package model

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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
	oldNewUser, oldInviter, oldInvitee := common.QuotaForNewUser, common.QuotaForInviter, common.QuotaForInvitee
	oldMinimum := common.ReferralMinTopUpCNY
	common.QuotaForNewUser, common.QuotaForInviter, common.QuotaForInvitee = 0, 5000, 5000
	common.ReferralMinTopUpCNY = "0"
	payment := operation_setting.GetPaymentSetting()
	oldPayment := *payment
	payment.ComplianceConfirmed, payment.ComplianceTermsVersion = true, operation_setting.CurrentComplianceTermsVersion
	t.Cleanup(func() {
		common.QuotaForNewUser, common.QuotaForInviter, common.QuotaForInvitee = oldNewUser, oldInviter, oldInvitee
		common.ReferralMinTopUpCNY = oldMinimum
		*payment = oldPayment
	})
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
	inviter.AffCode = common.GetRandomString(4)
	require.NoError(t, DB.Model(&inviter).Update("aff_code", inviter.AffCode).Error)
	view, err := GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
	require.NoError(t, err)
	assert.Equal(t, "5000", fmt.Sprint(view.Config.InviterPoints))
	assert.Equal(t, "5000", fmt.Sprint(view.Config.InviteePoints))
	assert.Equal(t, inviter.AffCode, view.Code, "both domains expose the account's existing invitation code")
	again, err := GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
	require.NoError(t, err)
	assert.Equal(t, view.Code, again.Code)
	return inviter, inviter.AffCode
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
	common.QuotaForInviter, common.QuotaForInvitee, common.ReferralMinTopUpCNY = 8000, 9000, "2"
	newer := referralNewUser(t, code)
	var snapshot CommunityReferral
	require.NoError(t, DB.First(&snapshot, "invitee_id = ?", newer.Id).Error)
	assert.Equal(t, int64(8000), snapshot.InviterQuota)
	assert.Equal(t, int64(9000), snapshot.InviteeQuota)
	assert.Equal(t, int64(200), snapshot.MinTopUpFen)
	common.QuotaForInviter, common.QuotaForInvitee = 0, 0
	disabled := referralNewUser(t, code)
	var count int64
	require.NoError(t, DB.Model(&CommunityReferral{}).Where("invitee_id = ?", disabled.Id).Count(&count).Error)
	assert.Zero(t, count, "zero rewards stop new reward entitlements")
	require.NoError(t, DB.First(&disabled, disabled.Id).Error)
	assert.Equal(t, inviter.Id, disabled.InviterId, "attribution survives disabled rewards")
	referralPaidOrder(t, invitee)
	paidQuota := getUserQuotaFromDB(t, invitee.Id)
	require.NoError(t, creditCommunityReferral(invitee.Id))
	assert.Equal(t, 5000, getUserQuotaFromDB(t, inviter.Id))
	assert.Equal(t, paidQuota+5000, getUserQuotaFromDB(t, invitee.Id))
	view, err := GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), view.Credited)
	assert.Equal(t, "5000", fmt.Sprint(view.Earned))
	var frozen CommunityReferral
	require.NoError(t, DB.First(&frozen, "invitee_id = ?", invitee.Id).Error)
	assert.Zero(t, frozen.ConfigVersion)
	assert.Zero(t, frozen.MinTopUpFen)
	assert.Equal(t, int64(5000), frozen.InviterQuota)
	assert.Equal(t, int64(5000), frozen.InviteeQuota)
	assert.Equal(t, "credited", frozen.State)
	// Updating or visiting an existing account never calls the registration binder.
	old := createReserveTestUser(t, 0)
	old.CommunityInviteCode = code
	require.NoError(t, DB.Model(&old).Update("display_name", "Existing account").Error)
	require.NoError(t, DB.Model(&CommunityReferral{}).Where("invitee_id = ?", old.Id).Count(&count).Error)
	assert.Zero(t, count)
	common.ReferralMinTopUpCNY = "0"
	referralPaidOrder(t, newer)
	newerPaidQuota := getUserQuotaFromDB(t, newer.Id)
	require.NoError(t, creditCommunityReferral(newer.Id))
	assert.Equal(t, newerPaidQuota, getUserQuotaFromDB(t, newer.Id), "lowering the current threshold never changes a bound entitlement")
	order := TopUp{UserId: newer.Id, TradeNo: common.GetRandomString(24), Status: common.TopUpStatusSuccess,
		CreateTime: snapshot.BoundAt, CompleteTime: snapshot.BoundAt, PaymentProvider: PaymentProviderStripe,
		PaymentCurrency: "CNY", PaymentReference: common.GetRandomString(24), PaymentVerified: true,
		PaymentPositive: true, PaymentPrincipalFen: 200}
	require.NoError(t, DB.Create(&order).Error)
	require.NoError(t, creditCommunityReferral(newer.Id))
	assert.Equal(t, 13000, getUserQuotaFromDB(t, inviter.Id))
	assert.Equal(t, newerPaidQuota+9000, getUserQuotaFromDB(t, newer.Id))
}

func TestCommunityReferralUnifiedNativeRegistration(t *testing.T) {
	referralDatabase(t)
	inviter, code := referralInvitation(t)
	invitee := referralNewUser(t, code)
	invitee.FinishInsert(inviter.Id)
	invitee.FinalizeOAuthUserCreation(inviter.Id)
	var stored User
	require.NoError(t, DB.First(&stored, invitee.Id).Error)
	assert.Equal(t, inviter.Id, stored.InviterId)
	var referral CommunityReferral
	require.NoError(t, DB.First(&referral, "invitee_id = ?", invitee.Id).Error)
	assert.Equal(t, inviter.Id, referral.InviterID)
	assert.Zero(t, referral.ConfigVersion)
	assert.Equal(t, int64(5000), referral.InviterQuota)
	assert.Equal(t, int64(5000), referral.InviteeQuota)
	assert.Zero(t, getUserQuotaFromDB(t, inviter.Id))
	assert.Zero(t, getUserQuotaFromDB(t, invitee.Id))
}

func TestCommunityReferralInvitationCodeForExistingAccounts(t *testing.T) {
	referralDatabase(t)
	for _, test := range []struct {
		name, code string
		nullCode   bool
	}{
		{name: "empty-root"},
		{name: "null-historical", code: "before-null", nullCode: true},
		{name: "existing-code", code: "stable-existing-code"},
	} {
		t.Run(test.name, func(t *testing.T) {
			user := User{Username: "aff-" + test.name, Password: "unchanged-root-hash", Role: common.RoleRootUser,
				Status: common.UserStatusEnabled, AffCode: test.code, Quota: 123, AffCount: 2, AffQuota: 7, AffHistoryQuota: 11, AuthVersion: 1}
			require.NoError(t, DB.Create(&user).Error)
			if test.nullCode {
				require.NoError(t, DB.Model(&user).Update("aff_code", nil).Error)
			}
			type codeResult struct {
				code string
				err  error
			}
			results := make(chan codeResult, 4)
			var wg sync.WaitGroup
			for i := range 4 {
				wg.Go(func() {
					if i%2 == 0 {
						pending := User{Id: user.Id}
						err := pending.EnsureAffCode()
						results <- codeResult{code: pending.AffCode, err: err}
						return
					}
					view, err := GetCommunityReferralOverview(referralTestOrigin, user.Id, 0)
					result := codeResult{err: err}
					if view != nil {
						result.code = view.Code
					}
					results <- result
				})
			}
			wg.Wait()
			close(results)
			var stored User
			require.NoError(t, DB.First(&stored, user.Id).Error)
			require.NotEmpty(t, stored.AffCode)
			if test.code != "" && !test.nullCode {
				assert.Equal(t, test.code, stored.AffCode)
			} else {
				assert.Len(t, stored.AffCode, 4)
			}
			for result := range results {
				require.NoError(t, result.err)
				assert.Equal(t, stored.AffCode, result.code, "concurrent reads return the persisted invitation code")
			}
			again, err := GetCommunityReferralOverview("https://other-community.test", user.Id, 0)
			require.NoError(t, err)
			assert.Equal(t, stored.AffCode, again.Code)
			inviterID, err := GetUserIdByAffCode(stored.AffCode)
			require.NoError(t, err)
			assert.Equal(t, user.Id, inviterID)
			assert.Equal(t, user.Password, stored.Password)
			assert.Equal(t, user.Quota, stored.Quota)
			assert.Equal(t, user.AffCount, stored.AffCount)
			assert.Equal(t, user.AffQuota, stored.AffQuota)
			assert.Equal(t, user.AffHistoryQuota, stored.AffHistoryQuota)
			assert.Equal(t, user.AuthVersion, stored.AuthVersion)
		})
	}
}

func TestCommunityReferralPaymentGatePreservesAttribution(t *testing.T) {
	referralDatabase(t)
	inviter, code := referralInvitation(t)
	operation_setting.GetPaymentSetting().ComplianceConfirmed = false
	invitee := referralNewUser(t, code)
	require.NoError(t, DB.First(&invitee, invitee.Id).Error)
	assert.Equal(t, inviter.Id, invitee.InviterId)
	var count int64
	require.NoError(t, DB.Model(&CommunityReferral{}).Where("invitee_id = ?", invitee.Id).Count(&count).Error)
	assert.Zero(t, count)
	view, err := GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
	require.NoError(t, err)
	assert.False(t, view.Config.Enabled)
}

func TestCommunityReferralMinimumAmountValidationAndPrecision(t *testing.T) {
	referralDatabase(t)
	for _, input := range []string{"0", "0.01", "0.29", "10.29", "100000000"} {
		t.Run("accept-"+input, func(t *testing.T) {
			require.NoError(t, validateOptionValue("ReferralMinTopUpCNY", input))
		})
	}
	for _, input := range []string{"-1", "NaN", "Inf", "0.001", "100000000.01", "1e10", ""} {
		t.Run("reject-"+input, func(t *testing.T) {
			require.Error(t, validateOptionValue("ReferralMinTopUpCNY", input))
		})
	}
	_, code := referralInvitation(t)
	for _, test := range []struct {
		input string
		fen   int64
	}{{input: "0.29", fen: 29}, {input: "10.29", fen: 1029}} {
		t.Run("snapshot-"+test.input, func(t *testing.T) {
			common.ReferralMinTopUpCNY = test.input
			config, err := GetUnifiedReferralConfig()
			require.NoError(t, err)
			assert.Equal(t, test.fen, config.MinTopUpFen)
			invitee := referralNewUser(t, code)
			var referral CommunityReferral
			require.NoError(t, DB.First(&referral, "invitee_id = ?", invitee.Id).Error)
			assert.Equal(t, test.fen, referral.MinTopUpFen)
		})
	}
}

func TestCommunityReferralSinglePaymentThreshold(t *testing.T) {
	referralDatabase(t)
	inviter, code := referralInvitation(t)
	common.ReferralMinTopUpCNY = "1.50"
	for _, test := range []struct {
		name      string
		currency  string
		principal int64
		refunded  int64
		qualifies bool
	}{
		{name: "exact", currency: "CNY", principal: 150, qualifies: true},
		{name: "below", currency: "CNY", principal: 149},
		{name: "net-exact", currency: "CNY", principal: 200, refunded: 50, qualifies: true},
		{name: "net-below", currency: "CNY", principal: 200, refunded: 51},
		{name: "fully-refunded", currency: "CNY", principal: 200, refunded: 200},
		{name: "other-currency", currency: "USD", principal: 10000},
	} {
		t.Run(test.name, func(t *testing.T) {
			invitee := referralNewUser(t, code)
			var referral CommunityReferral
			require.NoError(t, DB.First(&referral, "invitee_id = ?", invitee.Id).Error)
			assert.Equal(t, int64(150), referral.MinTopUpFen)
			order := TopUp{UserId: invitee.Id, TradeNo: common.GetRandomString(24), Status: common.TopUpStatusSuccess,
				CreateTime: referral.BoundAt, CompleteTime: referral.BoundAt, PaymentProvider: PaymentProviderStripe,
				PaymentCurrency: test.currency, PaymentReference: common.GetRandomString(24), PaymentVerified: true,
				PaymentPositive: true, PaymentPrincipalFen: test.principal, PaymentRefundedFen: test.refunded}
			require.NoError(t, DB.Create(&order).Error)
			require.NoError(t, creditCommunityReferral(invitee.Id))
			want := 0
			if test.qualifies {
				want = 5000
			}
			assert.Equal(t, want, getUserQuotaFromDB(t, invitee.Id))
		})
	}
	assert.Equal(t, 10000, getUserQuotaFromDB(t, inviter.Id))
	t.Run("later-single-payment", func(t *testing.T) {
		invitee := referralNewUser(t, code)
		var referral CommunityReferral
		require.NoError(t, DB.First(&referral, "invitee_id = ?", invitee.Id).Error)
		var qualifying TopUp
		for i, principal := range []int64{100, 100, 150} {
			order := TopUp{UserId: invitee.Id, TradeNo: common.GetRandomString(24), Status: common.TopUpStatusSuccess,
				CreateTime: referral.BoundAt, CompleteTime: referral.BoundAt, PaymentProvider: PaymentProviderStripe,
				PaymentCurrency: "CNY", PaymentReference: common.GetRandomString(24), PaymentVerified: true,
				PaymentPositive: true, PaymentPrincipalFen: principal}
			require.NoError(t, DB.Create(&order).Error)
			require.NoError(t, creditCommunityReferral(invitee.Id))
			want := 0
			if i == 2 {
				want, qualifying = 5000, order
			}
			assert.Equal(t, want, getUserQuotaFromDB(t, invitee.Id), "separate sub-threshold payments never accumulate")
		}
		require.NoError(t, DB.First(&referral, "invitee_id = ?", invitee.Id).Error)
		assert.Equal(t, qualifying.Id, referral.TopUpID)
		require.NoError(t, creditCommunityReferral(invitee.Id))
		assert.Equal(t, 5000, getUserQuotaFromDB(t, invitee.Id))
	})
}

func TestCommunityReferralZeroRewardsAndFractionalPoints(t *testing.T) {
	for _, test := range []struct {
		name                         string
		inviter, invitee             int
		inviterPoints, inviteePoints string
	}{
		{name: "inviter-only", inviter: 1, inviterPoints: "0.5", inviteePoints: "0"},
		{name: "invitee-only", invitee: 1, inviterPoints: "0", inviteePoints: "0.5"},
		{name: "both-zero", inviterPoints: "0", inviteePoints: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			referralDatabase(t)
			inviter, code := referralInvitation(t)
			t.Setenv("UNIFIED_WALLET_QUOTA_PER_CNY", "10000")
			common.QuotaForInviter, common.QuotaForInvitee = test.inviter, test.invitee
			invitee := referralNewUser(t, code)
			require.NoError(t, DB.First(&invitee, invitee.Id).Error)
			assert.Equal(t, inviter.Id, invitee.InviterId)
			var count int64
			require.NoError(t, DB.Model(&CommunityReferral{}).Where("invitee_id = ?", invitee.Id).Count(&count).Error)
			view, err := GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
			require.NoError(t, err)
			assert.Equal(t, test.inviterPoints, fmt.Sprint(view.Config.InviterPoints))
			assert.Equal(t, test.inviteePoints, fmt.Sprint(view.Config.InviteePoints))
			if test.inviter+test.invitee == 0 {
				assert.Zero(t, count)
				assert.False(t, view.Config.Enabled)
				return
			}
			assert.Equal(t, int64(1), count)
			referralPaidOrder(t, invitee)
			paidQuota := getUserQuotaFromDB(t, invitee.Id)
			require.NoError(t, creditCommunityReferral(invitee.Id))
			assert.Equal(t, test.inviter, getUserQuotaFromDB(t, inviter.Id))
			assert.Equal(t, paidQuota+test.invitee, getUserQuotaFromDB(t, invitee.Id))
			var entries int64
			require.NoError(t, DB.Model(&UnifiedWalletEntry{}).Where("operation_id = ?", WalletReference("community-referral", strconv.Itoa(invitee.Id))).Count(&entries).Error)
			assert.Equal(t, int64(1), entries, "the zero side has no wallet mutation")
			view, err = GetCommunityReferralOverview(referralTestOrigin, inviter.Id, 0)
			require.NoError(t, err)
			require.Len(t, view.Items, 1)
			assert.Equal(t, test.inviterPoints, fmt.Sprint(view.Items[0].InviterPoints))
			assert.Equal(t, test.inviteePoints, fmt.Sprint(view.Items[0].InviteePoints))
			assert.Equal(t, test.inviterPoints, fmt.Sprint(view.Earned))
		})
	}
}

func TestCommunityReferralLegacyFrozenRewards(t *testing.T) {
	referralDatabase(t)
	inviter, _ := referralInvitation(t)
	invitee := createReserveTestUser(t, 0)
	referral := CommunityReferral{InviteeID: invitee.Id, InviterID: inviter.Id, Origin: referralTestOrigin,
		ConfigVersion: 7, InviterPoints: 700, InviteePoints: 900, InviterQuota: 700, InviteeQuota: 900,
		BoundAt: time.Now().Unix(), State: "pending", Attempts: 2}
	require.NoError(t, DB.Create(&referral).Error)
	common.QuotaForInviter, common.QuotaForInvitee, common.ReferralMinTopUpCNY = 0, 0, "999"
	operation_setting.GetPaymentSetting().ComplianceConfirmed = false
	referralPaidOrder(t, invitee)
	paidQuota := getUserQuotaFromDB(t, invitee.Id)
	require.NoError(t, ProcessCommunityReferralRewards())
	assert.Equal(t, 700, getUserQuotaFromDB(t, inviter.Id))
	assert.Equal(t, paidQuota+900, getUserQuotaFromDB(t, invitee.Id))
	require.NoError(t, DB.First(&referral, "invitee_id = ?", invitee.Id).Error)
	creditedAt, topupID := referral.CreditedAt, referral.TopUpID
	assert.Equal(t, int64(7), referral.ConfigVersion)
	assert.Zero(t, referral.MinTopUpFen)
	assert.Positive(t, creditedAt)
	require.NoError(t, DB.AutoMigrate(&CommunityReferral{}))
	require.NoError(t, DB.AutoMigrate(&CommunityReferral{}))
	require.NoError(t, creditCommunityReferral(invitee.Id))
	require.NoError(t, DB.First(&referral, "invitee_id = ?", invitee.Id).Error)
	assert.Equal(t, creditedAt, referral.CreditedAt)
	assert.Equal(t, topupID, referral.TopUpID)
	assert.Equal(t, 700, getUserQuotaFromDB(t, inviter.Id))
	assert.Equal(t, paidQuota+900, getUserQuotaFromDB(t, invitee.Id))
	var entries int64
	require.NoError(t, DB.Model(&UnifiedWalletEntry{}).Where("operation_id = ?", WalletReference("community-referral", strconv.Itoa(invitee.Id))).Count(&entries).Error)
	assert.Equal(t, int64(2), entries)
}

func TestCommunityReferralSharedAttributionAndLegacyStatistics(t *testing.T) {
	referralDatabase(t)
	inviter, code := referralInvitation(t)
	require.NoError(t, DB.Model(&inviter).Updates(map[string]any{"aff_count": 2, "aff_history": 250, "aff_quota": 75}).Error)
	otherOrigin := "https://other-community.test"
	alias := CommunityReferralCode{Code: "wf" + common.GetRandomString(24), Origin: otherOrigin, UserID: inviter.Id}
	require.NoError(t, DB.Create(&alias).Error)
	native, legacyAlias := referralNewUser(t, code), referralNewUser(t, alias.Code)
	historical := createReserveTestUser(t, 0)
	require.NoError(t, DB.Model(&historical).Update("inviter_id", inviter.Id).Error)
	require.NoError(t, DB.AutoMigrate(&CommunityReferral{}))
	require.NoError(t, DB.AutoMigrate(&CommunityReferral{}))
	var count int64
	require.NoError(t, DB.Model(&CommunityReferral{}).Where("invitee_id = ?", historical.Id).Count(&count).Error)
	assert.Zero(t, count, "historical native invitations are never backfilled as new unpaid rewards")
	for _, invitee := range []User{native, legacyAlias} {
		require.NoError(t, DB.First(&invitee, invitee.Id).Error)
		assert.Equal(t, inviter.Id, invitee.InviterId)
		referralPaidOrder(t, invitee)
		require.NoError(t, creditCommunityReferral(invitee.Id))
		own, err := GetCommunityReferralOverview(otherOrigin, invitee.Id, 0)
		require.NoError(t, err)
		require.NotNil(t, own.AsInvitee, "attribution is visible from every allowed origin")
		assert.Equal(t, inviter.Id, own.AsInvitee.InviterID)
	}
	for _, origin := range []string{referralTestOrigin, otherOrigin} {
		view, err := GetCommunityReferralOverview(origin, inviter.Id, 0)
		require.NoError(t, err)
		assert.Equal(t, code, view.Code)
		assert.Equal(t, int64(4), view.Invited)
		assert.Equal(t, int64(2), view.Credited)
		assert.Equal(t, "10250", fmt.Sprint(view.Earned))
		assert.Len(t, view.Items, 2)
	}
	require.NoError(t, DB.First(&inviter, inviter.Id).Error)
	assert.Equal(t, 2, inviter.AffCount)
	assert.Equal(t, 250, inviter.AffHistoryQuota)
	assert.Equal(t, 75, inviter.AffQuota)
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
				require.NoError(t, DB.AutoMigrate(&TopUp{}, &CommunityReferralConfig{}, &CommunityReferralCode{}))
				// The released referral schema predates only the per-invitation payment threshold.
				current := reflect.TypeFor[CommunityReferral]()
				fields := []reflect.StructField{}
				for i := range current.NumField() {
					field := current.Field(i)
					if field.Name != "MinTopUpFen" {
						fields = append(fields, field)
					}
				}
				previous := reflect.New(reflect.StructOf(fields)).Interface()
				table := DB.NamingStrategy.TableName("CommunityReferral")
				require.NoError(t, DB.Table(table).AutoMigrate(previous))
				for _, old := range []map[string]any{
					{"invitee_id": 11, "inviter_id": 2, "origin": referralTestOrigin, "config_version": 3, "inviter_points": 700, "invitee_points": 900, "inviter_quota": 700, "invitee_quota": 900, "bound_at": 100, "state": "waiting_topup", "credited_at": 0, "next_check_at": 0},
					{"invitee_id": 12, "inviter_id": 2, "origin": referralTestOrigin, "config_version": 3, "inviter_points": 700, "invitee_points": 900, "inviter_quota": 700, "invitee_quota": 900, "bound_at": 100, "state": "credited", "credited_at": 200, "top_up_id": 9, "next_check_at": 0},
				} {
					require.NoError(t, DB.Table(table).Create(old).Error)
				}
				require.NoError(t, DB.Create(&CommunityReferralConfig{Origin: referralTestOrigin, Version: 3, Enabled: true, InviterPoints: 700, InviteePoints: 900, UpdatedAt: 50}).Error)
				require.NoError(t, DB.Create(&CommunityReferralCode{Code: "wf" + common.GetRandomString(24), Origin: referralTestOrigin, UserID: 2}).Error)
				require.NoError(t, DB.Create(&TopUp{TradeNo: "existing-order", UserId: 9, Amount: 100, Money: 1, Status: common.TopUpStatusSuccess, PaymentVerified: true, PaymentPositive: true, PaymentCurrency: "CNY", PaymentPrincipalFen: 100, PaymentAmount: "1"}).Error)
			}
			require.NoError(t, DB.AutoMigrate(tables...))
			require.NoError(t, DB.AutoMigrate(tables...))
			if upgrade {
				var old TopUp
				require.NoError(t, DB.Where("trade_no = ?", "existing-order").First(&old).Error)
				assert.Equal(t, int64(100), old.Amount)
				assert.True(t, old.PaymentPositive)
				assert.Equal(t, "1", old.PaymentAmount)
				assert.Error(t, DB.Create(&TopUp{TradeNo: "existing-order"}).Error, "payment order uniqueness survives upgrade")
				for _, id := range []int{11, 12} {
					var referral CommunityReferral
					require.NoError(t, DB.First(&referral, "invitee_id = ?", id).Error)
					assert.Equal(t, int64(3), referral.ConfigVersion)
					assert.Zero(t, referral.MinTopUpFen)
					assert.Equal(t, int64(700), referral.InviterQuota)
					assert.Equal(t, int64(900), referral.InviteeQuota)
					if id == 11 {
						assert.Equal(t, "waiting_topup", referral.State)
						assert.Zero(t, referral.CreditedAt)
					} else {
						assert.Equal(t, "credited", referral.State)
						assert.Equal(t, int64(200), referral.CreditedAt)
						assert.Equal(t, 9, referral.TopUpID)
					}
				}
				var config CommunityReferralConfig
				require.NoError(t, DB.First(&config, "origin = ?", referralTestOrigin).Error)
				assert.Equal(t, int64(3), config.Version)
				assert.Equal(t, int64(700), config.InviterPoints)
				assert.Equal(t, int64(900), config.InviteePoints)
				var alias CommunityReferralCode
				require.NoError(t, DB.First(&alias, "user_id = ?", 2).Error)
				assert.Error(t, DB.Create(&CommunityReferralCode{Code: common.GetRandomString(26), Origin: referralTestOrigin, UserID: 2}).Error, "legacy alias ownership remains unique")
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
