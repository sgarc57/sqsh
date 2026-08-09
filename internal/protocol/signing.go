package protocol

import (
	"strconv"
	"strings"
)

// signingPrefix is the domain-separation header of the §7.4 encoding.
const signingPrefix = "sqsh-request-v1"

// SigningString returns the canonical §7.4 signing string for r. The encoding
// is a stable protocol contract: changing it is a breaking change.
//
// Signatures are not verified in this release; this exists so the format is
// locked by tests before verification ships.
func SigningString(r Request) string {
	var b strings.Builder
	line := func(s string) {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	line(signingPrefix)
	line(r.RequestID)
	line(r.Target.Host)
	line(r.Command)
	line(strconv.Itoa(len(r.Args)))
	for _, a := range r.Args {
		// Length in bytes, so the prefix is unambiguous for any UTF-8 content.
		line(strconv.Itoa(len(a)) + ":" + a)
	}
	line(FormatTime(r.IssuedAt))
	if r.NotBefore != nil {
		line(FormatTime(*r.NotBefore))
	} else {
		line("-")
	}
	line(FormatTime(r.ExpiresAt))
	line(strconv.Itoa(r.TimeoutSeconds))
	return b.String()
}
