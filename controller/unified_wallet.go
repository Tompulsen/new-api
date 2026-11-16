package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func RegisterUnifiedWalletRoutes(api *gin.RouterGroup) {
	api.GET("/user/self/wallet", middleware.UserAuth(), middleware.DisableCache(), getUnifiedWallet)
	api.POST("/qxchat/wallet/import", middleware.CommunitySSOExchangeRateLimit(), middleware.QXChatWalletProof(), importQXChatWallet)
	api.GET("/qxchat/wallet/receipts/:transfer_id", middleware.CommunitySSOExchangeRateLimit(), middleware.QXChatWalletProof(), getQXChatWalletReceipt)
}

func walletAPIError(c *gin.Context, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, model.ErrWalletConflict) || errors.Is(err, model.ErrWalletDuplicate) {
		status = http.StatusConflict
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		status = http.StatusNotFound
	}
	c.JSON(status, gin.H{"success": false, "message": "Wallet operation could not be completed", "code": "WALLET_OPERATION_FAILED"})
}

func getUnifiedWallet(c *gin.Context) {
	view, err := model.GetUnifiedWallet(c.GetInt("id"))
	if err != nil {
		walletAPIError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": view})
}

func importQXChatWallet(c *gin.Context) {
	var request model.WalletImportRequest
	if err := common.UnmarshalBodyReusable(c, &request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "INVALID_IMPORT"})
		return
	}
	receipt, err := model.ImportQXChatWallet(request, c.GetString(middleware.MigrationWalletDigestKey))
	if err != nil {
		walletAPIError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": receipt})
}

func getQXChatWalletReceipt(c *gin.Context) {
	var receipt model.UnifiedWalletReceipt
	if err := model.DB.First(&receipt, "transfer_id = ?", c.Param("transfer_id")).Error; err != nil {
		walletAPIError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": receipt})
}
