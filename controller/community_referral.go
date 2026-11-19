package controller

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// The community server enforces its user/admin permissions before this private service call.
func CommunityReferral(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	secret, origin := os.Getenv("COMMUNITY_LOTTERY_SECRET"), communitySSOOrigin()
	if len(secret) < 32 || origin == "" || !model.UnifiedWalletEnabled() {
		communitySSOError(c, http.StatusServiceUnavailable, "referral_unavailable")
		return
	}
	want := sha256.Sum256([]byte("Bearer " + secret))
	got := sha256.Sum256([]byte(c.GetHeader("Authorization")))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		communitySSOError(c, http.StatusUnauthorized, "invalid_referral_credential")
		return
	}
	var request struct {
		ClientID      string `json:"client_id"`
		Action        string `json:"action"`
		UserID        int    `json:"user_id"`
		Cursor        int    `json:"cursor"`
		Version       int64  `json:"version"`
		Enabled       *bool  `json:"enabled"`
		InviterPoints int64  `json:"inviterPoints"`
		InviteePoints int64  `json:"inviteePoints"`
	}
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") || common.DecodeJson(c.Request.Body, &request) != nil || request.ClientID != origin || request.UserID < 0 || request.Cursor < 0 {
		communitySSOError(c, http.StatusBadRequest, "invalid_request")
		return
	}
	var result any
	var err error
	switch request.Action {
	case "status":
		result, err = model.GetCommunityReferralOverview(origin, request.UserID, request.Cursor)
	case "config":
		result, err = model.GetCommunityReferralConfig(origin)
	case "configure":
		if request.Enabled == nil || request.UserID <= 0 {
			communitySSOError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		result, err = model.UpdateCommunityReferralConfig(origin, request.UserID, request.Version, *request.Enabled, request.InviterPoints, request.InviteePoints)
	default:
		communitySSOError(c, http.StatusBadRequest, "invalid_request")
		return
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, model.ErrWalletConflict) {
			status = http.StatusConflict
		} else if errors.Is(err, model.ErrInvalidTopUpQuota) {
			status = http.StatusBadRequest
		}
		communitySSOError(c, status, "referral_request_failed")
		return
	}
	c.JSON(http.StatusOK, result)
}
