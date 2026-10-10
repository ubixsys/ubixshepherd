package chat

// Feed kinds the daemon sends once its run outcomes, commits and gate are told apart.
// The strings are both the store's raw kinds and their api.Event* values (see
// internal/api/events.go on dev). They are named here so the chat works whether or not
// this build's internal/api knows them, and with an older daemon that never sends them.
const (
	kindRunPassed      = "run_passed"
	kindRunFailed      = "run_failed"
	kindRunInterrupted = "run_interrupted" // stopped by the person, or cut short
	kindRunQuota       = "run_quota"       // the agent is out of quota and held
	kindCommit         = "commit"          // a run ended having made commits
	kindGate           = "gate"            // Shepherd's own check before a push
	kindDeskRotated    = "desk_rotated"    // the front desk's session was rotated
)
