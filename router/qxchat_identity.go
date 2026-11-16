package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func RegisterQXChatIdentityRoutes(api *gin.RouterGroup) {
	api.POST("/qxchat/session/exchange", middleware.CriticalRateLimit(), middleware.DisableCache(), middleware.AnonymousRequestBodyLimit(), controller.QXChatSessionExchange)
	api.POST("/qxchat/session/validate", middleware.DisableCache(), middleware.AnonymousRequestBodyLimit(), controller.QXChatSessionValidate)
	api.GET("/user/qxchat/config", middleware.DisableCache(), controller.QXChatIdentityConfig)
	api.GET("/user/qxchat", middleware.UserAuth(), middleware.DisableCache(), controller.QXChatIdentityStatus)
	api.POST("/user/login/qxchat", middleware.CriticalRateLimit(), middleware.DisableCache(), middleware.AnonymousRequestBodyLimit(), middleware.TurnstileCheck(), controller.QXChatIdentityLogin)
	api.POST("/user/qxchat/bind", middleware.UserAuth(), middleware.UserCriticalRateLimit("account-security"), middleware.CriticalRateLimit(), middleware.DisableCache(), middleware.AnonymousRequestBodyLimit(), controller.QXChatIdentityBind)
}
