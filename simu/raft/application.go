package raft

// Application provides a raft implements.
type Application interface {
	ID() int
	Kill()
	Start(nodes []uint64) error
	Shutdown()
	Propose(data int) (uint64, uint64, bool)

	GetState() (uint64, bool)
	ApplyError() error

	LogLength() int
	LogAt(index int) (int, bool)

	SendHeartbeat(end int)
	Disconnect(end int)
}