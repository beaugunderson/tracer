package trace

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
)

// asnNameCache memoizes ASN-number → name; the same AS appears on many hops.
var asnNameCache sync.Map

// asnRename shortens a few well-known AS handles for the narrow ASN column.
var asnRename = map[string]string{"SPACEX-STARLINK": "STARLINK"}

// lookupASN returns a short "AS<n> NAME" label for an IP using Team Cymru's
// DNS-based IP-to-ASN service, or "" if the address has no routable origin AS.
//
// It is two TXT lookups: the origin zone maps the IP to its origin ASN, and the
// ASN zone maps that number to a name.
func lookupASN(ctx context.Context, ip net.IP) string {
	if !routableForASN(ip) {
		return ""
	}
	zone := cymruOriginZone(ip)
	if zone == "" {
		return ""
	}
	txts, err := net.DefaultResolver.LookupTXT(ctx, zone)
	if err != nil || len(txts) == 0 {
		return ""
	}
	// e.g. "15169 | 8.8.8.0/24 | US | arin | 1992-12-01"; field 1 may list
	// several space-separated ASNs for a multi-origin prefix — take the first.
	asField := strings.TrimSpace(strings.SplitN(txts[0], "|", 2)[0])
	fields := strings.Fields(asField)
	if len(fields) == 0 || !isAllDigits(fields[0]) {
		return ""
	}
	num := fields[0]
	if name := asnName(ctx, num); name != "" {
		return name
	}
	return "AS" + num // unnamed AS: fall back to the number
}

func asnName(ctx context.Context, num string) string {
	if v, ok := asnNameCache.Load(num); ok {
		return v.(string)
	}
	name := ""
	if txts, err := net.DefaultResolver.LookupTXT(ctx, "AS"+num+".asn.cymru.com"); err == nil && len(txts) > 0 {
		// e.g. "15169 | US | arin | 2000-03-30 | GOOGLE, US"; take the last
		// field and drop the trailing ", CC" country suffix.
		parts := strings.Split(txts[0], "|")
		name = strings.TrimSpace(parts[len(parts)-1])
		if i := strings.LastIndex(name, ","); i > 0 {
			name = strings.TrimSpace(name[:i]) // drop trailing ", CC" country
		}
		if i := strings.Index(name, " - "); i > 0 {
			name = strings.TrimSpace(name[:i]) // keep the short handle, drop "- Org Name"
		}
		if short, ok := asnRename[name]; ok {
			name = short
		}
	}
	asnNameCache.Store(num, name)
	return name
}

// cymruOriginZone builds the reversed-IP query name for the Cymru origin lookup.
func cymruOriginZone(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", v4[3], v4[2], v4[1], v4[0])
	}
	v6 := ip.To16()
	if v6 == nil {
		return ""
	}
	var sb strings.Builder
	for i := len(v6) - 1; i >= 0; i-- {
		fmt.Fprintf(&sb, "%x.%x.", v6[i]&0x0f, v6[i]>>4)
	}
	sb.WriteString("origin6.asn.cymru.com")
	return sb.String()
}

// routableForASN skips addresses that have no public origin AS (RFC1918, CGNAT,
// loopback, link-local), avoiding pointless "NA" lookups.
func routableForASN(ip net.IP) bool {
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false // 100.64.0.0/10 carrier-grade NAT
	}
	return true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
