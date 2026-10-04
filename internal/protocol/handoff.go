package protocol

// Worker handoff (CapWorkerHandoff, CapSessionRestart): agentd asks a
// session worker to replace its executable without stopping the agent, and
// the worker tells attached clients it is restarting. See
// internal/worker/handoff.go.

// HandoffSession asks a session worker to hand its session over to
// Executable: it re-executes itself in place (same pid, so the agent stays
// its child) keeping the PTY and its socket, and restores the terminal state
// in the new image. The worker answers HandedOff from the new image, or Error
// from the old one if the handoff did not happen, in which case the session
// keeps running unchanged. Only agentd sends it, to a worker's socket; the
// daemon refuses it from clients.
type HandoffSession struct {
	SessionID  string
	Executable string
}

// HandedOff reports a completed handoff, written by the new image.
type HandedOff struct {
	SessionID string
	// WorkerPID is unchanged by a handoff; it is reported so the caller
	// can tell it is the same process.
	WorkerPID  uint32
	Executable string
	// Handoffs counts the handoffs this session's worker has been through.
	Handoffs uint32
}

// SessionRestarting tells an attached client that the session's worker is
// restarting (for example being handed to a new agentd binary) and the
// stream is about to end. The session itself keeps running: the client
// should attach again, which succeeds once the worker is back. A worker
// sends it only on streams that listed CapSessionRestart.
type SessionRestarting struct {
	SessionID string
	Reason    string
}
