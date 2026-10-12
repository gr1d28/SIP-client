package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/icholy/digest"
)

const (
	registerExpires  = 3600 // requested registration lifetime, seconds
	registerTimeout  = 10 * time.Second
	registerRetry    = 15 * time.Second
	expiresSafetyGap = 300 // refresh this much before expiry
)

// Register starts (re)registration in background. Status is visible via Status().
func (p *Phone) Register(server, number, password string) error {
	if server == "" || number == "" {
		return errors.New("server and number are required")
	}

	ip := p.resolveAdvertiseIP(server)

	p.mu.Lock()
	if p.regCancel != nil {
		p.regCancel()
	}
	p.config = Config{Server: server, Number: number, Password: password}
	p.status.Server = server
	p.status.Number = number
	p.status.Registration = regRegistering
	p.status.RegError = ""

	client, err := sipgo.NewClient(p.ua,
		sipgo.WithClientHostname(ip),
		sipgo.WithClientPort(p.port),
		sipgo.WithClientLogger(p.log),
	)
	if err != nil {
		p.mu.Unlock()
		return fmt.Errorf("create SIP client: %w", err)
	}
	p.client = client
	contact := p.contactHeader(number, ip)
	p.dClient = sipgo.NewDialogClientCache(client, *contact)
	p.dServer = sipgo.NewDialogServerCache(client, *contact)
	ctx, cancel := context.WithCancel(context.Background())
	p.regCancel = cancel
	p.mu.Unlock()

	p.log.Info("registration started", "server", server, "number", number, "contact", contact.Address.String())

	go p.registerLoop(ctx, Config{Server: server, Number: number, Password: password})
	return nil
}

// registerLoop registers and refreshes until ctx is canceled.
func (p *Phone) registerLoop(ctx context.Context, cfg Config) {
	for {
		res, err := p.registerOnce(ctx, cfg, registerExpires)
		var wait time.Duration
		switch {
		case err != nil:
			p.setReg(regFailed, err.Error())
			p.log.Error("REGISTER failed", "error", err)
			wait = registerRetry
		case res.StatusCode != sip.StatusOK:
			msg := fmt.Sprintf("server answered %d %s", res.StatusCode, res.Reason)
			p.setReg(regFailed, msg)
			p.log.Error("REGISTER rejected", "status", res.StatusCode, "reason", res.Reason)
			wait = registerRetry
		default:
			expires := responseExpires(res, registerExpires)
			next := expires - expiresSafetyGap
			if next < 60 {
				next = expires / 2
			}
			if next < 60 {
				next = 60
			}
			p.setReg(regActive, "")
			p.log.Info("registered", "server", cfg.Server, "number", cfg.Number, "expires", expires)
			wait = time.Duration(next) * time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Unregister stops refreshes and best-effort sends REGISTER with Expires: 0.
func (p *Phone) Unregister() {
	p.mu.Lock()
	if p.regCancel != nil {
		p.regCancel()
		p.regCancel = nil
	}
	cfg := p.config
	status := p.status.Registration
	p.status.Registration = regNone
	p.status.RegError = ""
	p.mu.Unlock()

	if status == regActive {
		ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
		defer cancel()
		if _, err := p.registerOnce(ctx, cfg, 0); err != nil {
			p.log.Warn("unregister request failed", "error", err)
		}
	}
	p.log.Info("unregistered", "number", cfg.Number)
}

// registerOnce performs REGISTER with a digest authentication round if challenged.
func (p *Phone) registerOnce(ctx context.Context, cfg Config, expires int) (*sip.Response, error) {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return nil, errors.New("not configured")
	}

	recipient, err := parseSIPURI("sip:" + cfg.Number + "@" + cfg.Server)
	if err != nil {
		return nil, err
	}

	contact := p.contactHeader(cfg.Number, p.advertisedIP())
	contact.Params = sip.NewParams()
	contact.Params.Add("expires", strconv.Itoa(expires))

	req := sip.NewRequest(sip.REGISTER, recipient)
	req.SetTransport("UDP")
	req.AppendHeader(contact)
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(expires)))

	res, err := p.doRequest(ctx, client, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != sip.StatusUnauthorized && res.StatusCode != sip.StatusProxyAuthRequired {
		return res, nil
	}

	// Digest challenge: answer with credentials and retry.
	chalHdr := res.GetHeader("WWW-Authenticate")
	authHeader := "Authorization"
	if res.StatusCode == sip.StatusProxyAuthRequired {
		chalHdr = res.GetHeader("Proxy-Authenticate")
		authHeader = "Proxy-Authorization"
	}
	if chalHdr == nil {
		return nil, fmt.Errorf("%d without authentication challenge", res.StatusCode)
	}
	chal, err := digest.ParseChallenge(chalHdr.Value())
	if err != nil {
		return nil, fmt.Errorf("parse auth challenge: %w", err)
	}
	cred, err := digest.Digest(chal, digest.Options{
		Method:   sip.REGISTER.String(),
		URI:      recipient.Host,
		Username: cfg.Number,
		Password: cfg.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("compute digest: %w", err)
	}

	authReq := req.Clone()
	authReq.RemoveHeader("Via") // regenerated for the retried request
	authReq.AppendHeader(sip.NewHeader(authHeader, cred.String()))
	return p.doRequest(ctx, client, authReq, sipgo.ClientRequestIncreaseCSEQ, sipgo.ClientRequestAddVia)
}

// doRequest sends a request through a client transaction and waits for the
// final response, skipping provisional ones.
func (p *Phone) doRequest(ctx context.Context, client *sipgo.Client, req *sip.Request, opts ...sipgo.ClientRequestOption) (*sip.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()

	tx, err := client.TransactionRequest(ctx, req, opts...)
	if err != nil {
		return nil, fmt.Errorf("send %s: %w", req.Method, err)
	}
	defer tx.Terminate()

	for {
		select {
		case res := <-tx.Responses():
			if res.IsProvisional() {
				continue
			}
			return res, nil
		case <-tx.Done():
			if err := tx.Err(); err != nil {
				return nil, fmt.Errorf("%s transaction failed: %w", req.Method, err)
			}
			return nil, fmt.Errorf("%s transaction died without a response", req.Method)
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: no response within %s", req.Method, registerTimeout)
		}
	}
}

// responseExpires reads registration lifetime from the 200 OK response
// (Contact expires param, then Expires header), defaulting to def seconds.
func responseExpires(res *sip.Response, def int) int {
	if c := res.Contact(); c != nil {
		if v, ok := c.Params.Get("expires"); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	if h := res.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(h.Value()); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func (p *Phone) setReg(state, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.Registration = state
	p.status.RegError = errMsg
}
