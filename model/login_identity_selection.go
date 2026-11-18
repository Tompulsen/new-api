package model

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const AuthFlowPurposeLoginIdentity = "login_identity"
const LoginIdentitySelectionTTL = 5 * time.Minute

type LoginIdentityCandidate struct {
	SelectionID  string `json:"selection_id"`
	Username     string `json:"username"`
	AccountLabel string `json:"account_label"`
}

type LoginIdentitySelection struct {
	RequiresIdentitySelection bool                     `json:"requires_identity_selection"`
	FlowToken                 string                   `json:"flow_token"`
	ExpiresAt                 int64                    `json:"expires_at"`
	Candidates                []LoginIdentityCandidate `json:"candidates"`
}

type loginIdentityProof struct {
	SelectionID    string `json:"selection_id"`
	UserID         int    `json:"user_id"`
	AuthVersion    int64  `json:"auth_version"`
	PasswordDigest string `json:"password_digest"`
}

type loginIdentityPayload struct {
	Identifier string               `json:"identifier"`
	Origin     string               `json:"origin"`
	Proofs     []loginIdentityProof `json:"proofs"`
}

func loginPasswordDigest(passwordHash string) string {
	return common.GenerateHMACWithKey([]byte("login-identity-password-v1:"+common.SessionSecret), passwordHash)
}

// AuthenticatePasswordIdentifier never chooses between multiple identities by
// password. Even a single successful password needs an explicit confirmation
// when the supplied name belongs to several accounts.
func AuthenticatePasswordIdentifier(identifier, password, origin string) (*User, *LoginIdentitySelection, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" || password == "" {
		return nil, nil, ErrUserEmptyCredentials
	}
	if len(identifier) > 255 || len(password) > 512 || len(origin) > 512 {
		return nil, nil, ErrInvalidCredentials
	}
	users, err := passwordLoginCandidates(DB, identifier)
	if err != nil {
		return nil, nil, err
	}
	valid := make([]User, 0, len(users))
	for _, user := range users {
		if passwordLoginAllowed(user, password) {
			valid = append(valid, user)
		}
	}
	if len(valid) == 0 {
		return nil, nil, ErrInvalidCredentials
	}
	if len(users) == 1 {
		return &valid[0], nil, nil
	}
	payload := loginIdentityPayload{Identifier: identifier, Origin: origin}
	result := &LoginIdentitySelection{RequiresIdentitySelection: true}
	for _, user := range valid {
		if user.AuthVersion <= 0 {
			return nil, nil, ErrInvalidCredentials
		}
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, nil, err
		}
		selectionID := base64.RawURLEncoding.EncodeToString(random)
		payload.Proofs = append(payload.Proofs, loginIdentityProof{SelectionID: selectionID, UserID: user.Id,
			AuthVersion: user.AuthVersion, PasswordDigest: loginPasswordDigest(user.Password)})
		label := fmt.Sprintf("独立资料 · 账号 #%d", user.Id)
		var count int64
		if err := DB.Model(&QXChatIdentity{}).Where("user_id = ? AND state = ? AND source_email <> ?", user.Id, QXChatIdentityActive, "").Count(&count).Error; err != nil {
			return nil, nil, err
		}
		if count > 0 {
			label = fmt.Sprintf("原 QX 资料 · 账号 #%d", user.Id)
		}
		result.Candidates = append(result.Candidates, LoginIdentityCandidate{SelectionID: selectionID, Username: user.Username, AccountLabel: label})
	}
	encoded, err := common.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	expires := time.Now().Add(LoginIdentitySelectionTTL)
	token, _, err := CreateAuthFlow(AuthFlowCreate{Purpose: AuthFlowPurposeLoginIdentity, Intent: AuthFlowIntentLogin,
		Payload: string(encoded), ExpiresAt: expires})
	if err != nil {
		return nil, nil, err
	}
	result.FlowToken, result.ExpiresAt = token, expires.Unix()
	return nil, result, nil
}

func ConfirmPasswordLoginIdentity(token, selectionID, origin string) (*User, error) {
	if len(token) != 43 || len(selectionID) != 22 || len(origin) > 512 {
		return nil, ErrInvalidCredentials
	}
	var selected User
	_, err := ConsumeAuthFlowWithAction(token, AuthFlowMatch{Purpose: AuthFlowPurposeLoginIdentity, Intent: AuthFlowIntentLogin}, func(tx *gorm.DB, flow *AuthFlow) error {
		var payload loginIdentityPayload
		if common.UnmarshalJsonStr(flow.Payload, &payload) != nil || payload.Origin != origin ||
			payload.Identifier == "" || len(payload.Proofs) == 0 || len(payload.Proofs) > MaxPasswordLoginCandidates {
			return ErrInvalidCredentials
		}
		var proof *loginIdentityProof
		for i := range payload.Proofs {
			if payload.Proofs[i].SelectionID == selectionID {
				proof = &payload.Proofs[i]
				break
			}
		}
		if proof == nil || proof.UserID <= 0 || proof.AuthVersion <= 0 || len(proof.PasswordDigest) != 64 {
			return ErrInvalidCredentials
		}
		if err := lockForUpdate(tx).First(&selected, proof.UserID).Error; err != nil {
			return ErrInvalidCredentials
		}
		if selected.Status != common.UserStatusEnabled || selected.AuthVersion != proof.AuthVersion ||
			loginPasswordDigest(selected.Password) != proof.PasswordDigest || strings.HasPrefix(selected.Password, QXChatImportPasswordPrefix) {
			return ErrInvalidCredentials
		}
		current, err := passwordLoginCandidates(tx, payload.Identifier)
		if err != nil {
			return err
		}
		for _, user := range current {
			if user.Id == selected.Id {
				return nil
			}
		}
		return ErrInvalidCredentials
	})
	if err != nil {
		if errors.Is(err, ErrAuthFlowInvalid) || errors.Is(err, ErrAuthFlowExpired) || errors.Is(err, ErrAuthFlowConsumed) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	return &selected, nil
}
