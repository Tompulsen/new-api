package middleware

import "github.com/gin-gonic/gin"

// All community users exchange codes through the same backend IP. Keep this
// bucket separate from human password/MFA attempts and apply a server budget.
func CommunitySSOExchangeRateLimit() gin.HandlerFunc {
	return rateLimitFactory(600, 60, "COMMUNITY_SSO_EXCHANGE")
}
