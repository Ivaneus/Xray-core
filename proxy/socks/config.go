package socks

import (
	"google.golang.org/protobuf/proto"

	"github.com/xtls/xray-core/common/protocol"
)

func (a *Account) Equals(another protocol.Account) bool {
	if account, ok := another.(*Account); ok {
		return a.Username == account.Username
	}
	return false
}

func (a *Account) ToProto() proto.Message {
	return a
}

func (a *Account) AsAccount() (protocol.Account, error) {
	return a, nil
}

func (c *ServerConfig) HasAccount(username, password string) bool {
	if c.Accounts == nil {
		return false
	}
	storedPassed, found := c.Accounts[username]
	if !found {
		return false
	}
	return storedPassed == password
}

const (
	ValidKey int32 = 1 // 有效密钥的值
)

// ValidateKey 密钥验证函数 - 使用int32作为值
func (c *ServerConfig) ValidateKey(key string) bool {
	if c.Keys == nil {
		return false
	}
	// 检查key是否存在且值为ValidKey(1)
	value, exists := c.Keys[key]
	return exists && value == ValidKey
}
