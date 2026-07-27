// Package codec wraps the BBF TR-369 USP Record (envelope) and Msg (payload)
// protobuf schemas for the agent side.
//
// USP is two nested layers on the wire:
//
//  1. The outer Record carries to_id, from_id, payload_security and one of
//     several record types (no_session_context, session_context,
//     mqtt_connect and friends).
//  2. The inner Msg lives inside Record.NoSessionContext.payload as its own
//     serialized protobuf, and carries Header.msg_id plus a Body that is a
//     request, a response or an error.
//
// A simulated CPE only ever needs the no_session_context form: session_context
// exists for end-to-end encrypted sessions (TR-369 4.1.3), which a simulator
// has no reason to establish. DecodeMessage says so explicitly rather than
// returning a confusing nil.
//
// The .proto files under internal/usp/proto/ are the Broadband Forum
// originals, redistributed under their BSD-3-Clause terms with the notice
// intact. Regenerate the bindings with:
//
//	protoc --proto_path=internal/usp/proto --go_out=. \
//	  --go_opt=Musp-msg-1-5.proto=github.com/ispx-limited/cpe-labs/internal/usp/codec/usp \
//	  --go_opt=Musp-record-1-5.proto=github.com/ispx-limited/cpe-labs/internal/usp/codec/usp_record \
//	  internal/usp/proto/usp-msg-1-5.proto internal/usp/proto/usp-record-1-5.proto
package codec

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp"
	"github.com/ispx-limited/cpe-labs/internal/usp/codec/usp_record"
)

// RecordVersion is the USP version stamped on outbound records. 1.3 is what
// current controllers and obuspa interoperate on; the schema is 1.5, and the
// version field describes the record format rather than the schema file.
const RecordVersion = "1.3"

var (
	// ErrSessionContextUnsupported is returned for records that carry an
	// end-to-end session payload. A simulator never negotiates one.
	ErrSessionContextUnsupported = errors.New("usp/codec: session_context records are not supported by the simulator")

	// ErrNoMessagePayload is returned for record types that carry no inner
	// Msg at all (mqtt_connect, websocket_connect, disconnect). Callers
	// inspect those through Record.GetMqttConnect() and friends.
	ErrNoMessagePayload = errors.New("usp/codec: record type carries no USP message")
)

// DecodeRecord unmarshals raw envelope bytes from the wire into a Record.
func DecodeRecord(b []byte) (*usp_record.Record, error) {
	var rec usp_record.Record
	if err := proto.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("usp/codec: unmarshal record: %w", err)
	}
	return &rec, nil
}

// DecodeMessage extracts and unmarshals the inner Msg from a
// no_session_context Record.
func DecodeMessage(rec *usp_record.Record) (*usp.Msg, error) {
	if rec == nil {
		return nil, errors.New("usp/codec: nil record")
	}
	switch rec.GetRecordType().(type) {
	case *usp_record.Record_NoSessionContext:
		payload := rec.GetNoSessionContext().GetPayload()
		if len(payload) == 0 {
			return nil, errors.New("usp/codec: no_session_context record has an empty payload")
		}
		var msg usp.Msg
		if err := proto.Unmarshal(payload, &msg); err != nil {
			return nil, fmt.Errorf("usp/codec: unmarshal message: %w", err)
		}
		return &msg, nil
	case *usp_record.Record_SessionContext:
		return nil, ErrSessionContextUnsupported
	default:
		return nil, ErrNoMessagePayload
	}
}

// WrapMessage builds a no_session_context Record around msg and serializes the
// whole envelope ready to publish.
//
// payload_security is PLAINTEXT: TR-369 allows it, every MTP we speak runs the
// transport's own TLS, and a simulator has no certificate to sign with.
func WrapMessage(msg *usp.Msg, fromID, toID string) ([]byte, error) {
	if msg == nil {
		return nil, errors.New("usp/codec: nil message")
	}
	if fromID == "" || toID == "" {
		return nil, errors.New("usp/codec: from_id and to_id are both required")
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("usp/codec: marshal message: %w", err)
	}
	rec := &usp_record.Record{
		Version:         RecordVersion,
		ToId:            toID,
		FromId:          fromID,
		PayloadSecurity: usp_record.Record_PLAINTEXT,
		RecordType: &usp_record.Record_NoSessionContext{
			NoSessionContext: &usp_record.NoSessionContextRecord{Payload: payload},
		},
	}
	out, err := proto.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("usp/codec: marshal record: %w", err)
	}
	return out, nil
}
