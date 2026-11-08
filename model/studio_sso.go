package model

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const studioCodePurpose = "studio_sso_session"

type studioGrant struct {
	communityGrant
	ExpectedUser int `json:"expected_user"`
}

func CreateStudioGrant(identity AuthSessionIdentity, origin, challenge string, expectedUser int) (string, error) {
	if origin == "" || !communityOpaque.MatchString(challenge) || expectedUser != identity.UserID || expectedUser <= 0 || strings.HasPrefix(identity.SessionID, "pat:") {
		return "", ErrAuthFlowInvalid
	}
	payload, err := json.Marshal(studioGrant{communityGrant{identity, origin, challenge}, expectedUser})
	if err != nil {
		return "", err
	}
	var code string
	err = DB.Transaction(func(tx *gorm.DB) error {
		if err := ValidateAuthSessionWithTx(tx, identity); err != nil {
			return err
		}
		var err error
		code, _, err = createAuthFlowWithTx(tx, AuthFlowCreate{Purpose: studioCodePurpose, UserId: identity.UserID, SessionId: identity.SessionID, Payload: string(payload), ExpiresAt: time.Now().Add(2 * time.Minute)})
		return err
	})
	return code, err
}

// Grant consumption and a separate chat session commit together. User and
// source-session locks from validation also serialize per-user session limits.
func ExchangeStudioGrant(code, origin, verifier string, expectedUser int, session *UserSession) (*User, error) {
	if session == nil || expectedUser <= 0 || !communityOpaque.MatchString(code) || !communityVerifier.MatchString(verifier) {
		return nil, ErrAuthFlowInvalid
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	var user User
	deadline := userSessionCacheDeadline()
	_, err := ConsumeAuthFlowWithAction(code, AuthFlowMatch{Purpose: studioCodePurpose}, func(tx *gorm.DB, flow *AuthFlow) error {
		grant, err := validateCommunityGrant(tx, flow, origin)
		if err != nil {
			return err
		}
		var studio studioGrant
		if json.Unmarshal([]byte(flow.Payload), &studio) != nil || studio.ExpectedUser != expectedUser || grant.UserID != expectedUser || subtle.ConstantTimeCompare([]byte(grant.Challenge), []byte(challenge)) != 1 {
			return ErrAuthFlowInvalid
		}
		var active, issued int64
		now := time.Now().Unix()
		if err := tx.Model(&UserSession{}).Where("user_id = ? AND status = ? AND expires_at > ?", expectedUser, UserSessionStatusActive, now).Count(&active).Error; err != nil {
			return err
		}
		if active >= int64(common.UserSessionActiveLimit) {
			return ErrUserSessionLimit
		}
		if err := tx.Model(&UserSession{}).Where("user_id = ? AND created_at > ?", expectedUser, now-common.UserSessionIssuanceWindowSeconds).Count(&issued).Error; err != nil {
			return err
		}
		if issued >= int64(common.UserSessionIssuanceLimit) {
			return ErrUserSessionIssuanceLimit
		}
		session.UserID = grant.UserID
		session.UserAuthVersion = grant.UserAuthVersion
		if err := createUserSessionWithTx(tx, session); err != nil {
			return err
		}
		return tx.First(&user, expectedUser).Error
	})
	if err != nil {
		return nil, err
	}
	if err := publishCreatedUserSession(session, deadline); err != nil {
		_, _ = RevokeUserSession(session.UserID, session.SID, "studio_session_failed")
		return nil, err
	}
	return &user, nil
}
