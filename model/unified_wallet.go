package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const (
	WalletAPI = "api"
	WalletCommunity = "community"
	WalletLegacy = "legacy"
	WalletPaid = "paid"
	WalletCharity = "charity"
)

var (
	ErrWalletUnavailable = errors.New("unified wallet is unavailable")
	ErrWalletInsufficient = errors.New("wallet quota insufficient")
	ErrWalletConflict = errors.New("wallet operation conflicts with its original request")
	ErrWalletDuplicate = errors.New("wallet request was already accepted; check its original result")
	ErrWalletInvariant = errors.New("wallet source balances do not match the account balance")
)

// The flag is deliberately independent of UI settings. Enabling requires a drained
// legacy writer fleet; disabling after activation must not restore legacy writers.
func UnifiedWalletEnabled() bool { return os.Getenv("UNIFIED_WALLET_ENABLED") == "true" }

func UnifiedWalletQuotaPerPoint() (int64, error) {
	rate, err := strconv.ParseInt(os.Getenv("UNIFIED_WALLET_QUOTA_PER_CNY"), 10, 64)
	if err != nil || rate <= 0 || rate > int64(common.MaxWalletQuota) || rate%5000 != 0 {
		return 0, ErrWalletUnavailable
	}
	return rate / 5000, nil
}

type UnifiedWalletAccount struct {
	UserID int `gorm:"primaryKey;autoIncrement:false"`
	LegacyQuota int64 `gorm:"type:bigint;not null"`
	PaidQuota int64 `gorm:"type:bigint;not null"`
	CreatedAt int64 `gorm:"type:bigint;not null"`
}

type UnifiedWalletLot struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	UserID int `gorm:"index:idx_wallet_lot_owner,priority:1;not null"`
	Kind string `gorm:"type:varchar(16);index:idx_wallet_lot_owner,priority:2;not null"`
	Reference string `gorm:"type:varchar(64);uniqueIndex;not null"`
	Source string `gorm:"type:varchar(64);not null"`
	InitialQuota int64 `gorm:"type:bigint;not null"`
	RemainingQuota int64 `gorm:"type:bigint;not null"`
	InitialPoints int64 `gorm:"type:bigint;not null"`
	ExpiresAt int64 `gorm:"type:bigint;not null"`
	CreatedAt int64 `gorm:"type:bigint;not null"`
}

type WalletAllocation struct {
	LotID int64 `json:"lot_id"`
	Kind string `json:"kind"`
	Quota int64 `json:"quota"`
}

type UnifiedWalletOperation struct {
	ID string `gorm:"type:varchar(64);primaryKey"`
	UserID int `gorm:"index;not null"`
	Mode string `gorm:"type:varchar(16);not null"`
	State string `gorm:"type:varchar(16);not null"`
	Quota int64 `gorm:"type:bigint;not null"`
	TokenID int `gorm:"not null"`
	TaskID int64 `gorm:"type:bigint;index;not null"`
	Allocations []WalletAllocation `gorm:"serializer:json;type:text"`
	CreatedAt int64 `gorm:"type:bigint;not null"`
	UpdatedAt int64 `gorm:"type:bigint;not null"`
}

type UnifiedWalletEntry struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	UserID int `gorm:"index;not null"`
	OperationID string `gorm:"type:varchar(64);index;not null"`
	LotID int64 `gorm:"index;not null"`
	Kind string `gorm:"type:varchar(16);not null"`
	Event string `gorm:"type:varchar(32);not null"`
	QuotaDelta int64 `gorm:"type:bigint;not null"`
	CreatedAt int64 `gorm:"type:bigint;not null"`
}

type UnifiedWalletNonce struct {
	ID string `gorm:"type:varchar(64);primaryKey"`
	ExpiresAt int64 `gorm:"type:bigint;index;not null"`
}

type WalletImportLot struct {
	SourceLotID string `json:"source_lot_id"`
	Points int64 `json:"points"`
	ExpiresAt *int64 `json:"expires_at"`
}

type WalletImportRequest struct {
	TransferID string `json:"transfer_id"`
	SourceUserID string `json:"source_user_id"`
	NewAPIUserID int `json:"newapi_user_id"`
	BindingVersion int64 `json:"binding_version"`
	Lots []WalletImportLot `json:"lots"`
}

type UnifiedWalletReceipt struct {
	TransferID string `json:"transfer_id" gorm:"type:varchar(96);primaryKey"`
	SourceUserID string `json:"-" gorm:"type:varchar(64);not null"`
	UserID int `json:"newapi_user_id" gorm:"index;not null"`
	BindingVersion int64 `json:"-" gorm:"type:bigint;not null"`
	Status string `json:"status" gorm:"type:varchar(16);not null"`
	Points int64 `json:"points" gorm:"type:bigint;not null"`
	ExpiredPoints int64 `json:"expired_points" gorm:"type:bigint;not null"`
	Digest string `json:"digest" gorm:"type:varchar(64);not null"`
	CreatedAt int64 `json:"created_at" gorm:"type:bigint;not null"`
}

type UnifiedWalletView struct {
	Enabled bool `json:"enabled"`
	Currency string `json:"currency"`
	PointsPerCNY int `json:"points_per_cny"`
	CharityPoints string `json:"charity_points"`
	PermanentPoints string `json:"permanent_points"`
	LegacyAPIQuota int64 `json:"legacy_api_quota"`
	PaidAPIQuota int64 `json:"paid_api_quota"`
	TotalAPIQuota int64 `json:"total_api_quota"`
	CharityExpiringAt *int64 `json:"charity_expiring_at"`
}

func MigrateUnifiedWallet(db *gorm.DB) error {
	return db.AutoMigrate(&UnifiedWalletAccount{}, &UnifiedWalletLot{}, &UnifiedWalletOperation{}, &UnifiedWalletEntry{}, &UnifiedWalletNonce{}, &UnifiedWalletReceipt{})
}

func WalletReference(parts ...string) string {
	h := sha256.New()
	for _, part := range parts { fmt.Fprintf(h, "%d:%s", len(part), part) }
	return hex.EncodeToString(h.Sum(nil))
}

// Lock the existing user row first, including during lazy classification. This
// serializes new accounts and all payment, API, community and import writers.
func lockUnifiedWallet(tx *gorm.DB, userID int) (*User, *UnifiedWalletAccount, error) {
	var user User
	if err := lockForUpdate(tx).First(&user, userID).Error; err != nil { return nil, nil, err }
	var account UnifiedWalletAccount
	err := lockForUpdate(tx).First(&account, "user_id = ?", userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		account = UnifiedWalletAccount{UserID:userID, LegacyQuota:int64(user.Quota), CreatedAt:time.Now().Unix()}
		err = tx.Create(&account).Error
	}
	if err != nil { return nil, nil, err }
	if account.PaidQuota < 0 || account.LegacyQuota + account.PaidQuota != int64(user.Quota) { return nil, nil, ErrWalletInvariant }
	return &user, &account, nil
}

func saveUnifiedWallet(tx *gorm.DB, user *User, account *UnifiedWalletAccount) error {
	total := account.LegacyQuota + account.PaidQuota
	if account.PaidQuota < 0 || total < -int64(common.MaxWalletQuota) || total > int64(common.MaxWalletQuota) { return ErrWalletQuotaLimitExceeded }
	if err := tx.Save(account).Error; err != nil { return err }
	return tx.Model(user).Update("quota", total).Error
}

func walletEntry(tx *gorm.DB, userID int, operation string, allocation WalletAllocation, event string, delta int64) error {
	return tx.Create(&UnifiedWalletEntry{UserID:userID, OperationID:operation, LotID:allocation.LotID, Kind:allocation.Kind, Event:event, QuotaDelta:delta, CreatedAt:time.Now().Unix()}).Error
}

func takeWalletQuota(tx *gorm.DB, account *UnifiedWalletAccount, operation, mode string, amount int64, requireAvailable bool) ([]WalletAllocation, error) {
	allocations := []WalletAllocation{}
	remaining := amount
	if mode == WalletAPI && account.LegacyQuota > 0 {
		take := min(remaining, account.LegacyQuota)
		account.LegacyQuota -= take
		remaining -= take
		allocations = append(allocations, WalletAllocation{Kind:WalletLegacy, Quota:take})
	}
	kinds := []string{WalletPaid}
	if mode == WalletCommunity { kinds = []string{WalletCharity, WalletPaid} }
	for _, kind := range kinds {
		if remaining == 0 { break }
		var lots []UnifiedWalletLot
		query := tx.Where("user_id = ? AND kind = ? AND remaining_quota > 0", account.UserID, kind)
		if kind == WalletCharity { query = query.Where("expires_at = 0 OR expires_at > ?", time.Now().Unix()) }
		if err := lockForUpdate(query).Order("CASE WHEN expires_at = 0 THEN 1 ELSE 0 END, expires_at, id").Find(&lots).Error; err != nil { return nil, err }
		paidAvailable := max(int64(0), account.PaidQuota + min(int64(0), account.LegacyQuota))
		for i := range lots {
			lot := &lots[i]
			take := min(remaining, lot.RemainingQuota)
			if kind == WalletPaid { take = min(take, paidAvailable) }
			if take == 0 { continue }
			lot.RemainingQuota -= take
			if err := tx.Model(lot).Update("remaining_quota", lot.RemainingQuota).Error; err != nil { return nil, err }
			if kind == WalletPaid { account.PaidQuota -= take; paidAvailable -= take }
			remaining -= take
			allocations = append(allocations, WalletAllocation{LotID:lot.ID, Kind:kind, Quota:take})
			if remaining == 0 { break }
		}
	}
	if remaining > 0 {
		if requireAvailable { return nil, ErrWalletInsufficient }
		// Usage already delivered beyond reservation remains an auditable API debt.
		// It is never manufactured as charity or as verified recharge principal.
		if account.LegacyQuota < -int64(common.MaxWalletQuota)+remaining { return nil, ErrWalletQuotaLimitExceeded }
		account.LegacyQuota -= remaining
		allocations = append(allocations, WalletAllocation{Kind:WalletLegacy, Quota:remaining})
	}
	for _, allocation := range allocations {
		if err := walletEntry(tx, account.UserID, operation, allocation, "debit", -allocation.Quota); err != nil { return nil, err }
	}
	return allocations, nil
}

func restoreWalletQuota(tx *gorm.DB, account *UnifiedWalletAccount, operation string, allocations []WalletAllocation, amount int64) ([]WalletAllocation, error) {
	// Undo the last sources first, so final consumption retains charity priority.
	for i := len(allocations)-1; i >= 0 && amount > 0; i-- {
		allocation := &allocations[i]
		refund := min(amount, allocation.Quota)
		if refund == 0 { continue }
		if allocation.Kind == WalletLegacy { account.LegacyQuota += refund } else {
			var lot UnifiedWalletLot
			if err := lockForUpdate(tx).First(&lot, allocation.LotID).Error; err != nil { return nil, err }
			if lot.UserID != account.UserID || lot.Kind != allocation.Kind || refund > lot.InitialQuota-lot.RemainingQuota { return nil, ErrWalletInvariant }
			if err := tx.Model(&lot).Update("remaining_quota", lot.RemainingQuota+refund).Error; err != nil { return nil, err }
			if allocation.Kind == WalletPaid { account.PaidQuota += refund }
		}
		if err := walletEntry(tx, account.UserID, operation, *allocation, "refund", refund); err != nil { return nil, err }
		allocation.Quota -= refund
		amount -= refund
	}
	if amount != 0 { return nil, ErrWalletInvariant }
	return allocations, nil
}

func BeginWalletOperation(userID int, id, mode string, quota, tokenID int) error {
	if !UnifiedWalletEnabled() { return ErrWalletUnavailable }
	if len(id) != 64 || (mode != WalletAPI && mode != WalletCommunity) || quota < 0 || quota > common.MaxWalletQuota || tokenID < 0 { return ErrWalletConflict }
	return DB.Transaction(func(tx *gorm.DB) error {
		user, account, err := lockUnifiedWallet(tx, userID)
		if err != nil { return err }
		if user.Status != common.UserStatusEnabled { return ErrWalletUnavailable }
		var count int64
		if err := tx.Model(&UnifiedWalletOperation{}).Where("id = ?", id).Count(&count).Error; err != nil { return err }
		if count != 0 { return ErrWalletDuplicate }
		allocations, err := takeWalletQuota(tx, account, id, mode, int64(quota), true)
		if err != nil { return err }
		now := time.Now().Unix()
		op := UnifiedWalletOperation{ID:id, UserID:userID, Mode:mode, State:"reserved", Quota:int64(quota), TokenID:tokenID, Allocations:allocations, CreatedAt:now, UpdatedAt:now}
		if err := adjustWalletToken(tx, &op, int64(quota), true); err != nil { return err }
		if err := tx.Create(&op).Error; err != nil { return err }
		return saveUnifiedWallet(tx, user, account)
	})
}

// ChangeWalletOperation uses an absolute target so callback retries cannot apply
// a second delta. Terminal states cannot reopen an accepted payment intention.
func ChangeWalletOperation(userID int, id string, quota int, state string, requireAvailable bool) error {
 if !UnifiedWalletEnabled() { return ErrWalletUnavailable }
 return DB.Transaction(func(tx *gorm.DB) error {
  _,_,err:=changeWalletOperationTx(tx,userID,id,quota,state,requireAvailable)
  return err
 })
}

func changeWalletOperationTx(tx *gorm.DB,userID int,id string,quota int,state string,requireAvailable bool) (int,bool,error) {
 if quota<0 || quota>common.MaxWalletQuota || (state!="reserved" && state!="settled" && state!="refunded") || (state=="refunded" && quota!=0) { return 0,false,ErrWalletConflict }
 user,account,err:=lockUnifiedWallet(tx,userID)
 if err!=nil { return 0,false,err }
 var op UnifiedWalletOperation
 if err:=lockForUpdate(tx).First(&op,"id = ?",id).Error;err!=nil { return 0,false,err }
 if op.UserID!=userID { return 0,false,ErrWalletConflict }
 if op.State!="reserved" {
  if op.State==state && op.Quota==int64(quota) { return 0,false,nil }
  return 0,false,ErrWalletConflict
 }
 delta:=int64(quota)-op.Quota
 if delta>0 {
  allocations,err:=takeWalletQuota(tx,account,id,op.Mode,delta,requireAvailable)
  if err!=nil { return 0,false,err };op.Allocations=append(op.Allocations,allocations...)
 } else if delta<0 {
  op.Allocations,err=restoreWalletQuota(tx,account,id,op.Allocations,-delta)
  if err!=nil { return 0,false,err }
 }
 if err:=adjustWalletToken(tx,&op,delta,requireAvailable);err!=nil { return 0,false,err }
 op.Quota,op.State,op.UpdatedAt=int64(quota),state,time.Now().Unix()
 if err:=tx.Save(&op).Error;err!=nil { return 0,false,err }
 if err:=saveUnifiedWallet(tx,user,account);err!=nil { return 0,false,err }
 return int(delta),true,nil
}

// The task quota and its source allocation change together, so callback retries
// cannot refund twice or leave a stale marker after a successful debit.
func ChangeWalletTaskQuota(task *Task,quota int,state string) (delta int,applied bool,err error) {
 if !UnifiedWalletEnabled() || task==nil || task.ID==0 || task.PrivateData.WalletOperationID=="" { return 0,false,ErrWalletConflict }
 err=DB.Transaction(func(tx *gorm.DB) error {
  var changeErr error
  delta,applied,changeErr=changeWalletOperationTx(tx,task.UserId,task.PrivateData.WalletOperationID,quota,state,false)
  if changeErr!=nil { return changeErr }
  var stored Task
  if err:=lockForUpdate(tx).First(&stored,task.ID).Error;err!=nil { return err }
  if stored.UserId!=task.UserId || stored.PrivateData.WalletOperationID!=task.PrivateData.WalletOperationID { return ErrWalletConflict }
  return tx.Model(&stored).Update("quota",quota).Error
 })
 if err==nil { task.Quota=quota }
 return
}

// Delivered usage without a reservation still uses one atomic fund/token write.
func ApplyWalletUsage(userID,tokenID int,id,mode string,quota int) error {
 if !UnifiedWalletEnabled() || quota<0 || quota>common.MaxWalletQuota || len(id)!=64 || (mode!=WalletAPI && mode!=WalletCommunity) { return ErrWalletConflict }
 return DB.Transaction(func(tx *gorm.DB) error {
  user,account,err:=lockUnifiedWallet(tx,userID)
  if err!=nil { return err }
  var op UnifiedWalletOperation
  err=tx.First(&op,"id = ?",id).Error
  if err==nil {
   if op.UserID==userID && op.TokenID==tokenID && op.State=="settled" && op.Mode==mode && op.Quota==int64(quota) { return nil };return ErrWalletConflict
  }
  if !errors.Is(err,gorm.ErrRecordNotFound) { return err }
  allocations,err:=takeWalletQuota(tx,account,id,mode,int64(quota),false)
  if err!=nil { return err }
  now:=time.Now().Unix()
  op=UnifiedWalletOperation{ID:id,UserID:userID,Mode:mode,State:"settled",Quota:int64(quota),TokenID:tokenID,Allocations:allocations,CreatedAt:now,UpdatedAt:now}
  if err:=adjustWalletToken(tx,&op,int64(quota),false);err!=nil { return err }
  if err:=tx.Create(&op).Error;err!=nil { return err }
  return saveUnifiedWallet(tx,user,account)
 })
}

func adjustWalletToken(tx *gorm.DB, op *UnifiedWalletOperation, delta int64, requireAvailable bool) error {
	if op.TokenID==0 || delta==0 { return nil }
	var token Token
	err:=lockForUpdate(tx.Unscoped()).First(&token,op.TokenID).Error
	if errors.Is(err,gorm.ErrRecordNotFound) && delta<0 { return nil }
	if err!=nil { return err }
	if token.UserId!=op.UserID { return ErrWalletConflict }
	if delta>0 && requireAvailable && !token.UnlimitedQuota && int64(token.RemainQuota)<delta { return ErrWalletInsufficient }
	return tx.Unscoped().Model(&token).Updates(map[string]any{"remain_quota":gorm.Expr("remain_quota - ?",delta),"used_quota":gorm.Expr("used_quota + ?",delta),"accessed_time":time.Now().Unix()}).Error
}

// Existing non-request credits are deliberately legacy, never verified payment.
func UnifiedWalletDeltaTx(tx *gorm.DB, userID int, delta int64, requireAvailable bool, reference string) error {
	if delta < -int64(common.MaxWalletQuota) || delta > int64(common.MaxWalletQuota) { return ErrWalletQuotaLimitExceeded }
	user, account, err := lockUnifiedWallet(tx, userID)
	if err != nil { return err }
	if delta > 0 {
		if int64(user.Quota) > int64(common.MaxWalletQuota)-delta { return ErrWalletQuotaLimitExceeded }
		account.LegacyQuota += delta
		if err := walletEntry(tx, userID, reference, WalletAllocation{Kind:WalletLegacy}, "credit", delta); err != nil { return err }
	} else if delta < 0 {
		if _, err := takeWalletQuota(tx, account, reference, WalletAPI, -delta, requireAvailable); err != nil { return err }
	}
	return saveUnifiedWallet(tx, user, account)
}

func UnifiedWalletDelta(userID int, delta int64, requireAvailable bool) error {
	return DB.Transaction(func(tx *gorm.DB) error { return UnifiedWalletDeltaTx(tx, userID, delta, requireAvailable, WalletReference("legacy-adjustment", common.NewRequestId())) })
}

func CreditVerifiedPaymentTx(tx *gorm.DB, userID int, quota int, topup *TopUp, updates map[string]any) error {
	user, account, err := lockUnifiedWallet(tx, userID)
	if err != nil { return err }
	if quota <= 0 || int64(user.Quota) > int64(common.MaxWalletQuota)-int64(quota) { return ErrWalletQuotaLimitExceeded }
	if topup.PaymentCurrency != "CNY" || topup.PaymentPrincipalFen <= 0 || !topup.PaymentVerified {
		if err:=UnifiedWalletDeltaTx(tx, userID, int64(quota), false, WalletReference("topup", topup.TradeNo));err!=nil { return err }
		if len(updates)>0 { return tx.Model(user).Updates(updates).Error }
		return nil
	}
	points, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(topup.PaymentPrincipalFen).Mul(decimal.NewFromInt(50)))
	if err != nil { return err }
	lot := UnifiedWalletLot{UserID:userID, Kind:WalletPaid, Reference:WalletReference("topup",topup.TradeNo), Source:topup.PaymentProvider, InitialQuota:int64(quota), RemainingQuota:int64(quota), InitialPoints:int64(points), CreatedAt:time.Now().Unix()}
	if err := tx.Create(&lot).Error; err != nil { return err }
	account.PaidQuota += int64(quota)
	if err := walletEntry(tx, userID, lot.Reference, WalletAllocation{Kind:WalletPaid,LotID:lot.ID}, "payment",int64(quota)); err != nil { return err }
	if err := saveUnifiedWallet(tx,user,account); err != nil { return err }
	if len(updates)>0 { return tx.Model(user).Updates(updates).Error }
	return nil
}

func GetUnifiedWallet(userID int) (*UnifiedWalletView, error) {
	view := &UnifiedWalletView{Enabled:UnifiedWalletEnabled(), Currency:"CNY", PointsPerCNY:5000, CharityPoints:"0", PermanentPoints:"0"}
	if !view.Enabled { return view,nil }
	err := DB.Transaction(func(tx *gorm.DB) error {
		user, account, err := lockUnifiedWallet(tx,userID)
		if err != nil { return err }
		view.LegacyAPIQuota,view.PaidAPIQuota,view.TotalAPIQuota = account.LegacyQuota,account.PaidQuota,int64(user.Quota)
		var lots []UnifiedWalletLot
		if err := tx.Where("user_id = ? AND remaining_quota > 0 AND (expires_at = 0 OR expires_at > ?)",userID,time.Now().Unix()).Order("id").Find(&lots).Error; err != nil { return err }
		charity,paid := decimal.Zero,decimal.Zero
		paidDebt := max(int64(0),-account.LegacyQuota)
		for _,lot := range lots {
			remaining := lot.RemainingQuota
			if lot.Kind == WalletPaid { offset:=min(remaining,paidDebt);remaining-=offset;paidDebt-=offset }
			points:=decimal.NewFromInt(lot.InitialPoints).Mul(decimal.NewFromInt(remaining)).Div(decimal.NewFromInt(lot.InitialQuota))
			if lot.Kind == WalletPaid { paid=paid.Add(points) } else {
				charity=charity.Add(points)
				if lot.ExpiresAt>0 && (view.CharityExpiringAt==nil || lot.ExpiresAt<*view.CharityExpiringAt) { expiry:=lot.ExpiresAt;view.CharityExpiringAt=&expiry }
			}
		}
		view.CharityPoints,view.PermanentPoints=charity.Truncate(6).String(),paid.Truncate(6).String()
		return nil
	})
	return view,err
}

func ImportQXChatWallet(request WalletImportRequest, digest string) (*UnifiedWalletReceipt,error) {
	if !UnifiedWalletEnabled() { return nil,ErrWalletUnavailable }
	if len(request.TransferID)<16 || len(request.TransferID)>96 || len(digest)!=64 || len(request.Lots)==0 || len(request.Lots)>1000 { return nil,ErrWalletConflict }
	rate,err:=UnifiedWalletQuotaPerPoint()
	if err!=nil { return nil,err }
	identity,err:=ResolveQXChatIdentity(request.SourceUserID)
	if err!=nil { return nil,err }
	if identity.UserID!=request.NewAPIUserID || identity.AuthorityVersion!=request.BindingVersion { return nil,ErrWalletConflict }
	var receipt UnifiedWalletReceipt
	err=DB.Transaction(func(tx *gorm.DB) error {
		_,_,err:=lockUnifiedWallet(tx,identity.UserID)
		if err!=nil { return err }
		err=tx.First(&receipt,"transfer_id = ?",request.TransferID).Error
		if err==nil {
			if receipt.UserID!=identity.UserID || receipt.Digest!=digest || receipt.Status!="credited" { return ErrWalletConflict }
			return nil
		}
		if !errors.Is(err,gorm.ErrRecordNotFound) { return err }
		now:=time.Now().Unix()
		receipt=UnifiedWalletReceipt{TransferID:request.TransferID,SourceUserID:request.SourceUserID,UserID:identity.UserID,BindingVersion:request.BindingVersion,Status:"credited",Digest:digest,CreatedAt:now}
		seen:=map[string]bool{}
		for _,source:=range request.Lots {
			if source.Points<=0 || source.Points>int64(common.MaxWalletQuota)/rate || len(source.SourceLotID)==0 || len(source.SourceLotID)>96 || seen[source.SourceLotID] || receipt.Points>int64(common.MaxWalletQuota)-source.Points { return ErrWalletConflict }
			seen[source.SourceLotID]=true
			expires:=int64(0)
			if source.ExpiresAt!=nil { expires=*source.ExpiresAt;if expires<=0 { return ErrWalletConflict } }
			receipt.Points+=source.Points
			lot:=UnifiedWalletLot{UserID:identity.UserID,Kind:WalletCharity,Reference:WalletReference("qxchat",request.TransferID,source.SourceLotID),Source:"qxchat_legacy",InitialQuota:source.Points*rate,RemainingQuota:source.Points*rate,InitialPoints:source.Points,ExpiresAt:expires,CreatedAt:now}
			if expires>0 && expires<=now { lot.RemainingQuota=0;receipt.ExpiredPoints+=source.Points }
			if err:=tx.Create(&lot).Error;err!=nil { return err }
			if err:=walletEntry(tx,identity.UserID,WalletReference("qxchat",request.TransferID),WalletAllocation{Kind:WalletCharity,LotID:lot.ID},"import",lot.RemainingQuota);err!=nil { return err }
		}
		return tx.Create(&receipt).Error
	})
	return &receipt,err
}
