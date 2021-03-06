package verify

import (
	"fmt"
	"testing"

	"github.com/thinkermao/bior/raft/core/peer"
	"github.com/thinkermao/bior/simu/env"
	"github.com/thinkermao/bior/simu/raft"
)

func TestRaft_InitialElection(t *testing.T) {
	peer.Simulation = true
	servers := 3
	env := envior.MakeEnvironment(t, servers, false)
	defer env.Cleanup()

	fmt.Printf("Test: initial election ...\n")

	// is a leader elected?
	env.CheckOneLeader()

	// does the leader+term stay the same if there is no network failure?
	term1 := env.CheckTerms()
	sleep(3 * raft.ElectionTimeout)
	term2 := env.CheckTerms()
	if term1 != term2 {
		fmt.Printf("warning: term changed even though there were no failures")
	}

	fmt.Printf("  ... Passed\n")
}

func TestRaft_PreVoteReject(t *testing.T) {
	peer.Simulation = true
	servers := 3
	env := envior.MakeEnvironment(t, servers, false)
	defer env.Cleanup()

	fmt.Printf("Test: no election majority peer online ...\n")

	leader1 := env.CheckOneLeader()
	term1 := env.CheckTerms()

	// if the one disconnects, no election should be propose.
	env.Disconnect((leader1 + 1) % servers)

	// wait node become preVote state.
	sleep(2 * raft.ElectionTimeout)

	// node rejoin, pre vote request should be rejected.
	env.Connect((leader1 + 1) % servers)

	sleep(2 * raft.ElectionTimeout)

	leader2 := env.CheckOneLeader()
	term2 := env.CheckTerms()
	if leader1 != leader2 || term1 != term2 {
		fmt.Printf("there's quorum, no election should be propose")
	}
	fmt.Printf("  ... Passed\n")
}

func TestRaft_ReElection(t *testing.T) {
	peer.Simulation = true
	servers := 3
	env := envior.MakeEnvironment(t, servers, false)
	defer env.Cleanup()

	fmt.Printf("Test: election after network failure ...\n")

	leader1 := env.CheckOneLeader()

	// if the leader disconnects, a new One should be elected.
	env.Disconnect(leader1)
	leader2 := env.CheckOneLeader()

	// if the old leader rejoins, that shouldn't disturb the old leader.
	env.Connect(leader1)
	sleep(3 * raft.HeartbeatTimeout)
	if leader := env.CheckOneLeader(); leader != leader2 {
		t.Fatal("old leader rejoins, but leader changed from ",
			leader2, " to ", leader)
	}
	if _, isLeader := env.GetState(leader1); isLeader {
		t.Fatal("old leader should lost leadership because expired term")
	}

	// if there's no quorum, no leader should be elected.
	env.Disconnect(leader2)
	env.Disconnect((leader2 + 1) % servers)
	sleep(3 * raft.ElectionTimeout)
	env.CheckNoLeader()

	// if a quorum arises, it should elect a leader.
	env.Connect((leader2 + 1) % servers)
	env.CheckOneLeader()

	// re-join of last node shouldn't prevent leader from existing.
	env.Connect(leader2)
	env.CheckOneLeader()

	fmt.Printf("  ... Passed\n")
}

// Followers should reject new leaders, if from their point of
// view the existing leader is still functioning correctly
func TestRaft_LeaderStickness(t *testing.T) {
	peer.Simulation = true
	servers := 3
	env := envior.MakeEnvironment(t, servers, false)
	defer env.Cleanup()

	fmt.Printf("Test: leader stickness ...\n")

	// leader network failure
	leader1 := env.CheckOneLeader()
	env.Disconnect(leader1)

	// new leader network failure
	leader2 := env.CheckOneLeader()
	env.Disconnect(leader2)

	// old leader connected again
	env.Connect(leader1)

	// wait for new elected leader
	leader3 := env.CheckOneLeader()

	// all together now
	env.Connect(leader2)

	sleep(2 * raft.ElectionTimeout)

	if leader := env.CheckOneLeader(); leader != leader3 {
		t.Fatal("leader flip")
	}

	fmt.Printf("  ... Passed\n")
}
