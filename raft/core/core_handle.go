package core

import (
	log "github.com/sirupsen/logrus"
	"github.com/thinkermao/bior/raft/core/conf"
	"github.com/thinkermao/bior/raft/core/peer"
	"github.com/thinkermao/bior/raft/core/read"
	"github.com/thinkermao/bior/raft/proto"
	"github.com/thinkermao/bior/utils"
)

func (c *core) stepLeader(msg *raftpd.Message) {
	switch msg.MsgType {
	case raftpd.MsgHeartbeatResponse:
		c.handleHeartbeatResponse(msg)
	case raftpd.MsgSnapshotResponse:
		c.handleSnapshotResponse(msg)
	case raftpd.MsgAppendResponse:
		c.handleAppendEntriesResponse(msg)
	case raftpd.MsgUnreachable:
		c.handleUnreachable(msg)
	case raftpd.MsgReadIndexRequest:
		c.handleReadIndexRequest(msg)
	}
}

func (c *core) stepFollower(msg *raftpd.Message) {
	switch msg.MsgType {
	case raftpd.MsgReadIndexResponse:
		c.becomeFollower(c.term, msg.From)
		c.handleReadIndexResponse(msg)
	case raftpd.MsgAppendRequest:
		c.becomeFollower(c.term, msg.From)
		c.handleAppendEntries(msg)
	case raftpd.MsgHeartbeatRequest:
		c.becomeFollower(c.term, msg.From)
		c.handleHeartbeat(msg)
	case raftpd.MsgSnapshotRequest:
		c.becomeFollower(c.term, msg.From)
		c.handleSnapshot(msg)
	}
}

func (c *core) stepCandidate(msg *raftpd.Message) {
	switch msg.MsgType {
	// Only handle vote responses corresponding to our candidacy (while in
	// StateCandidate, we may get stale MsgPreVoteResp messages in this term from
	// our pre-candidate state).
	case raftpd.MsgPreVoteResponse:
		if c.state == RolePrevCandidate {
			c.handleVoteResponse(msg)
		}
	case raftpd.MsgVoteResponse:
		if c.state == RoleCandidate {
			c.handleVoteResponse(msg)
		}

		// If a candidate receives an AppendEntries RPC from another rpc claiming
		// to be leader whose term is at least as large as the candidate's current term,
		// it recognizes the leader as legitimate and returns to follower state.
	case raftpd.MsgAppendRequest:
		c.backToFollower(msg.Term, msg.From)
		c.handleAppendEntries(msg)
	case raftpd.MsgHeartbeatRequest:
		c.backToFollower(msg.Term, msg.From)
		c.handleHeartbeat(msg)
	case raftpd.MsgSnapshotRequest:
		c.backToFollower(msg.Term, msg.From)
		c.handleSnapshot(msg)
	}
}

func (c *core) dispatch(msg *raftpd.Message) {
	switch c.state {
	case RoleLeader:
		c.stepLeader(msg)
	case RoleFollower:
		c.stepFollower(msg)
	case RolePrevCandidate:
		fallthrough
	case RoleCandidate:
		c.stepCandidate(msg)
	}
}

func (c *core) handleReadIndexRequest(msg *raftpd.Message) {
	utils.Assert(c.state.IsLeader(), "receive wrong message")
	// c must be leader, so term great than InvalidTerm.
	if c.log.Term(c.log.CommitIndex()) != c.term {
		// Reject read only request when this leader has not
		// committed any log entry at its term. (raft thesis 6.4)
		return
	}

	c.readOnly.AddRequest(c.log.CommitIndex(), msg.From, msg.Context)

	if c.quorum() > 1 {
		c.broadcastHeartbeatWithCtx(msg.Context)
	} else {
		c.advanceReadOnly(msg.Context)
	}
}

func (c *core) handleReadIndexResponse(msg *raftpd.Message) {
	log.Debugf("%d [Term: %d, commit: %d] receive read index response from: %d [idx: %d]",
		c.id, c.term, msg.From, msg.Index)

	// TODO: check
	readState := read.ReadState{
		Index:      msg.Index,
		RequestCtx: msg.Context,
	}

	c.callback.saveReadState(&readState)
}

// RPC:
// - AppendEntries(commitIndex, prevLogIndex, prevLogTerm, entries)
// - AppendEntriesReply(index, hint, reject)
func (c *core) handleAppendEntries(msg *raftpd.Message) {
	reply := raftpd.Message{
		MsgType: raftpd.MsgAppendResponse,
		To:      msg.From,
	}
	if c.log.CommitIndex() > msg.LogIndex {
		log.Debugf("%d [Term: %d, commit: %d] reject expired append Entries "+
			"from %d [logterm: %d, idx: %d]", c.id, c.term, c.log.CommitIndex(),
			msg.From, msg.LogTerm, msg.LogIndex)

		// expired append Entries has been committed,
		// so it reply same with success append.
		reply.Index = msg.LogIndex
		reply.RejectHint = c.log.CommitIndex()
		reply.Reject = false
	} else if idx, ok := c.log.TryAppend(msg.LogIndex, msg.LogTerm, msg.Entries); ok {
		log.Debugf("%d [Term: %d, commit: %d] accept append Entries "+
			"from %d [logterm: %d, idx: %d], last: %d", c.id, c.term, c.log.CommitIndex(),
			msg.From, msg.LogTerm, msg.LogIndex, idx)

		// If leaderCommit > commitIndex, set commitIndex =
		// min(leaderCommit, index of last new entry)
		c.log.CommitTo(utils.MinUint64(msg.Index, idx))
		reply.Index = msg.LogIndex
		reply.RejectHint = idx /* idx is index of latest log entry */
		reply.Reject = false
	} else {
		log.Debugf("%d [logterm: %d, commit: %d, last idx: %d] rejected msgApp "+
			"[logterm: %d, idx: %d] and hint %d from %d", c.id, c.log.Term(msg.LogIndex),
			c.log.CommitIndex(), c.log.LastIndex(), msg.LogTerm, msg.LogIndex, idx, msg.From)

		reply.Index = msg.LogIndex
		reply.RejectHint = idx /* idx is hintIndex*/
		reply.Reject = true
	}
	c.send(&reply)
}

func (c *core) handleAppendEntriesResponse(msg *raftpd.Message) {
	log.Debugf("%d received append entries response from %d [rj: %v, idx: %d, hint: %d]",
		c.id, msg.From, msg.Reject, msg.Index, msg.RejectHint)

	node := c.getNodeByID(msg.From)
	successAppend := node.HandleAppendEntries(msg.Reject, msg.Index, msg.RejectHint)
	if successAppend {
		c.poll(node.Matched)
	}
}

func (c *core) tryRestore(snapshot *raftpd.Snapshot) bool {
	utils.Assert(snapshot != nil, "nullptr exception")

	if snapshot.Metadata.Index <= c.log.CommitIndex() {
		/* expired snapshot install */
		return false
	}

	if c.log.Term(snapshot.Metadata.Index) == snapshot.Metadata.Term {
		// FIXME: c.log.CommitTo(snapshot.Metadata.Index)
		return false
	}

	return true
}

func (c *core) handleSnapshot(msg *raftpd.Message) {
	reply := raftpd.Message{
		MsgType: raftpd.MsgSnapshotResponse,
		To:      msg.From,
		Reject:  false,
	}
	if c.tryRestore(msg.Snapshot) {
		log.Debugf("%x [commit: %d] restore snapshot [index: %d, term: %d]",
			c.id, c.log.CommitIndex(),
			msg.Snapshot.Metadata.Index, msg.Snapshot.Metadata.Term)

		// FIXME: maybe blocked or compact before it return.
		c.callback.applySnapshot(msg.Snapshot)
		c.ApplySnapshot(&msg.Snapshot.Metadata)

		reply.Index = msg.Snapshot.Metadata.Index
		reply.RejectHint = c.log.LastIndex()
	} else {
		log.Debugf("%x [commit: %d] ignored snapshot [index: %d, term: %d]",
			c.id, c.log.CommitIndex(),
			msg.Snapshot.Metadata.Index, msg.Snapshot.Metadata.Term)

		reply.Index = msg.Snapshot.Metadata.Index
		reply.RejectHint = c.log.CommitIndex()
	}
	c.send(&reply)
}

func (c *core) handleSnapshotResponse(msg *raftpd.Message) {
	log.Debugf("%d received snapshot response from %d [rj: %v, idx: %d, hint: %d]",
		c.id, msg.From, msg.Reject, msg.Index, msg.RejectHint)

	node := c.getNodeByID(msg.From)
	node.HandleSnapshot(msg.Reject, msg.Index, msg.RejectHint)
}

func (c *core) handleUnreachable(msg *raftpd.Message) {
	node := c.getNodeByID(msg.From)

	node.HandleUnreachable()
	log.Infof("%d failed to send message to %d"+
		" because it is unreachable", c.id, msg.From)
}

func (c *core) handleHeartbeat(msg *raftpd.Message) {
	log.Debugf("%d [term: %d] handle heartbeat request from %d", c.id, c.term, msg.From)

	c.log.CommitTo(msg.Index)

	reply := raftpd.Message{
		To:      msg.From,
		Reject:  false,
		MsgType: raftpd.MsgHeartbeatResponse,
		Context: msg.Context,
	}
	c.send(&reply)
}

func (c *core) handleHeartbeatResponse(msg *raftpd.Message) {
	log.Debugf("%d [term: %d] handle heartbeat response from %d", c.id, c.term, msg.From)
	ackCount := c.readOnly.ReceiveAck(msg.From, msg.Context)
	if ackCount < c.quorum() {
		return
	}
	log.Debugf("%d [term: %d] handle heartbeat response from %d", c.id, c.term, msg.From)

	c.advanceReadOnly(msg.Context)
}

func (c *core) voteStateCount(state peer.VoteState) int {
	var count = 0
	for i := 0; i < len(c.nodes); i++ {
		if c.nodes[i].Vote == state {
			count++
		}
	}
	return count
}

func (c *core) handlePreVote(msg *raftpd.Message) {
	reply := raftpd.Message{
		To:      msg.From,
		MsgType: raftpd.MsgPreVoteResponse,
	}
	// Leader Stickness property:
	// 	Followers should reject new leaders, if from their point of view the
	// existing leader is still functioning correctly.
	//
	// Reply false if last AppendEntries call was received less than election timeout ago.
	// Reply false if term < currentTerm.
	// Reply false if candidate's log isn't at least as up­to­date as receiver's log.
	if (c.leaderID != conf.InvalidID && c.timeElapsed < c.electionTick) ||
		(msg.Term < c.term) ||
		!c.log.IsUpToDate(msg.LogIndex, msg.LogTerm) {
		/* rejected */
		reply.Reject = true
		reply.Term = c.term
		log.Infof("%d [term: %d] reject pre vote request from %d", c.id, c.term, msg.From)
	} else {
		// When responding to MsgPreVote messages we include the term
		// from the message, not the local term. To see why consider the
		// case where a single node was previously partitioned away and
		// it's local term is now of date. If we include the local term
		// (recall that for pre-votes we don't update the local term), the
		// pre-campaigning node on the other end will proceed to ignore
		// the message (it ignores all out of date messages).
		// The term in the original message and current local term are the
		// same in the case of regular votes, but different for pre-votes.

		/* accept */
		reply.Reject = false
		reply.Term = msg.Term
		log.Infof("%d [term: %d] accept vote request from %d", c.id, c.term, msg.From)
	}

	c.send(&reply)
}

func (c *core) handleVote(msg *raftpd.Message) {
	reply := raftpd.Message{
		To:      msg.From,
		MsgType: raftpd.MsgVoteResponse,
	}

	// If votedFor is null or candidateId, and candidate’s log is at
	// least as up-to-date as receiver’s log, grant vote (§5.2, §5.4)
	if (c.vote == conf.InvalidID || c.vote == msg.From) &&
		c.log.IsUpToDate(msg.LogIndex, msg.LogTerm) {
		reply.Reject = false
		c.vote = msg.From
		log.Infof("%d [term: %d] accepted vote request from %d", c.id, c.term, msg.From)
	} else {
		reply.Reject = true
		log.Infof("%d [term: %d] reject vote request from %d", c.id, c.term, msg.From)
	}

	c.send(&reply)
}

func (c *core) handleVoteResponse(msg *raftpd.Message) {
	if msg.Reject {
		log.Infof("%x received %v rejection from %x at term %d",
			c.id, msg.MsgType, msg.From, c.term)
	} else {
		log.Infof("%x received %v from %x at term %d",
			c.id, msg.MsgType, msg.From, msg.Term)
	}

	node := c.getNodeByID(msg.From)
	node.UpdateVoteState(msg.Reject)

	/* self has one */
	count := c.voteStateCount(peer.VoteGranted) + 1
	if count >= c.quorum() {
		if msg.MsgType == raftpd.MsgVoteResponse {
			log.Infof("%d [term: %d] win campaign", c.id, c.term)
			c.becomeLeader()
			c.broadcastVictory()
		} else {
			c.campaign()
		}
		return
	}

	// return to follower state if it receives vote denial from a majority
	count = c.voteStateCount(peer.VoteReject)
	if count >= c.quorum() {
		c.backToFollower(msg.Term, conf.InvalidID)
	}
}

func (c *core) broadcastHeartbeatWithCtx(context []byte) {
	log.Debugf("%d [Term: %d] begin broadcast heartbeat with context: %v", c.id, c.term, context)
	for i := 0; i < len(c.nodes); i++ {
		c.sendHeartbeat(c.nodes[i], context)
	}
}

func (c *core) sendHeartbeat(node *peer.Node, context []byte) {
	// Attach the commit as min(to.matched, raftlog.committed).
	// When the leader sends out heartbeat message,
	// the receiver(follower) might not be matched with the leader
	// or it might not have all the committed entries.
	// The leader MUST NOT forward the follower's commit to
	// an unmatched index, in order to preserving Log Matching Property.

	msg := raftpd.Message{
		To:      node.ID,
		MsgType: raftpd.MsgHeartbeatRequest,
		Index:   utils.MinUint64(node.Matched, c.log.CommitIndex()),
		Context: context,
	}

	c.send(&msg)
}

// broadcastAppend send append or snapshot to followers.
func (c *core) broadcastAppend() {
	firstIndex := c.log.FirstIndex()
	lastIndex := c.log.LastIndex()

	log.Debugf("%d broadcast message at term: %d [%d, %d]",
		c.id, c.term, firstIndex, lastIndex)
	if c.quorum() <= 1 {
		// there only one node in current cluster, so no responses will return,
		// just commit all entries directly.
		c.poll(lastIndex)
	} else {
		for i := 0; i < len(c.nodes); i++ {
			node := c.nodes[i]
			/* ignore paused node */
			if node.IsPaused() {
				continue
			}

			if node.NextIdx >= firstIndex {
				c.sendAppend(node)
			} else {
				// send snapshot if we failed to get term or entries
				c.sendSnapshot(node)
			}
		}
	}
}

func (c *core) sendAppend(node *peer.Node) {
	logIndex := node.NextIdx - 1
	msg := raftpd.Message{
		To:       node.ID,
		Index:    c.log.CommitIndex(),
		MsgType:  raftpd.MsgAppendRequest,
		LogIndex: logIndex,
		LogTerm:  c.log.Term(logIndex),
	}

	if c.log.LastIndex() >= node.NextIdx {
		entries := c.log.Slice(node.NextIdx, c.log.LastIndex()+1)
		// slice message with max size
		var size uint
		// Starting from 1, to prevent the issue of unable to send data.
		for i := 1; i < len(entries); i++ {
			// FIXME: Find a more accurate, easy to understand,
			// more aggregated way to calculate size.
			size += uint(16 + len(entries[i].Data))
			if size > c.maxSizePerMsg {
				entries = entries[:i]
				break
			}
		}
		msg.Entries = make([]raftpd.Entry, len(entries))
		copy(msg.Entries, entries)
		utils.Assert(len(entries) == 0 || msg.Entries[0].Index != conf.InvalidIndex, "")
	} else {
		msg.Entries = make([]raftpd.Entry, 0)
	}

	// Debug: validate consistency
	if len(msg.Entries) > 0 {
		for i := 0; i < len(msg.Entries)-1; i++ {
			utils.Assert(msg.Entries[i].Index+1 == msg.Entries[i+1].Index,
				"%d index:%d at:%d not sequences", c.id, msg.Entries[i].Index, i)
		}
	}

	log.Debugf("%d [Term: %d] send append [idx: %d, Term: %d, len: %d] "+
		"to node: %d [matched: %d next: %d]",
		c.id, c.term, msg.LogIndex, msg.LogTerm, len(msg.Entries),
		node.ID, node.Matched, node.NextIdx)

	utils.Assert(!node.IsPaused(), "try call paused node")

	node.SendEntries(msg.Entries)
	c.send(&msg)
}

func (c *core) sendSnapshot(node *peer.Node) {
	snapshot := c.callback.readSnapshot()

	// if snapshot is building at now, it will return nil,
	// so just ignore it and send message to it on next tick.
	if snapshot == nil {
		log.Debugf("%x failed to send snapshot to %x because snapshot "+
			"is temporarily unavailable", c.id, node.ID)
		return
	}

	log.Debugf("%x [firstIdx: %d, commit: %d] send "+
		"snapshot[index: %d, term: %d] to %x", c.id, c.log.FirstIndex(), c.log.CommitIndex(),
		snapshot.Metadata.Index, snapshot.Metadata.Term, node.ID)

	node.SendSnapshot(snapshot.Metadata.Index)

	log.Debugf("%x paused sending replication messages to %x", c.id, node.ID)

	msg := raftpd.Message{
		MsgType:  raftpd.MsgSnapshotRequest,
		To:       node.ID,
		Snapshot: snapshot,
	}

	c.send(&msg)
}
