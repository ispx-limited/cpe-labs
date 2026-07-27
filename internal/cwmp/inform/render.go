package inform

import (
	"fmt"
	"io"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// Render writes the cwmp:Inform body content (everything inside the
// outer cwmp:Inform start/end tags, exclusive of those tags) to w.
// Method-element framing comes from internal/cwmp/soap.
//
// Render is a pure function: it reads only inf and writes only to w.
// Concurrent calls on the same *Inform are safe.
func Render(w io.Writer, inf *Inform) error {
	if inf == nil {
		return cpeerr.Wrap("inform.Render", cpeerr.KindInvalidArgument,
			fmt.Errorf("inf is nil"))
	}

	bw := &writer{w: w}
	bw.writeDeviceID(inf.DeviceID)
	bw.writeEvents(inf.Events)
	bw.writef("      <MaxEnvelopes>%d</MaxEnvelopes>\n", maxEnvelopes(inf.MaxEnvelopes))
	bw.writef("      <CurrentTime>%s</CurrentTime>\n", inf.CurrentTime.UTC().Format(time.RFC3339))
	bw.writef("      <RetryCount>%d</RetryCount>\n", inf.RetryCount)
	if err := bw.writeParameters(inf.Parameters); err != nil {
		return err
	}
	if bw.err != nil {
		return cpeerr.Wrap("inform.Render", cpeerr.KindInternal, bw.err)
	}
	return nil
}

func maxEnvelopes(v uint) uint {
	if v == 0 {
		return 1
	}
	return v
}

type writer struct {
	w   io.Writer
	err error
}

func (b *writer) writef(format string, args ...any) {
	if b.err != nil {
		return
	}
	if _, err := fmt.Fprintf(b.w, format, args...); err != nil {
		b.err = err
	}
}

func (b *writer) write(s string) {
	if b.err != nil {
		return
	}
	if _, err := b.w.Write([]byte(s)); err != nil {
		b.err = err
	}
}

func (b *writer) writeDeviceID(d DeviceID) {
	b.write("      <DeviceId>\n")
	b.writef("        <Manufacturer>%s</Manufacturer>\n", escape(d.Manufacturer))
	b.writef("        <OUI>%s</OUI>\n", escape(d.OUI))
	b.writef("        <ProductClass>%s</ProductClass>\n", escape(d.ProductClass))
	b.writef("        <SerialNumber>%s</SerialNumber>\n", escape(d.SerialNumber))
	b.write("      </DeviceId>\n")
}

func (b *writer) writeEvents(events []Event) {
	if len(events) == 0 {
		b.write("      <Event></Event>\n")
		return
	}
	b.write("      <Event>\n")
	for _, e := range events {
		b.write("        <EventStruct>\n")
		b.writef("          <EventCode>%s</EventCode>\n", escape(e.EventCode))
		b.writef("          <CommandKey>%s</CommandKey>\n", escape(e.CommandKey))
		b.write("        </EventStruct>\n")
	}
	b.write("      </Event>\n")
}

func (b *writer) writeParameters(params []Parameter) error {
	if len(params) == 0 {
		b.write("      <ParameterList></ParameterList>\n")
		return nil
	}
	b.write("      <ParameterList>\n")
	for _, p := range params {
		canon, err := paramtree.Marshal(p.Value.Type, p.Value.Raw)
		if err != nil {
			return cpeerr.Wrap("inform.Render", cpeerr.KindInvalidArgument,
				fmt.Errorf("parameter %s: %w", p.Name, err))
		}
		b.write("        <ParameterValueStruct>\n")
		b.writef("          <Name>%s</Name>\n", escape(p.Name))
		b.writef("          <Value xsi:type=\"%s\">%s</Value>\n", p.Value.Type, escape(canon))
		b.write("        </ParameterValueStruct>\n")
	}
	b.write("      </ParameterList>\n")
	return nil
}

// escape XML-escapes the small set of characters that can break
// well-formedness inside element text.
func escape(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			out = append(out, []byte("&amp;")...)
		case '<':
			out = append(out, []byte("&lt;")...)
		case '>':
			out = append(out, []byte("&gt;")...)
		case '"':
			out = append(out, []byte("&quot;")...)
		case '\'':
			out = append(out, []byte("&apos;")...)
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
