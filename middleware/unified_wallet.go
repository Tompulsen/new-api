package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const MigrationWalletDigestKey = "migration_wallet_digest"

var walletNoncePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var walletRequestPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{16,128}$`)

func walletProofFailure(c *gin.Context) { c.AbortWithStatusJSON(http.StatusUnauthorized,gin.H{"error":gin.H{"message":"Invalid wallet service authorization","type":"wallet_authorization_failed"}}) }

func verifyWalletProof(c *gin.Context, purpose, secret, timestamp, nonce, signature string, prefix []string) (string,error) {
	if !model.UnifiedWalletEnabled() || len(secret)<32 || !walletNoncePattern.MatchString(nonce) { return "",model.ErrWalletUnavailable }
	seconds,err:=strconv.ParseInt(timestamp,10,64)
	now:=time.Now().Unix()
	if err!=nil || seconds<now-60 || seconds>now+60 || strconv.FormatInt(seconds,10)!=timestamp { return "",model.ErrWalletConflict }
	provided,err:=hex.DecodeString(signature)
	if err!=nil || len(provided)!=sha256.Size || hex.EncodeToString(provided)!=signature { return "",model.ErrWalletConflict }
	storage,err:=common.GetBodyStorage(c)
	if err!=nil { return "",err }
	reader,err:=storage.NewReader()
	if err!=nil { return "",err }
	defer reader.Close()
	hash:=sha256.New()
	if _,err=io.Copy(hash,reader);err!=nil { return "",err }
	digest:=hex.EncodeToString(hash.Sum(nil))
	canonical:=strings.Join(append(prefix,digest),"\n")
	mac:=hmac.New(sha256.New,[]byte(secret))
	mac.Write([]byte(canonical))
	if !hmac.Equal(provided,mac.Sum(nil)) { return "",model.ErrWalletConflict }
	// The database uniqueness constraint provides replay protection across replicas.
	proof:=model.UnifiedWalletNonce{ID:model.WalletReference(purpose,nonce),ExpiresAt:seconds+120}
	if err:=model.DB.Create(&proof).Error;err!=nil { return "",model.ErrWalletConflict }
	if err:=model.DB.Where("expires_at < ?",now).Delete(&model.UnifiedWalletNonce{}).Error;err!=nil { common.SysError("wallet nonce cleanup failed") }
	return digest,nil
}

// Runs before body mutation; ownership is checked again against the authenticated
// API token by NewBillingSession. No browser-controlled header selects a wallet.
func CommunityWalletProof() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("X-Community-Signature")=="" {
			for _,header:=range []string{"X-Community-Client","X-Community-User","X-Community-Timestamp","X-Community-Nonce"} { if c.GetHeader(header)!="" { walletProofFailure(c);return } }
			c.Next();return
		}
		defer common.CleanupBodyStorage(c)
		if !strings.HasPrefix(c.Request.URL.Path,"/v1/") && !strings.HasPrefix(c.Request.URL.Path,"/v1beta/") { walletProofFailure(c);return }
		client,userText:=c.GetHeader("X-Community-Client"),c.GetHeader("X-Community-User")
		userID,err:=strconv.Atoi(userText)
		requestID:=c.GetHeader("Idempotency-Key")
		if err!=nil || userID<=0 || strconv.Itoa(userID)!=userText || client=="" || client!=os.Getenv("COMMUNITY_WALLET_CLIENT_ID") || strings.ContainsAny(client,"\r\n") || !walletRequestPattern.MatchString(requestID) || c.Request.Method!=http.MethodPost || c.GetHeader("Content-Encoding")!="" {
			walletProofFailure(c);return
		}
		timestamp,nonce:=c.GetHeader("X-Community-Timestamp"),c.GetHeader("X-Community-Nonce")
		prefix:=[]string{"community-wallet-v1",client,userText,timestamp,nonce,c.Request.Method,c.Request.URL.RequestURI(),requestID}
		if _,err:=verifyWalletProof(c,"community-wallet",os.Getenv("COMMUNITY_WALLET_SECRET"),timestamp,nonce,c.GetHeader("X-Community-Signature"),prefix);err!=nil { walletProofFailure(c);return }
		c.Set(common.CommunityWalletUserKey,userID)
		c.Set(common.CommunityWalletOperationKey,model.WalletReference("community",client,userText,requestID))
		c.Next()
	}
}

func QXChatWalletProof() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer common.CleanupBodyStorage(c)
		c.Request.Body=http.MaxBytesReader(c.Writer,c.Request.Body,1024*1024)
		timestamp,nonce:=c.GetHeader("X-Migration-Timestamp"),c.GetHeader("X-Migration-Nonce")
		prefix:=[]string{"qxchat-wallet-v1",timestamp,nonce,c.Request.Method,c.Request.URL.RequestURI()}
		digest,err:=verifyWalletProof(c,"qxchat-wallet",os.Getenv("QXCHAT_MIGRATION_SECRET"),timestamp,nonce,c.GetHeader("X-Migration-Signature"),prefix)
		if err!=nil { walletProofFailure(c);return }
		c.Set(MigrationWalletDigestKey,digest)
		c.Next()
	}
}

// Proof identifies the billing scope; the API token still authenticates its owner.
func CommunityWalletOwner() gin.HandlerFunc {
 return func(c *gin.Context) {
  if userID:=c.GetInt(common.CommunityWalletUserKey);userID>0 && userID!=c.GetInt("id") { walletProofFailure(c);return }
  c.Next()
 }
}
