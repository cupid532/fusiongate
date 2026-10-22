package fusiongate

import "net/url"

// ledgerUpstreamAddress records the direct upstream base address, never URL
// credentials, query tokens or fragments. Do not backfill from mutable providers.
func ledgerUpstreamAddress(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
