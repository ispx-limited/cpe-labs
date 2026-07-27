package cwmp

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/inform"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/soap"
	"github.com/ispx-limited/cpe-labs/internal/cwmp/transport"
)

// SessionOptions configures one Session.
type SessionOptions struct {
	Transport      *transport.Transport
	Inform         *inform.Builder
	Handlers       []Handler
	EncoderOptions soap.EncoderOptions
	DecoderOptions soap.DecoderOptions
	SessionTimeout time.Duration
	Logger         *slog.Logger
	IDGenerator    func() string
}

// Session runs one CWMP session against an ACS. Sessions are
// sequential; concurrent calls to Run on the same Session are not
// supported.
type Session struct {
	transport      *transport.Transport
	inform         *inform.Builder
	handlers       map[string]Handler
	pendingCPERPCs []CPEInitiatedRPC
	retryCount     uint
	encOpts        soap.EncoderOptions
	decOpts        soap.DecoderOptions
	timeout        time.Duration
	logger         *slog.Logger
	nextID         func() string
}

// CPEInitiatedRPC is one RPC the CPE wants to send between
// InformResponse and the drain loop. Method() names the RPC (e.g.
// "TransferComplete"); Body() returns the inner body XML the encoder
// wraps in a CWMP envelope.
type CPEInitiatedRPC interface {
	Method() string
	Body() ([]byte, error)
}

// setBuilder swaps the Inform builder used by Run. Used by RunSession
// to inject a per-session builder built with the tracker's current
// parameter lists. Not goroutine-safe; callers must serialize against
// concurrent Run invocations.
func (s *Session) setBuilder(b *inform.Builder) {
	s.inform = b
}

// setPendingCPERPCs replaces the queue of CPE-initiated RPCs the next
// Run call will send between InformResponse and the drain loop. Not
// goroutine-safe; callers serialize against Run invocations.
func (s *Session) setPendingCPERPCs(rpcs []CPEInitiatedRPC) {
	s.pendingCPERPCs = rpcs
}

// setRetryCount sets the RetryCount the next Run call stamps on its
// Inform (TR-069 3.2.1.1). Not goroutine-safe; callers serialize
// against Run invocations. RunSession sets it from RetryState before
// each session.
func (s *Session) setRetryCount(n uint) {
	s.retryCount = n
}

// NewSession validates opts and returns a Session ready for Run.
func NewSession(opts SessionOptions) (*Session, error) {
	if opts.Transport == nil {
		return nil, cpeerr.Wrap("cwmp.NewSession", cpeerr.KindInvalidArgument,
			fmt.Errorf("transport is required"))
	}
	if opts.Inform == nil {
		return nil, cpeerr.Wrap("cwmp.NewSession", cpeerr.KindInvalidArgument,
			fmt.Errorf("inform builder is required"))
	}
	if opts.Logger == nil {
		return nil, cpeerr.Wrap("cwmp.NewSession", cpeerr.KindInvalidArgument,
			fmt.Errorf("logger is required"))
	}

	handlers := make(map[string]Handler, len(opts.Handlers))
	for _, h := range opts.Handlers {
		method := h.Method()
		if method == "" {
			return nil, cpeerr.Wrap("cwmp.NewSession", cpeerr.KindInvalidArgument,
				fmt.Errorf("handler returned empty Method()"))
		}
		if _, dup := handlers[method]; dup {
			return nil, cpeerr.Wrap("cwmp.NewSession", cpeerr.KindInvalidArgument,
				fmt.Errorf("duplicate handler for method %q", method))
		}
		handlers[method] = h
	}

	idGen := opts.IDGenerator
	if idGen == nil {
		var counter atomic.Uint64
		idGen = func() string {
			return strconv.FormatUint(counter.Add(1), 10)
		}
	}

	return &Session{
		transport: opts.Transport,
		inform:    opts.Inform,
		handlers:  handlers,
		encOpts:   opts.EncoderOptions,
		decOpts:   opts.DecoderOptions,
		timeout:   opts.SessionTimeout,
		logger:    opts.Logger,
		nextID:    idGen,
	}, nil
}

// Run executes the session: build Inform from events, send it,
// validate InformResponse, drain ACS-initiated RPCs, close on 204
// or empty body.
func (s *Session) Run(ctx context.Context, events []inform.Event) error {
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	s.transport.ResetSession()
	s.logger.Info("cwmp session start", "events", eventCodes(events))

	if err := s.sendInform(ctx, events); err != nil {
		s.logger.Info("cwmp session end", "result", "error", "err", err.Error())
		return err
	}

	// Send any queued CPE-initiated RPCs (e.g. TransferComplete) and
	// validate the matching method-response from the ACS, before
	// entering the drain loop.
	for _, rpc := range s.pendingCPERPCs {
		if err := s.sendCPEInitiated(ctx, rpc); err != nil {
			s.logger.Info("cwmp session end", "result", "error", "err", err.Error())
			return err
		}
	}

	// Drain loop: each iteration sends `outbound` (nil -> empty POST) and
	// reads the response. If the response is empty / 204, we're done.
	// Otherwise dispatch and use the dispatched output as the next
	// outbound payload.
	var outbound []byte
	for {
		respBytes, err := s.send(ctx, outbound)
		if err != nil {
			s.logger.Info("cwmp session end", "result", "error", "err", err.Error())
			return err
		}
		if isSessionClose(respBytes) {
			s.logger.Info("cwmp session end", "result", "ok")
			return nil
		}

		outbound, err = s.handleACSMessage(ctx, respBytes)
		if err != nil {
			s.logger.Info("cwmp session end", "result", "error", "err", err.Error())
			return err
		}
	}
}

// sendInform builds and sends the Inform envelope, then validates the
// ACS response is an InformResponse.
func (s *Session) sendInform(ctx context.Context, events []inform.Event) error {
	inf, err := s.inform.Build(events, s.retryCount)
	if err != nil {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument, err)
	}

	envelope, err := s.encodeRequest(s.nextID(), "Inform", func(w io.Writer) error {
		return inform.Render(w, inf)
	})
	if err != nil {
		return err
	}

	respBytes, err := s.send(ctx, envelope)
	if err != nil {
		return err
	}
	if isSessionClose(respBytes) {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument,
			fmt.Errorf("ACS closed session before InformResponse"))
	}
	return s.validateInformResponse(respBytes)
}

// handleACSMessage decodes an ACS-initiated message and returns the
// outbound envelope to send next: either a method response or a fault.
func (s *Session) handleACSMessage(ctx context.Context, b []byte) ([]byte, error) {
	d := soap.NewDecoder(bytes.NewReader(b), s.decOpts)
	env, err := d.ReadEnvelope()
	if err != nil {
		return nil, cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument, err)
	}
	if env.IsFault {
		return nil, cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument,
			fmt.Errorf("ACS returned fault during drain"))
	}

	tokens, err := d.MethodTokens()
	if err != nil {
		return nil, cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument, err)
	}

	return s.dispatch(ctx, env.Method, env.Header.ID, tokens)
}

// dispatch routes the method to a handler (or auto-faults). Returns
// the response envelope bytes the loop will send next.
func (s *Session) dispatch(ctx context.Context, method, requestID string, req xml.TokenReader) ([]byte, error) {
	handler, found := s.handlers[method]
	if !found {
		s.logger.Warn("cwmp method not supported", "method", method, "id", requestID)
		drainTokens(req)
		return s.encodeFault(requestID, soap.Fault{
			FaultCode:   faultMethodNotSupported,
			FaultString: "Method not supported",
		})
	}

	s.logger.Debug("cwmp dispatch", "method", method, "id", requestID)

	var bodyBuf bytes.Buffer
	handlerErr := handler.Handle(ctx, req, &bodyBuf)
	// Drain any unconsumed request tokens so the decoder is consistent
	// even if the handler returned early.
	drainTokens(req)

	if handlerErr != nil {
		var faultErr *FaultError
		if errors.As(handlerErr, &faultErr) {
			return s.encodeFault(requestID, faultErr.Fault)
		}
		return s.encodeFault(requestID, soap.Fault{
			FaultCode:   faultInternalError,
			FaultString: handlerErr.Error(),
		})
	}

	return s.encodeRequest(requestID, method+"Response", func(w io.Writer) error {
		_, err := w.Write(bodyBuf.Bytes())
		return err
	})
}

// sendCPEInitiated encodes rpc into a CWMP envelope, sends it, and
// validates that the ACS responded with the matching <Method>Response
// envelope. ACS-side faults during this phase abort the session.
func (s *Session) sendCPEInitiated(ctx context.Context, rpc CPEInitiatedRPC) error {
	body, err := rpc.Body()
	if err != nil {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInternal,
			fmt.Errorf("build %s body: %w", rpc.Method(), err))
	}
	requestID := s.nextID()
	envelope, err := s.encodeRequest(requestID, rpc.Method(), func(w io.Writer) error {
		_, werr := w.Write(body)
		return werr
	})
	if err != nil {
		return err
	}

	respBytes, err := s.send(ctx, envelope)
	if err != nil {
		return err
	}
	if isSessionClose(respBytes) {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument,
			fmt.Errorf("ACS closed session before %sResponse", rpc.Method()))
	}
	return s.validateMethodResponse(respBytes, rpc.Method()+"Response")
}

// validateMethodResponse confirms b is the named method-response
// envelope (or surfaces a fault as a session error).
func (s *Session) validateMethodResponse(b []byte, expectedMethod string) error {
	d := soap.NewDecoder(bytes.NewReader(b), s.decOpts)
	env, err := d.ReadEnvelope()
	if err != nil {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument, err)
	}
	if env.IsFault {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument,
			fmt.Errorf("ACS returned fault on %s", expectedMethod))
	}
	if env.Method != expectedMethod {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument,
			fmt.Errorf("expected %s, got %q", expectedMethod, env.Method))
	}
	tokens, err := d.MethodTokens()
	if err != nil {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument, err)
	}
	drainTokens(tokens)
	return nil
}

// validateInformResponse confirms the bytes hold an InformResponse
// envelope (or a fault, which is reported as a session error).
func (s *Session) validateInformResponse(b []byte) error {
	d := soap.NewDecoder(bytes.NewReader(b), s.decOpts)
	env, err := d.ReadEnvelope()
	if err != nil {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument, err)
	}
	if env.IsFault {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument,
			fmt.Errorf("ACS returned fault on Inform"))
	}
	if env.Method != "InformResponse" {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument,
			fmt.Errorf("expected InformResponse, got %q", env.Method))
	}
	tokens, err := d.MethodTokens()
	if err != nil {
		return cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInvalidArgument, err)
	}
	drainTokens(tokens)
	return nil
}

// send wraps transport.Send to translate 204 No Content into a
// session-close signal (returns empty bytes, no error).
func (s *Session) send(ctx context.Context, body []byte) ([]byte, error) {
	resp, err := s.transport.Send(ctx, body)
	if err != nil {
		var httpErr *transport.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == 204 {
			return nil, nil // 204 == clean session close
		}
		return nil, cpeerr.Wrap("cwmp.Session.Run", cpeerr.KindInternal, err)
	}
	return resp, nil
}

// isSessionClose reports whether the ACS response signals "no more
// work", empty body or whitespace-only body.
func isSessionClose(b []byte) bool {
	return len(bytes.TrimSpace(b)) == 0
}

// encodeRequest builds an envelope with the given cwmp:ID and method
// element. fillBody writes the inner body XML.
func (s *Session) encodeRequest(id, method string, fillBody func(io.Writer) error) ([]byte, error) {
	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, s.encOpts)
	mw, err := enc.WriteRequest(soap.Header{ID: id}, method)
	if err != nil {
		return nil, cpeerr.Wrap("cwmp.Session.encode", cpeerr.KindInternal, err)
	}
	if err := fillBody(mwRawWriter{mw: mw}); err != nil {
		return nil, cpeerr.Wrap("cwmp.Session.encode", cpeerr.KindInternal, err)
	}
	if err := mw.Close(); err != nil {
		return nil, cpeerr.Wrap("cwmp.Session.encode", cpeerr.KindInternal, err)
	}
	return buf.Bytes(), nil
}

// encodeFault builds a Fault envelope echoing the request's ID.
func (s *Session) encodeFault(id string, f soap.Fault) ([]byte, error) {
	var buf bytes.Buffer
	enc := soap.NewEncoder(&buf, s.encOpts)
	if err := enc.WriteFault(soap.Header{ID: id}, f); err != nil {
		return nil, cpeerr.Wrap("cwmp.Session.encode", cpeerr.KindInternal, err)
	}
	return buf.Bytes(), nil
}

// drainTokens reads until io.EOF, discarding tokens. Safe to call
// multiple times.
func drainTokens(r xml.TokenReader) {
	if r == nil {
		return
	}
	for {
		_, err := r.Token()
		if err != nil {
			return
		}
	}
}

// mwRawWriter adapts soap.MethodWriter.Raw to io.Writer.
type mwRawWriter struct {
	mw *soap.MethodWriter
}

func (w mwRawWriter) Write(p []byte) (int, error) {
	if err := w.mw.Raw(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// eventCodes returns the event-code strings from events for log fields.
func eventCodes(events []inform.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.EventCode)
	}
	return out
}
