package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func parseSIPURI(s string) (sip.Uri, error) {
	var uri sip.Uri
	if err := sip.ParseUri(s, &uri); err != nil {
		return uri, fmt.Errorf("bad SIP URI %q: %w", s, err)
	}
	if uri.Scheme == "" {
		uri.Scheme = "sip"
	}
	return uri, nil
}

// ---------------------------------------------------------------- outgoing

// Call sends an INVITE to the given number.
func (p *Phone) Call(number string) error {
	if number == "" {
		return errors.New("number is required")
	}

	p.mu.Lock()
	if p.dClient == nil || p.status.Registration != regActive {
		p.mu.Unlock()
		return errors.New("not registered")
	}
	if p.status.Call != callIdle {
		p.mu.Unlock()
		return fmt.Errorf("phone is busy (%s)", p.status.Call)
	}
	p.status.Call = callOutgoing
	p.status.CallRemote = number
	p.status.CallStarted = ""
	cfg := p.config
	dClient := p.dClient
	ctx, cancel := context.WithCancel(context.Background())
	p.outCancel = cancel
	p.mu.Unlock()

	p.log.Info("outgoing call", "number", number, "server", cfg.Server)

	go p.runOutgoing(ctx, cancel, dClient, cfg, number)
	return nil
}

func (p *Phone) runOutgoing(ctx context.Context, cancel context.CancelFunc, dClient *sipgo.DialogClientCache, cfg Config, number string) {
	defer cancel()

	recipient, err := parseSIPURI("sip:" + number + "@" + cfg.Server)
	if err != nil {
		p.log.Error("bad callee number", "error", err)
		p.finishCall()
		return
	}

	sess, err := dClient.Invite(ctx, recipient, nil)
	if err != nil {
		p.log.Error("INVITE failed", "error", err)
		p.finishCall()
		return
	}

	p.mu.Lock()
	p.outSess = sess
	p.mu.Unlock()

	sess.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			p.finishCall()
		}
	})

	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{
		OnResponse: func(res *sip.Response) error {
			switch res.StatusCode {
			case sip.StatusRinging, sip.StatusCallIsForwarded, sip.StatusQueued, sip.StatusSessionInProgress:
				p.setCall(callRinging)
			}
			return nil
		},
		// Some PBXes also challenge INVITE - let the stack answer the digest.
		Username: cfg.Number,
		Password: cfg.Password,
	})
	if err != nil {
		if ctx.Err() != nil {
			p.log.Info("call canceled", "number", number, "cause", err)
		} else {
			p.log.Error("call was not answered", "number", number, "error", err)
		}
		p.finishCall()
		return
	}

	if err := sess.Ack(ctx); err != nil {
		p.log.Error("ACK failed", "error", err)
	}

	p.mu.Lock()
	p.status.Call = callActive
	p.status.CallStarted = time.Now().Format(time.RFC3339)
	p.mu.Unlock()
	p.log.Info("call established (signaling only, no media)", "remote", number)
}

// CancelCall sends CANCEL for an unanswered outgoing call.
func (p *Phone) CancelCall() error {
	p.mu.Lock()
	if p.status.Call != callOutgoing && p.status.Call != callRinging && p.status.Call != callCanceling {
		p.mu.Unlock()
		return errors.New("there is no outgoing call to cancel")
	}
	cancel := p.outCancel
	p.status.Call = callCanceling
	p.mu.Unlock()

	if cancel == nil {
		return errors.New("there is no outgoing call to cancel")
	}
	p.log.Info("canceling outgoing call")
	cancel() // WaitAnswer detects ctx cancellation and sends CANCEL
	return nil
}

// ---------------------------------------------------------------- incoming

func (p *Phone) registerServerHandlers() {
	p.srv.OnInvite(p.onIncomingInvite)

	p.srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		p.mu.Lock()
		d := p.dServer
		p.mu.Unlock()
		if d == nil {
			return
		}
		if err := d.ReadAck(req, tx); err != nil {
			p.log.Debug("unmatched ACK", "error", err)
		}
	})

	p.srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		p.mu.Lock()
		dClient, dServer := p.dClient, p.dServer
		p.mu.Unlock()

		// The BYE belongs to our UAC or UAS dialog. Both respond 200 OK.
		if dClient != nil && dClient.ReadBye(req, tx) == nil {
			p.log.Info("call terminated by remote BYE")
			return
		}
		if dServer != nil && dServer.ReadBye(req, tx) == nil {
			p.log.Info("call terminated by remote BYE")
			return
		}
		p.log.Warn("BYE for unknown dialog")
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
	})

	p.srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		res.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, OPTIONS, REGISTER"))
		_ = tx.Respond(res)
	})
}

func (p *Phone) onIncomingInvite(req *sip.Request, tx sip.ServerTransaction) {
	p.mu.Lock()
	if p.dServer == nil || p.status.Registration != regActive {
		p.mu.Unlock()
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusForbidden, "Not Registered", nil))
		return
	}
	if p.status.Call != callIdle {
		p.mu.Unlock()
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBusyHere, "Busy Here", nil))
		return
	}
	p.mu.Unlock()

	sess, err := p.dServer.ReadInvite(req, tx)
	if err != nil {
		p.log.Error("failed to accept INVITE", "error", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Error", nil))
		return
	}

	remote := "unknown"
	if f := req.From(); f != nil && f.Address.User != "" {
		remote = f.Address.User
	}

	if err := sess.Respond(sip.StatusRinging, "Ringing", nil); err != nil {
		p.log.Error("failed to send 180 Ringing", "error", err)
	}

	p.mu.Lock()
	if p.status.Call != callIdle { // a race with an outgoing call
		p.mu.Unlock()
		_ = sess.Respond(sip.StatusBusyHere, "Busy Here", nil)
		return
	}
	p.inSess = sess
	p.status.Call = callIncoming
	p.status.CallRemote = remote
	p.mu.Unlock()
	p.log.Info("incoming call", "from", remote)

	// The handler MUST NOT return before the dialog finishes: sipgo
	// terminates the INVITE server transaction as soon as the handler
	// returns, and then we could not answer 200 OK anymore. The answer /
	// decline is done from the API handlers; the context is canceled when
	// the dialog ends (BYE, CANCEL, decline or timeout).
	<-sess.Context().Done()
	p.log.Info("incoming call transaction finished", "from", remote)

	p.mu.Lock()
	if p.inSess == sess {
		p.inSess = nil
		if p.status.Call != callIdle {
			p.status.Call = callIdle
			p.status.CallRemote = ""
			p.status.CallStarted = ""
		}
	}
	p.mu.Unlock()
}

// Answer accepts an incoming call (200 OK to INVITE).
func (p *Phone) Answer() error {
	p.mu.Lock()
	if p.status.Call != callIncoming || p.inSess == nil {
		p.mu.Unlock()
		return errors.New("there is no incoming call")
	}
	sess := p.inSess
	remote := p.status.CallRemote
	p.mu.Unlock()

	// Respond may block for a while; update the state only afterwards and
	// only if this session is still the current one.
	if err := sess.Respond(sip.StatusOK, "OK", nil); err != nil {
		p.log.Error("failed to send 200 OK", "error", err)
		return fmt.Errorf("answer failed: %w", err)
	}

	p.mu.Lock()
	if p.inSess == sess && p.status.Call == callIncoming {
		p.status.Call = callActive
		p.status.CallStarted = time.Now().Format(time.RFC3339)
	}
	p.mu.Unlock()
	p.log.Info("answered incoming call", "remote", remote)
	return nil
}

// Decline rejects an incoming call (603 Decline Here).
func (p *Phone) Decline() error {
	p.mu.Lock()
	if p.status.Call != callIncoming || p.inSess == nil {
		p.mu.Unlock()
		return errors.New("there is no incoming call")
	}
	sess := p.inSess
	remote := p.status.CallRemote
	p.mu.Unlock()

	if err := sess.Respond(sip.StatusGlobalDecline, "Decline Here", nil); err != nil {
		p.log.Error("failed to send 603 Decline", "error", err)
	}

	p.mu.Lock()
	if p.inSess == sess && p.status.Call == callIncoming {
		p.inSess = nil
		p.status.Call = callIdle
		p.status.CallRemote = ""
	}
	p.mu.Unlock()
	p.log.Info("incoming call declined", "remote", remote)
	return nil
}

// ---------------------------------------------------------------- common

// Hangup ends an established conversation with BYE.
func (p *Phone) Hangup() error {
	p.mu.Lock()
	if p.status.Call != callActive {
		p.mu.Unlock()
		return errors.New("there is no active call")
	}
	out, in := p.outSess, p.inSess
	p.status.Call = callEnding
	p.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		switch {
		case out != nil:
			err = out.Bye(ctx)
		case in != nil:
			err = in.Bye(ctx)
		}
		if err != nil {
			p.log.Error("BYE failed", "error", err)
		} else {
			p.log.Info("call terminated with BYE")
		}
		p.finishCall()
	}()
	return nil
}

// finishCall resets call state. Safe to call multiple times.
func (p *Phone) finishCall() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status.Call == callIdle {
		return
	}
	p.status.Call = callIdle
	p.status.CallRemote = ""
	p.status.CallStarted = ""
	p.outSess = nil
	p.outCancel = nil
	p.inSess = nil
}

func (p *Phone) setCall(state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.Call = state
}
