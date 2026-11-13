package cronet

import (
	"errors"
	"fmt"
	"strings"
)

// SetALPN sets this engine's HTTP protocols before startup. Empty restores the
// default policy. ["h3"] selects QUIC exclusively; otherwise an ordered subset
// of ["h2", "http/1.1"] selects TCP/TLS and disables QUIC. Unsupported protocols,
// duplicate entries and mixing h3 with TCP protocols are rejected.
func (e Engine) SetALPN(protocols []string) error {
	if err := validateALPN(protocols); err != nil {
		return err
	}
	return e.setALPN(strings.Join(protocols, ","))
}

func validateALPN(protocols []string) error {
	seen := make(map[string]bool, len(protocols))
	for _, protocol := range protocols {
		switch protocol {
		case "h2", "http/1.1", "h3":
		default:
			return fmt.Errorf("cronet: unsupported ALPN protocol %q", protocol)
		}
		if seen[protocol] {
			return fmt.Errorf("cronet: duplicate ALPN protocol %q", protocol)
		}
		seen[protocol] = true
	}
	if seen["h3"] && len(protocols) != 1 {
		return errors.New("cronet: h3 ALPN cannot be combined with TCP protocols")
	}
	return nil
}

func (o BrowserXHTTPOptions) http3() bool {
	return len(o.ALPN) == 1 && o.ALPN[0] == "h3"
}
