package common

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

const QXChatPasswordPrefix = "$qxchat$"

var qxchatLegacyHash = regexp.MustCompile(`^([a-fA-F0-9]{32}):([a-fA-F0-9]{128})$`)
var qxchatCurrentHash = regexp.MustCompile(`^scrypt\$v1\$131072\$8\$1\$([a-fA-F0-9]{32})\$([a-fA-F0-9]{128})$`)
var qxchatPasswordSlots = make(chan struct{}, 2)

// Only trusted, live source authentication may create this tagged password.
// In particular, an arbitrary scrypt string in users.password is not accepted.
func ImportQXChatPasswordHash(encoded string) (string, error) {
	if !qxchatLegacyHash.MatchString(encoded) && !qxchatCurrentHash.MatchString(encoded) {
		return "", errors.New("unsupported QXChat password hash")
	}
	return QXChatPasswordPrefix + encoded, nil
}

func validateQXChatPassword(password, encoded string) bool {
	if !utf8.ValidString(password) || len(password) > 512 || len(utf16.Encode([]rune(password))) > 128 {
		return false
	}
	encoded, ok := strings.CutPrefix(encoded, QXChatPasswordPrefix)
	if !ok {
		return false
	}
	parts, cost := qxchatCurrentHash.FindStringSubmatch(encoded), 131072
	if parts == nil {
		parts, cost = qxchatLegacyHash.FindStringSubmatch(encoded), 16384
	}
	if parts == nil {
		return false
	}
	select {
	case qxchatPasswordSlots <- struct{}{}:
		defer func() { <-qxchatPasswordSlots }()
	default:
		return false
	}
	// Node's original implementation supplied the hexadecimal salt as text.
	actual, err := scrypt.Key([]byte(password), []byte(parts[1]), cost, 8, 1, 64)
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(parts[2])
	return err == nil && subtle.ConstantTimeCompare(actual, expected) == 1
}
