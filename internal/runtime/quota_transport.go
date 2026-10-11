package runtime

// QuotaSocketConfig is trusted operator configuration for an existing private
// local socket. Socket ownership and the helper process UID are pinned
// separately: an operator may provision a root helper's socket for a supervisor.
type QuotaSocketConfig struct {
	Path                string
	OwnerUID, HelperUID uint32
}

// A concrete transport with a local endpoint checks the entire prepared mount
// inventory, including read-only binds excluded from disk accounting.
type quotaMountGuard interface{ checkQuotaMounts(FS, []Mount) error }
