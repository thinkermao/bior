package envior

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/thinkermao/bior/simu/raft"
	"github.com/thinkermao/network-simu-go"
)

const walDir = "./wal_log/"

// Environment support Environment for test.
type Environment struct {
	t          *testing.T
	net        network.Network
	totalNodes int
	apps       []raft.Application
}

// MakeEnvironment return instance of Environment.
func MakeEnvironment(t *testing.T, num int, unrealiable bool) *Environment {
	builder := network.CreateBuilder()
	env := &Environment{}
	// create a full set of Rafts.
	var apps []raft.Application
	for i := 0; i < num; i++ {
		dir := filepath.Join(walDir, strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0777); err != nil {
			panic(err)
		}

		handler := builder.AddEndpoint()
		apps = append(apps, raft.MakeApp(dir, handler, env))
	}

	env.t = t
	env.net = builder.Build()
	env.totalNodes = num
	env.apps = apps

	// Connect everyone
	for i := 0; i < num; i++ {
		env.Start1(i)
		env.Connect(i)
	}

	env.net.SetReliable(!unrealiable)

	return env
}

// CheckApply check consistency of applied entries.
func (env *Environment) CheckApply(id, index, value int) error {
	for j := 0; j < len(env.apps); j++ {
		app := env.apps[j]
		if v, ok := app.LogAt(index); ok && v != value {
			// some server has already committed a different value for this entry!
			return fmt.Errorf("commit index=%v server=%v %v != server=%v %v",
				index, id, value, app.ID(), v)
		}
	}
	return nil
}

// Crash1 shut down a Raft server but save its persistent state.
func (env *Environment) Crash1(i int) {
	env.Disconnect(i)
	env.apps[i].Shutdown()
}

// Start1 start or re-start a Raft.
// if One already exists, "kill" it first.
func (env *Environment) Start1(i int) {
	env.Crash1(i)

	/* read all nodes netId */
	ns := make([]uint64, 0)
	for i := 0; i < len(env.apps); i++ {
		ns = append(ns, uint64(env.apps[i].ID()))
	}

	if err := env.apps[i].Start(ns); err != nil {
		env.t.Fatal("start node", i, "error:", err)
	}
}

// Propose send propose to raft.
func (env *Environment) Propose(id int, num int) (uint64, uint64, bool) {
	return env.apps[id].Propose(num)
}

// GetState return the state of raft.
func (env *Environment) GetState(id int) (uint64, bool) {
	return env.apps[id].GetState()
}

// Cleanup kill all data
func (env *Environment) Cleanup() {
	for i := 0; i < len(env.apps); i++ {
		if env.apps[i] != nil {
			env.apps[i].Shutdown()
		}
	}
	if err := os.RemoveAll(walDir); err != nil {
		panic(err)
	}
}

// Connect  attach server i to the net.
func (env *Environment) Connect(i int) {
	// fmt.Printf("Connect(%d)\n", i)

	env.net.Enable(i)
}

// Disconnect detach server i from the net.
func (env *Environment) Disconnect(i int) {
	// fmt.Printf("Disconnect(%d)\n", i)

	env.net.Disable(i)
}

// GetCount how many counts of network call.
func (env *Environment) GetCount(server int) int {
	return int(env.net.GetCount(server))
}

// SetUnreliable make network become unrealiable.
func (env *Environment) SetUnreliable(unrel bool) {
	env.net.SetReliable(!unrel)
}

// SetLongDelay make network has randomized long delay.
func (env *Environment) SetLongDelay(longDelay bool) {
	env.net.SetLongDelays(longDelay)
}

// CheckOneLeader check that there's exactly One leader.
// try a few times in case re-elections are needed.
func (env *Environment) CheckOneLeader() int {
	for iters := 0; iters < 10; iters++ {
		time.Sleep(raft.ElectionTimeout * time.Millisecond)
		leaders := make(map[int][]int)
		for i := 0; i < env.totalNodes; i++ {
			if env.net.IsEnable(i) {
				if t, leader := env.apps[i].GetState(); leader {
					leaders[int(t)] = append(leaders[int(t)], i)
				}
			}
		}

		lastTermWithLeader := -1
		for t, leaders := range leaders {
			if len(leaders) > 1 {
				env.t.Fatalf("term %d has %d (>1) leaders", t, len(leaders))
			}
			if t > lastTermWithLeader {
				lastTermWithLeader = t
			}
		}

		if len(leaders) != 0 {
			return leaders[lastTermWithLeader][0]
		}
	}
	env.t.Fatalf("expected One leader, got none")
	return -1
}

// CheckTerms check that everyone agrees on the term.
func (env *Environment) CheckTerms() int {
	term := -1
	for i := 0; i < env.totalNodes; i++ {
		if env.net.IsEnable(i) {
			xterm, _ := env.apps[i].GetState()
			if term == -1 {
				term = int(xterm)
			} else if term != int(xterm) {
				env.t.Fatalf("servers disagree on term")
			}
		}
	}
	return term
}

// CheckNoLeader check that there's no leader
func (env *Environment) CheckNoLeader() {
	for i := 0; i < env.totalNodes; i++ {
		if env.net.IsEnable(i) {
			_, isLeader := env.apps[i].GetState()
			if isLeader {
				env.t.Fatalf("expected no leader, but %v claims to be leader", i)
			}
		}
	}
}

// CommittedNumber how many servers think a log entry is committed?
func (env *Environment) CommittedNumber(index int) (int, int) {
	count := 0
	cmd := -1
	for i := 0; i < len(env.apps); i++ {
		if err := env.apps[i].ApplyError(); err != nil {
			env.t.Fatal(err)
		}

		if value, ok := env.apps[i].LogAt(index); ok {
			if count > 0 && cmd != value {
				env.t.Fatalf("committed values do not match: index %v, %v, %v\n",
					index, cmd, value)
			}
			count++
			cmd = value
		}
	}
	// fmt.Println(index, "committed number", count, "value", cmd)
	return count, cmd
}

// Wait for at least n servers to commit.
// but don't Wait forever.
func (env *Environment) Wait(index int, n int, startTerm int) interface{} {
	to := 10 * time.Millisecond
	for iters := 0; iters < 30; iters++ {
		nd, _ := env.CommittedNumber(index)
		if nd >= n {
			break
		}
		time.Sleep(to)
		if to < time.Second {
			to *= 2
		}
		if startTerm > -1 {
			for _, r := range env.apps {
				if t, _ := r.GetState(); int(t) > startTerm {
					// someone has moved on
					// can no longer guarantee that we'll "win"
					return -1
				}
			}
		}
	}
	nd, cmd := env.CommittedNumber(index)
	if nd < n {
		env.t.Fatalf("only %d decided for index %d; wanted %d\n",
			nd, index, n)
	}
	return cmd
}

// One do a complete agreement.
// it might choose the wrong leader initially,
// and have to re-submit after giving up.
// entirely gives up after about 10 seconds.
// indirectly checks that the servers agree on the
// same value, since CommittedNumber() checks this,
// as do the threads that read from applyCh.
// returns index.
func (env *Environment) One(cmd int, expectedServers int) int {
	t0 := time.Now()
	starts := 0
	for time.Since(t0).Seconds() < 10 {
		// try all the servers, maybe One is the leader.
		index := -1
		for si := 0; si < env.totalNodes; si++ {
			starts = (starts + 1) % env.totalNodes
			if index1, _, ok := env.apps[starts].Propose(cmd); ok {
				index = int(index1)
				break
			}
		}

		if index != -1 {
			// somebody claimed to be the leader and to have
			// submitted our command; Wait a while for agreement.
			t1 := time.Now()
			for time.Since(t1).Seconds() < 2 {
				if nd, cmd1 := env.CommittedNumber(index); nd > 0 && nd >= expectedServers {
					// committed
					if cmd1 == cmd {
						// and it was the command we submitted.
						return index
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	env.t.Fatalf("One(%v) failed to reach agreement", cmd)
	return -1
}

func (env *Environment) GenSnapshot(id int) (uint64, uint64) {
	return env.apps[id].GenSnapshot()
}

func (env *Environment) IsCrash(id int) bool {
	return env.apps[id].IsCrash()
}
