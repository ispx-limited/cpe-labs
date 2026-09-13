package mqtt

import "testing"

func TestWithReplyTo_EscapesOnlySlashes(t *testing.T) {
	got := withReplyTo("usp/v1/controller/os::00256D-01", "usp/v1/agent/os::00256D-01")
	if want := "usp/v1/controller/os::00256D-01/reply-to=usp%2Fv1%2Fagent%2Fos::00256D-01"; got != want {
		t.Errorf("withReplyTo = %q, want %q", got, want)
	}
}

func TestParseReplyTo(t *testing.T) {
	cases := []struct{ topic, want string }{
		{"usp/v1/agent/os::00256D-01", ""},
		{"usp/v1/agent/os::00256D-01/reply-to=usp%2Fv1%2Fcontroller%2Fself::herder", "usp/v1/controller/self::herder"},
		{"usp/v1/agent/os::00256D-01/reply-to=first/reply-to=usp%2Fsecond", "usp/second"},
		{"usp/v1/agent/os::00256D-01/reply-to=usp%2Fself::a%2Db", "usp/self::a%2Db"},
	}
	for _, tc := range cases {
		if got := parseReplyTo(tc.topic); got != tc.want {
			t.Errorf("parseReplyTo(%q) = %q, want %q", tc.topic, got, tc.want)
		}
	}
}

func TestReplyTo_RoundTripsEndpointIDs(t *testing.T) {
	for _, replyTo := range []string{
		"usp/v1/controller/os::00256D-0123456789",
		"usp/v1/controller/oui:00256D:my-unique-bbf-id-42",
		"usp/v1/controller/self::my%2DAgent",
	} {
		topic := withReplyTo("usp/v1/agent/os::00256D-01", replyTo)
		if got := parseReplyTo(topic); got != replyTo {
			t.Errorf("parseReplyTo(withReplyTo(%q)) = %q", replyTo, got)
		}
	}
}
