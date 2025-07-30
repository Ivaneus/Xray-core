package conf

import (
	"encoding/json"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/http"
	"google.golang.org/protobuf/proto"
)

type HTTPAccount struct {
	Username string `json:"user"`
	Password string `json:"pass"`
}

func (v *HTTPAccount) Build() *http.Account {
	return &http.Account{
		Username: v.Username,
		Password: v.Password,
	}
}

const (
	HTTPAuthMethodNoAuth         = "noauth"
	HTTPAuthMethodUserPass       = "password"
	HTTPAuthMethodKeyAuth        = "keyauth"
	ValidKey               int32 = 1 // 添加新的认证方法常量
)

type HTTPServerConfig struct {
	Accounts    []*HTTPAccount `json:"accounts"`
	Transparent bool           `json:"allowTransparent"`
	UserLevel   uint32         `json:"userLevel"`
	AuthMethod  string         `json:"auth"` // 添加认证方法字段
	Keys        []string       `json:"keys"` // 添加keys字段
}

func (c *HTTPServerConfig) Build() (proto.Message, error) {
	config := &http.ServerConfig{
		AllowTransparent: c.Transparent,
		UserLevel:        c.UserLevel,
	}

	// 设置认证类型
	switch c.AuthMethod {
	case HTTPAuthMethodNoAuth:
		config.AuthType = http.AuthType_NO_AUTH
	case HTTPAuthMethodUserPass:
		config.AuthType = http.AuthType_PASSWORD
	case HTTPAuthMethodKeyAuth:
		config.AuthType = http.AuthType_KEYAUTH
		// 设置keys字段
		if c.Keys != nil {
			config.Keys = make(map[string]int32)
			for _, key := range c.Keys {
				config.Keys[key] = http.ValidKey // 直接使用值1
			}
		}
	default:
		// 默认不认证
		config.AuthType = http.AuthType_NO_AUTH
	}

	if len(c.Accounts) > 0 {
		config.Accounts = make(map[string]string)
		for _, account := range c.Accounts {
			config.Accounts[account.Username] = account.Password
		}
	}

	return config, nil
}

type HTTPRemoteConfig struct {
	Address *Address          `json:"address"`
	Port    uint16            `json:"port"`
	Users   []json.RawMessage `json:"users"`
}

type HTTPClientConfig struct {
	Servers []*HTTPRemoteConfig `json:"servers"`
	Headers map[string]string   `json:"headers"`
}

func (v *HTTPClientConfig) Build() (proto.Message, error) {
	config := new(http.ClientConfig)
	config.Server = make([]*protocol.ServerEndpoint, len(v.Servers))
	for idx, serverConfig := range v.Servers {
		server := &protocol.ServerEndpoint{
			Address: serverConfig.Address.Build(),
			Port:    uint32(serverConfig.Port),
		}
		for _, rawUser := range serverConfig.Users {
			user := new(protocol.User)
			if err := json.Unmarshal(rawUser, user); err != nil {
				return nil, errors.New("failed to parse HTTP user").Base(err).AtError()
			}
			account := new(HTTPAccount)
			if err := json.Unmarshal(rawUser, account); err != nil {
				return nil, errors.New("failed to parse HTTP account").Base(err).AtError()
			}
			user.Account = serial.ToTypedMessage(account.Build())
			server.User = append(server.User, user)
		}
		config.Server[idx] = server
	}
	config.Header = make([]*http.Header, 0, 32)
	for key, value := range v.Headers {
		config.Header = append(config.Header, &http.Header{
			Key:   key,
			Value: value,
		})
	}
	return config, nil
}
