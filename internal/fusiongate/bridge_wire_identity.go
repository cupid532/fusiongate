package fusiongate

import (
	"strconv"
	"strings"
)

// wireCredentialIdentity names the account behind a route so protocol learning
// can be keyed on it.
//
// The memory key used to carry the access token itself, which made every OAuth
// refresh start from an empty learning state: the entry recorded before the
// refresh could no longer be looked up, so the channel paid for the same failed
// native attempt again on the first request after every refresh, and the
// abandoned entries stayed in the map until the TTL swept them. Subscription
// channels refresh routinely (FusionGate refreshes 15 minutes ahead of expiry),
// so the 30-minute memory was in practice much shorter for exactly the channels
// that need it most. An endpoint's protocol support is a property of the
// channel and the account, never of the current token.
//
// Different accounts of one channel are still kept apart, because entitlements
// can differ between them (a second subscription may not carry Responses
// access), which is also why a provider Key is preferred over a route-wide
// credential: keys are stable rows while tokens are not.
func wireCredentialIdentity(z resolvedRoute) string {
	if z.ProviderKeyID > 0 {
		return "key:" + strconv.FormatInt(z.ProviderKeyID, 10)
	}
	credential := z.AuthCredential
	if credential == nil {
		return ""
	}
	if account := strings.TrimSpace(credential.AccountID); account != "" {
		return "account:" + account
	}
	if email := strings.ToLower(strings.TrimSpace(credential.Email)); email != "" {
		return "email:" + email
	}
	// Last resort: the credential shape. Two tokens of one platform on one
	// channel are interchangeable for protocol learning, and this stays stable
	// across their refreshes.
	return "kind:" + strings.ToLower(strings.TrimSpace(credential.Kind)) + "/" + strings.ToLower(strings.TrimSpace(credential.Platform))
}
