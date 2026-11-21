package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func RegisterUnifiedWalletRoutes(api *gin.RouterGroup) {
	api.POST("/qxchat/wallet/reservations/status", middleware.QXChatWalletProof(), getQXChatWalletReservationStatus)
	api.GET("/user/self/wallet", middleware.UserAuth(), middleware.DisableCache(), getUnifiedWallet)
	api.GET("/user/self/wallet/lots", middleware.UserAuth(), middleware.DisableCache(), getUnifiedWalletLots)
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

func getUnifiedWalletLots(c *gin.Context) {
	var before int64
	if value, present := c.GetQuery("before"); present {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 || parsed > int64(common.MaxWalletQuota) || strconv.FormatInt(parsed, 10) != value {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "INVALID_CURSOR"})
			return
		}
		before = parsed
	}
	view, err := model.GetUnifiedWalletLots(c.GetInt("id"), before)
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

func getQXChatWalletReservationStatus(c *gin.Context) {
	var request model.QXChatWalletReservationStatus
	if err := common.UnmarshalBodyReusable(c, &request); err != nil {
		walletAPIError(c, model.ErrWalletConflict)
		return
	}
	result, err := model.GetQXChatWalletReservationStatus(request)
	if err != nil {
		walletAPIError(c, err)
		return
	}
	body, err := common.Marshal(result)
	if err != nil {
		walletAPIError(c, err)
		return
	}
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{"qxchat-wallet-response-v1", c.GetHeader("X-Migration-Nonce"), "200", hex.EncodeToString(digest[:])}, "\n")
	c.Header("X-Migration-Signature", common.GenerateHMACWithKey([]byte(os.Getenv("QXCHAT_MIGRATION_SECRET")), canonical))
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", body)
}
