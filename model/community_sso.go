package model

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

const communityCodePurpose = "community_sso_code"
const communityIdentityPurpose = "community_sso_identity"

var communityVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
var communityOpaque = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type communityGrant struct {
	AuthSessionIdentity
	Origin    string `json:"origin"`
	Challenge string `json:"challenge"`
}

type CommunityIdentity struct {
	Subject  string `json:"sub"`
	Username string `json:"preferred_username"`
	Name     string `json:"name"`
}

// A community grant is an identity-only capability, never a model or dashboard token.
func CreateCommunityGrant(identity AuthSessionIdentity, origin, challenge string) (string, error) {
	if origin == "" || !communityOpaque.MatchString(challenge) || strings.HasPrefix(identity.SessionID, "pat:") {
		return "", ErrAuthFlowInvalid
	}
	payload, err := json.Marshal(communityGrant{identity, origin, challenge})
	if err != nil {
		return "", err
	}
	var code string
	err = DB.Transaction(func(tx *gorm.DB) error {
		if err := ValidateAuthSessionWithTx(tx, identity); err != nil {
			return err
		}
		var err error
		code, _, err = createAuthFlowWithTx(tx, AuthFlowCreate{
			Purpose: communityCodePurpose, UserId: identity.UserID, SessionId: identity.SessionID,
			Payload: string(payload), ExpiresAt: time.Now().Add(2 * time.Minute),
		})
		return err
	})
	return code, err
}

func validateCommunityGrant(tx *gorm.DB, flow *AuthFlow, origin string) (*communityGrant, error) {
	var grant communityGrant
	if json.Unmarshal([]byte(flow.Payload), &grant) != nil || grant.Origin != origin || origin == "" ||
		grant.UserID != flow.UserId || grant.SessionID != flow.SessionId || strings.HasPrefix(grant.SessionID, "pat:") {
		return nil, ErrAuthFlowInvalid
	}
	if err := ValidateAuthSessionWithTx(tx, grant.AuthSessionIdentity); err != nil {
		return nil, err
	}
	return &grant, nil
}

func ExchangeCommunityGrant(code, origin, verifier string) (string, error) {
	if !communityOpaque.MatchString(code) || !communityVerifier.MatchString(verifier) {
		return "", ErrAuthFlowInvalid
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	var token string
	_, err := ConsumeAuthFlowWithAction(code, AuthFlowMatch{Purpose: communityCodePurpose}, func(tx *gorm.DB, flow *AuthFlow) error {
		grant, err := validateCommunityGrant(tx, flow, origin)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(grant.Challenge), []byte(challenge)) != 1 {
			return ErrAuthFlowInvalid
		}
		token, _, err = createAuthFlowWithTx(tx, AuthFlowCreate{
			Purpose: communityIdentityPurpose, UserId: flow.UserId, SessionId: flow.SessionId,
			Payload: flow.Payload, ExpiresAt: time.Now().Add(time.Minute),
		})
		return err
	})
	return token, err
}

func ConsumeCommunityIdentity(token, origin string) (*CommunityIdentity, error) {
	if !communityOpaque.MatchString(token) {
		return nil, ErrAuthFlowInvalid
	}
	var identity CommunityIdentity
	_, err := ConsumeAuthFlowWithAction(token, AuthFlowMatch{Purpose: communityIdentityPurpose}, func(tx *gorm.DB, flow *AuthFlow) error {
		if _, err := validateCommunityGrant(tx, flow, origin); err != nil {
			return err
		}
		var user User
		if err := tx.First(&user, flow.UserId).Error; err != nil {
			return err
		}
		identity = CommunityIdentity{Subject: strconv.Itoa(user.Id), Username: user.Username, Name: user.DisplayName}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &identity, nil
}
