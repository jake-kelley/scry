package menubar

import (
	"time"

	"scry/internal/ipc"
)

// Options configures Run. Every field is caller-supplied config or an
// address of something already running (the daemon core and internal/web
// server); Run and its darwin implementation never load config.toml or
// start the daemon themselves — cmd/scry's menubar command owns that, the
// same split main.go already keeps between loading config and running a
// command.
type Options struct {
	// Addr is where the daemon this process is running (see internal/ipc)
	// answers "search"/"status"/"reindex"/"stop". Every menu action goes
	// through here, never through an index.Shard directly.
	Addr ipc.Addr

	// WebAddr is internal/web's bind address; it must already be serving
	// by the time Run is called. Search… opens http://WebAddr/ in the
	// user's default browser (§7 option 1).
	WebAddr string

	// PollInterval is how often the live indexed-file count refreshes.
	// <= 0 uses DefaultPollInterval.
	PollInterval time.Duration
}

// DefaultPollInterval is how often the status menu item's count refreshes
// when Options.PollInterval is unset.
const DefaultPollInterval = 2 * time.Second

func (o Options) pollInterval() time.Duration {
	if o.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return o.PollInterval
}
