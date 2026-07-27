package handlers

import (
	"context"
	"encoding/xml"
	"io"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
)

// grmHandler implements GetRPCMethods (TR-069 A.3.1.1): the ACS asks
// the CPE which CPE-side methods it supports. Capability discovery is
// the first call plugfest-grade ACS firmware makes in a session, so a
// simulated CPE answering fault 9000 here (the earlier behavior)
// diverges from every real device.
type grmHandler struct {
	methods []string
}

// NewGetRPCMethods returns a cwmp.Handler answering with the given
// method list. The caller (cmd/cpe-sim wiring) passes the names of
// every registered handler plus the CPE-to-ACS methods the session
// itself issues, so the answer always matches what the simulator
// actually accepts.
func NewGetRPCMethods(methods []string) cwmp.Handler {
	return &grmHandler{methods: methods}
}

func (h *grmHandler) Method() string { return "GetRPCMethods" }

func (h *grmHandler) Handle(_ context.Context, req xml.TokenReader, w io.Writer) error {
	// GetRPCMethods has no arguments; drain for decoder consistency.
	drainTokens(req)

	if err := writef(w, "      <MethodList soap-enc:arrayType=\"xsd:string[%d]\">\n", len(h.methods)); err != nil {
		return err
	}
	for _, m := range h.methods {
		if err := writef(w, "        <string>%s</string>\n", m); err != nil {
			return err
		}
	}
	return writef(w, "      </MethodList>\n")
}
