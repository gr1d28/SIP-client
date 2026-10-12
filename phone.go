package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Registration states.
const (
	regNone        = "none"
	regRegistering = "registering"
	regActive      = "registered"
	regFailed      = "failed"
)

// Call states.
const (
	callIdle      = "idle"
	callOutgoing  = "outgoing"  // INVITE sent, no final answer yet
	callRinging   = "ringing"   // 180/183 received for our outgoing call
	callIncoming  = "incoming"  // incoming INVITE, we are ringing
	callActive    = "active"    // conversation established
	callCanceling = "canceling" // CANCEL sent, waiting for the final answer
	callEnding    = "ending"    // BYE in flight
)

// Status is the snapshot exposed to the web UI.
type Status struct {
	Registration string `json:"registration"`
	RegError     string `json:"reg_error,omitempty"`
	Server       string `json:"server,omitempty"`
	Number       string `json:"number,omitempty"`
	Call         string `json:"call"`
	CallRemote   string `json:"call_remote,omitempty"`
	CallStarted  string `json:"call_started,omitempty"` // RFC3339, set when the conversation started
}

// Phone is a signaling-only SIP softphone with a web UI.
type Phone struct {
	log  *slog.Logger
	port int // local UDP port advertised in Contact/Via

	mu     sync.Mutex
	status Status

	advertiseIP string // set by flag or detected towards the SIP server

	ua     *sipgo.UserAgent
	srv    *sipgo.Server
	client *sipgo.Client // (re)created on registration, once the IP/number are known

	dClient *sipgo.DialogClientCache // outgoing dialogs
	dServer *sipgo.DialogServerCache // incoming dialogs

	config Config

	regCancel context.CancelFunc

	outSess   *sipgo.DialogClientSession
	outCancel context.CancelFunc
	inSess    *sipgo.DialogServerSession
}

type Config struct {
	Server   string
	Number   string
	Password string
}

func NewPhone(sipListenAddr, advertiseIP string, log *slog.Logger) (*Phone, error) {
	_, portStr, err := net.SplitHostPort(sipListenAddr)
	if err != nil {
		return nil, fmt.Errorf("bad -sip address %q: %w", sipListenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad -sip port: %w", err)
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("web-sipphone"))
	if err != nil {
		return nil, fmt.Errorf("create user agent: %w", err)
	}

	// The server handle shares the user agent transport: incoming requests
	// (INVITE/BYE/...) arrive on the same UDP socket the client sends from.
	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(log))
	if err != nil {
		return nil, fmt.Errorf("create server: %w", err)
	}

	p := &Phone{
		log:         log,
		port:        port,
		advertiseIP: advertiseIP,
		ua:          ua,
		srv:         srv,
		status:      Status{Registration: regNone, Call: callIdle},
	}
	p.registerServerHandlers()
	return p, nil
}

// Start binds the UDP socket and begins serving SIP. It returns once bound.
func (p *Phone) Start(ctx context.Context, listenAddr string) error {
	conn, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("bind SIP %s: %w", listenAddr, err)
	}
	go func() {
		if err := p.srv.ServeUDP(conn); err != nil && ctx.Err() == nil {
			p.log.Error("SIP listener failed", "error", err)
		}
	}()
	p.log.Info("SIP transport listening", "network", "udp", "addr", conn.LocalAddr().String())
	return nil
}

// Status returns a copy of the current status.
func (p *Phone) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// resolveAdvertiseIP determines the local IP used in Via/Contact headers.
// Does not send any traffic: UDP "dial" only asks the OS which route would
// be used towards the SIP server.
func (p *Phone) resolveAdvertiseIP(serverAddr string) string {
	p.mu.Lock()
	ip := p.advertiseIP
	p.mu.Unlock()
	if ip != "" {
		return ip
	}

	host := serverAddr
	if h, _, err := net.SplitHostPort(serverAddr); err == nil {
		host = h
	}
	if ips, err := net.LookupIP(host); err == nil {
		for _, ipAddr := range ips {
			if v4 := ipAddr.To4(); v4 != nil {
				host = v4.String()
				break
			}
		}
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, "5060"))
	if err != nil {
		p.log.Warn("cannot detect local IP towards the SIP server, falling back to 127.0.0.1", "error", err)
		return "127.0.0.1"
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "127.0.0.1"
	}
	p.mu.Lock()
	p.advertiseIP = local.IP.String()
	p.mu.Unlock()
	return local.IP.String()
}

// contactHeader builds <sip:<number>@<ip>:<port>> used in Contact headers.
func (p *Phone) contactHeader(number, ip string) *sip.ContactHeader {
	uri := sip.Uri{Scheme: "sip", User: number, Host: ip, Port: p.port}
	return &sip.ContactHeader{Address: uri}
}

func (p *Phone) advertisedIP() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.advertiseIP
}
