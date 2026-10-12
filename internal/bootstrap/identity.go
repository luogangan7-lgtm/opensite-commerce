// Identity composition validates optional offline configuration and opens only
// the dedicated staff auth connection. cmd/api calls it; no handlers or business
// SQL live here. Partial credentials or malformed signing keys fail startup.
package bootstrap

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"time"
)

func StaffLogin(ctx context.Context, databaseURL, origin, keyHex string) (*identity.LoginModule, error) {
	if databaseURL == "" && origin == "" && keyHex == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(keyHex)
	if databaseURL == "" || origin == "" || err != nil || len(key) != 32 {
		return nil, errors.New("staff login configuration is invalid")
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return identity.OpenLogin(startup, databaseURL, identity.LoginConfig{Origin: origin, SigningKey: key})
}
