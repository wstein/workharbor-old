//go:build !linux

package runtime

// NewUnixQuotaResolver has no authenticated host-helper transport on this host.
// A remote Docker VM bridge is deliberately not inferred or started.
func NewUnixQuotaResolver(QuotaSocketConfig) (QuotaResolver, error) {
	return nil, quotaUnsupported()
}
