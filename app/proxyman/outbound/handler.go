package outbound

import (
	"context"
	"crypto/rand"
	goerrors "errors"
	"io"
	"math/big"
	gonet "net"
	"os"
	"strings"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/mux"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/net/cnc"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/pipe"
	"google.golang.org/protobuf/proto"
)

func getStatCounter(v *core.Instance, tag string) (stats.Counter, stats.Counter) {
	var uplinkCounter stats.Counter
	var downlinkCounter stats.Counter

	policy := v.GetFeature(policy.ManagerType()).(policy.Manager)
	if len(tag) > 0 && policy.ForSystem().Stats.OutboundUplink {
		statsManager := v.GetFeature(stats.ManagerType()).(stats.Manager)
		name := "outbound>>>" + tag + ">>>traffic>>>uplink"
		c, _ := stats.GetOrRegisterCounter(statsManager, name)
		if c != nil {
			uplinkCounter = c
		}
	}
	if len(tag) > 0 && policy.ForSystem().Stats.OutboundDownlink {
		statsManager := v.GetFeature(stats.ManagerType()).(stats.Manager)
		name := "outbound>>>" + tag + ">>>traffic>>>downlink"
		c, _ := stats.GetOrRegisterCounter(statsManager, name)
		if c != nil {
			downlinkCounter = c
		}
	}

	return uplinkCounter, downlinkCounter
}

// Handler implements outbound.Handler.
type Handler struct {
	tag             string
	senderSettings  *proxyman.SenderConfig
	streamSettings  *internet.MemoryStreamConfig
	proxyConfig     proto.Message
	proxy           proxy.Outbound
	outboundManager outbound.Manager
	mux             *mux.ClientManager
	xudp            *mux.ClientManager
	udp443          string
	uplinkCounter   stats.Counter
	downlinkCounter stats.Counter
}

// NewHandler creates a new Handler based on the given configuration.
func NewHandler(ctx context.Context, config *core.OutboundHandlerConfig) (outbound.Handler, error) {
	v := core.MustFromContext(ctx)
	uplinkCounter, downlinkCounter := getStatCounter(v, config.Tag)
	h := &Handler{
		tag:             config.Tag,
		outboundManager: v.GetFeature(outbound.ManagerType()).(outbound.Manager),
		uplinkCounter:   uplinkCounter,
		downlinkCounter: downlinkCounter,
	}

	if config.SenderSettings != nil {
		senderSettings, err := config.SenderSettings.GetInstance()
		if err != nil {
			return nil, err
		}
		switch s := senderSettings.(type) {
		case *proxyman.SenderConfig:
			h.senderSettings = s
			mss, err := internet.ToMemoryStreamConfig(s.StreamSettings)
			if err != nil {
				return nil, errors.New("failed to parse stream settings").Base(err).AtWarning()
			}
			h.streamSettings = mss
		default:
			return nil, errors.New("settings is not SenderConfig")
		}
	}

	proxyConfig, err := config.ProxySettings.GetInstance()
	if err != nil {
		return nil, err
	}
	h.proxyConfig = proxyConfig

	rawProxyHandler, err := common.CreateObject(ctx, proxyConfig)
	if err != nil {
		return nil, err
	}

	proxyHandler, ok := rawProxyHandler.(proxy.Outbound)
	if !ok {
		return nil, errors.New("not an outbound handler")
	}

	if h.senderSettings != nil && h.senderSettings.MultiplexSettings != nil {
		if config := h.senderSettings.MultiplexSettings; config.Enabled {
			if config.Concurrency < 0 {
				h.mux = &mux.ClientManager{Enabled: false}
			}
			if config.Concurrency == 0 {
				config.Concurrency = 8 // same as before
			}
			if config.Concurrency > 0 {
				h.mux = &mux.ClientManager{
					Enabled: true,
					Picker: &mux.IncrementalWorkerPicker{
						Factory: &mux.DialingWorkerFactory{
							Proxy:  proxyHandler,
							Dialer: h,
							Strategy: mux.ClientStrategy{
								MaxConcurrency: uint32(config.Concurrency),
								MaxConnection:  128,
							},
						},
					},
				}
			}
			if config.XudpConcurrency < 0 {
				h.xudp = &mux.ClientManager{Enabled: false}
			}
			if config.XudpConcurrency == 0 {
				h.xudp = nil // same as before
			}
			if config.XudpConcurrency > 0 {
				h.xudp = &mux.ClientManager{
					Enabled: true,
					Picker: &mux.IncrementalWorkerPicker{
						Factory: &mux.DialingWorkerFactory{
							Proxy:  proxyHandler,
							Dialer: h,
							Strategy: mux.ClientStrategy{
								MaxConcurrency: uint32(config.XudpConcurrency),
								MaxConnection:  128,
							},
						},
					},
				}
			}
			h.udp443 = config.XudpProxyUDP443
		}
	}

	h.proxy = proxyHandler
	return h, nil
}

// Tag implements outbound.Handler.
func (h *Handler) Tag() string {
	return h.tag
}

// Dispatch implements proxy.Outbound.Dispatch.
func (h *Handler) Dispatch(ctx context.Context, link *transport.Link) {
	outbounds := session.OutboundsFromContext(ctx)
	ob := outbounds[len(outbounds)-1]
	if ob.Target.Network == net.Network_UDP && ob.OriginalTarget.Address != nil && ob.OriginalTarget.Address != ob.Target.Address {
		link.Reader = &buf.EndpointOverrideReader{Reader: link.Reader, Dest: ob.Target.Address, OriginalDest: ob.OriginalTarget.Address}
		link.Writer = &buf.EndpointOverrideWriter{Writer: link.Writer, Dest: ob.Target.Address, OriginalDest: ob.OriginalTarget.Address}
	}
	if h.mux != nil {
		test := func(err error) {
			if err != nil {
				err := errors.New("failed to process mux outbound traffic").Base(err)
				session.SubmitOutboundErrorToOriginator(ctx, err)
				errors.LogInfo(ctx, err.Error())
				common.Interrupt(link.Writer)
			}
		}
		if ob.Target.Network == net.Network_UDP && ob.Target.Port == 443 {
			switch h.udp443 {
			case "reject":
				test(errors.New("XUDP rejected UDP/443 traffic").AtInfo())
				return
			case "skip":
				goto out
			}
		}
		if h.xudp != nil && ob.Target.Network == net.Network_UDP {
			if !h.xudp.Enabled {
				goto out
			}
			test(h.xudp.Dispatch(ctx, link))
			return
		}
		if h.mux.Enabled {
			test(h.mux.Dispatch(ctx, link))
			return
		}
	}
out:
	err := h.proxy.Process(ctx, link, h)
	if err != nil {
		if goerrors.Is(err, io.EOF) || goerrors.Is(err, io.ErrClosedPipe) || goerrors.Is(err, context.Canceled) {
			err = nil
		}
	}
	if err != nil {
		// Ensure outbound ray is properly closed.
		err := errors.New("failed to process outbound traffic").Base(err)
		session.SubmitOutboundErrorToOriginator(ctx, err)
		errors.LogInfo(ctx, err.Error())
		common.Interrupt(link.Writer)
	} else {
		common.Close(link.Writer)
	}
	common.Interrupt(link.Reader)
}

// Address implements internet.Dialer.
func (h *Handler) Address() net.Address {
	if h.senderSettings == nil || h.senderSettings.Via == nil {
		return nil
	}
	return h.senderSettings.Via.AsAddress()
}

func (h *Handler) DestIpAddress() net.IP {
	return internet.DestIpAddress()
}

// Dial implements internet.Dialer.
func (h *Handler) Dial(ctx context.Context, dest net.Destination) (stat.Connection, error) {
	if h.senderSettings != nil {

		if h.senderSettings.ProxySettings.HasTag() {

			tag := h.senderSettings.ProxySettings.Tag
			handler := h.outboundManager.GetHandler(tag)
			if handler != nil {
				errors.LogDebug(ctx, "proxying to ", tag, " for dest ", dest)
				outbounds := session.OutboundsFromContext(ctx)
				ctx = session.ContextWithOutbounds(ctx, append(outbounds, &session.Outbound{
					Target: dest,
					Tag:    tag,
				})) // add another outbound in session ctx
				opts := pipe.OptionsFromContext(ctx)
				uplinkReader, uplinkWriter := pipe.New(opts...)
				downlinkReader, downlinkWriter := pipe.New(opts...)

				go handler.Dispatch(ctx, &transport.Link{Reader: uplinkReader, Writer: downlinkWriter})
				conn := cnc.NewConnection(cnc.ConnectionInputMulti(uplinkWriter), cnc.ConnectionOutputMulti(downlinkReader))

				if config := tls.ConfigFromStreamSettings(h.streamSettings); config != nil {
					tlsConfig := config.GetTLSConfig(tls.WithDestination(dest))
					conn = tls.Client(conn, tlsConfig)
				}

				return h.getStatCouterConnection(conn), nil
			}

			errors.LogWarning(ctx, "failed to get outbound handler with tag: ", tag)
		}

		if h.senderSettings.Via != nil {

			outbounds := session.OutboundsFromContext(ctx)
			ob := outbounds[len(outbounds)-1]
			var domain string
			addr := h.senderSettings.Via.AsAddress()
			domain = h.senderSettings.Via.GetDomain()
			switch {
			case h.senderSettings.ViaCidr != "":
				ob.Gateway = ParseRandomIP(addr, h.senderSettings.ViaCidr)

			case domain == "origin":
				if inbound := session.InboundFromContext(ctx); inbound != nil {
					// 使用一条LogDebug语句打印整个inbound对象
					errors.LogDebug(ctx, "Via origin mode - Inbound session: ", inbound)
					errors.LogDebug(ctx, "Via origin mode - Inbound Email: ", inbound.User.Email)

					// 检查用户Email是否存在并以指定前缀开头
					if inbound.User != nil && inbound.User.Email != "" {
						email := inbound.User.Email

						// 处理IPv4地址
						if strings.HasPrefix(email, "ipv4_") {
							ipStr := strings.TrimPrefix(email, "ipv4_")
							// 将格式为114-1-28-114-4转换为114.1.28.114
							parts := strings.Split(ipStr, "-")
							if len(parts) >= 4 {
								// 取前四个部分作为IPv4地址
								ipAddr := strings.Join(parts[:4], ".")
								ob.Gateway = net.ParseAddress(ipAddr)
								errors.LogDebug(ctx, "use email ipv4 as sendthrough: ", ipAddr)
							}
						} else if strings.HasPrefix(email, "ipv6_") {
							// 处理IPv6地址
							ipStr := strings.TrimPrefix(email, "ipv6_")
							// 将格式为2a14-7584-f000-2a32-a975-4c5-3ab4-1232转换为IPv6地址
							parts := strings.Split(ipStr, "-")
							if len(parts) >= 8 {
								// 取前8个部分作为IPv6地址
								ipAddr := strings.Join(parts[:8], ":")
								ob.Gateway = net.ParseAddress(ipAddr)
								errors.LogDebug(ctx, "use email ipv6 as sendthrough: ", ipAddr)
							}
						} else if inbound.Conn != nil {
							// 如果Email未匹配地址格式，继续使用原始的逻辑
							origin, _, err := net.SplitHostPort(inbound.Conn.LocalAddr().String())
							if err == nil {
								ob.Gateway = net.ParseAddress(origin)
								errors.LogDebug(ctx, "use receive package ip as sendthrough: ", origin)
							}
						}
					} else if inbound.Conn != nil {
						// 如果没有用户Email信息，使用原始逻辑
						origin, _, err := net.SplitHostPort(inbound.Conn.LocalAddr().String())
						if err == nil {
							ob.Gateway = net.ParseAddress(origin)
							errors.LogDebug(ctx, "use receive package ip as sendthrough: ", origin)
						}
					}
				} else {
					errors.LogDebug(ctx, "Via origin mode - No inbound session found in context")
				}
			case domain == "srcip":
				if inbound := session.InboundFromContext(ctx); inbound != nil {
					if inbound.Conn != nil {
						clientaddr, _, err := net.SplitHostPort(inbound.Conn.RemoteAddr().String())
						if err == nil {
							ob.Gateway = net.ParseAddress(clientaddr)
							errors.LogDebug(ctx, "use client src ip as snedthrough: ", clientaddr)
						}
					}

				}
			//case addr.Family().IsDomain():
			default:
				ob.Gateway = addr

			}

		}
	}

	if conn, err := h.getUoTConnection(ctx, dest); err != os.ErrInvalid {
		return conn, err
	}

	conn, err := internet.Dial(ctx, dest, h.streamSettings)
	conn = h.getStatCouterConnection(conn)
	outbounds := session.OutboundsFromContext(ctx)
	ob := outbounds[len(outbounds)-1]
	ob.Conn = conn
	return conn, err
}

func (h *Handler) getStatCouterConnection(conn stat.Connection) stat.Connection {
	if h.uplinkCounter != nil || h.downlinkCounter != nil {
		return &stat.CounterConnection{
			Connection:   conn,
			ReadCounter:  h.downlinkCounter,
			WriteCounter: h.uplinkCounter,
		}
	}
	return conn
}

// GetOutbound implements proxy.GetOutbound.
func (h *Handler) GetOutbound() proxy.Outbound {
	return h.proxy
}

// Start implements common.Runnable.
func (h *Handler) Start() error {
	return nil
}

// Close implements common.Closable.
func (h *Handler) Close() error {
	common.Close(h.mux)
	common.Close(h.proxy)
	return nil
}

// SenderSettings implements outbound.Handler.
func (h *Handler) SenderSettings() *serial.TypedMessage {
	return serial.ToTypedMessage(h.senderSettings)
}

// ProxySettings implements outbound.Handler.
func (h *Handler) ProxySettings() *serial.TypedMessage {
	return serial.ToTypedMessage(h.proxyConfig)
}

func ParseRandomIP(addr net.Address, prefix string) net.Address {

	_, ipnet, _ := gonet.ParseCIDR(addr.IP().String() + "/" + prefix)

	ones, bits := ipnet.Mask.Size()
	subnetSize := new(big.Int).Lsh(big.NewInt(1), uint(bits-ones))

	rnd, _ := rand.Int(rand.Reader, subnetSize)

	startInt := new(big.Int).SetBytes(ipnet.IP)
	rndInt := new(big.Int).Add(startInt, rnd)

	rndBytes := rndInt.Bytes()
	padded := make([]byte, len(ipnet.IP))
	copy(padded[len(padded)-len(rndBytes):], rndBytes)

	return net.ParseAddress(gonet.IP(padded).String())
}
