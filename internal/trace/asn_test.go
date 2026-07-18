package trace

import (
	"net"
	"testing"
)

func TestCymruOriginZone(t *testing.T) {
	cases := map[string]string{
		"8.8.8.8":              "8.8.8.8.origin.asn.cymru.com",
		"142.250.170.144":      "144.170.250.142.origin.asn.cymru.com",
		"2001:4860:4860::8888": "8.8.8.8.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.6.8.4.0.6.8.4.1.0.0.2.origin6.asn.cymru.com",
	}
	for in, want := range cases {
		if got := cymruOriginZone(net.ParseIP(in)); got != want {
			t.Errorf("cymruOriginZone(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestRoutableForASN(t *testing.T) {
	skip := []string{"192.168.1.1", "10.0.0.1", "100.64.0.1", "127.0.0.1", "fe80::1"}
	for _, s := range skip {
		if routableForASN(net.ParseIP(s)) {
			t.Errorf("%s should be skipped for ASN lookup", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2607:f8b0:4000::1"} {
		if !routableForASN(net.ParseIP(s)) {
			t.Errorf("%s should be ASN-lookupable", s)
		}
	}
}
