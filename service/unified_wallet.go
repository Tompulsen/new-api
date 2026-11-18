package service

import (
	"errors"
	"github.com/QuantumNous/new-api/model"
)

// UnifiedWalletFunding owns both fund and token limits in the same transaction.
type UnifiedWalletFunding struct {
	userID      int
	tokenID     int
	operationID string
	mode        string
	consumed    int
	begun       bool
	deferFinal  bool
}

func (w *UnifiedWalletFunding) Source() string { return BillingSourceWallet }

func (w *UnifiedWalletFunding) PreConsume(amount int) error {
	if !w.begun {
		if err := model.BeginWalletOperation(w.userID, w.operationID, w.mode, amount, w.tokenID); err != nil {
			return walletFundingError(err)
		}
		w.begun = true
		w.consumed = amount
		return nil
	}
	return w.reserve(w.consumed+amount, true)
}

func (w *UnifiedWalletFunding) reserve(target int, requireAvailable bool) error {
	if err := model.ChangeWalletOperation(w.userID, w.operationID, target, "reserved", requireAvailable); err != nil {
		return walletFundingError(err)
	}
	w.consumed = target
	return nil
}

func (w *UnifiedWalletFunding) Settle(delta int) error {
	state := "settled"
	if w.deferFinal {
		state = "reserved"
	}
	return model.ChangeWalletOperation(w.userID, w.operationID, w.consumed+delta, state, false)
}

func (w *UnifiedWalletFunding) Refund() error {
	if !w.begun {
		return nil
	}
	return refundWithRetry(func() error { return model.ChangeWalletOperation(w.userID, w.operationID, 0, "refunded", false) })
}

func walletFundingError(err error) error {
	if errors.Is(err, model.ErrWalletInsufficient) {
		return ErrInsufficientWalletQuota
	}
	return err
}

// Realtime responses accumulate within the same original source allocation.
func (s *BillingSession) accumulateWalletUsage(quota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	funding, ok := s.funding.(*UnifiedWalletFunding)
	if !ok || quota < 0 || s.settled || s.refunded {
		return model.ErrWalletConflict
	}
	target := s.walletUsage + quota
	if target < s.walletUsage {
		return model.ErrWalletConflict
	}
	reserved := max(target, s.preConsumedQuota)
	exhausted, err := model.ReserveWalletRealtimeUsage(funding.userID, funding.operationID, reserved, target)
	if err != nil {
		return err
	}
	funding.consumed = reserved
	s.preConsumedQuota = reserved
	s.walletUsage = target
	s.syncRelayInfo()
	if exhausted {
		return ErrInsufficientWalletQuota
	}
	return nil
}
