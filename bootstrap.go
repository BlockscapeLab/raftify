package raftify

import (
	"time"
)

// toBootstrap initiates the transition into the bootstrap mode. In this mode, nodes wait for
// the expected number of nodes specified in the expect field of the raftify.json to go online
// and start all nodes of the cluster at the same time.
func (n *Node) toBootstrap() {
	n.logger.Printf("[DEBUG] raftify: %v/%v nodes for bootstrap...\n", len(n.memberlist.Members()), n.config.Expect)
	n.state = Bootstrap

	if n.config.Expect == 1 {
		n.logger.Println("[DEBUG] raftify: Successfully bootstrapped cluster ✓")
		n.saveState()

		// If the node has no peers and thus does not try to join any, it can safely become the
		// cluster leader for its single-node cluster. However, if there are peers in the peerlist
		// and each node is expected to start on its own, the nodes must not become leaders right
		// away since that would cause double-signing. Instead they become followers which gives
		// enough leeway for heartbeat messages to be sent and received such that no two leaders
		// exist simultaneously.
		if len(n.config.PeerList) == 0 {
			n.toLeader()
		} else {
			n.logger.Printf("[INFO] raftify: Expecting 1 node, but found %v peers. Going through full leader election cycle...", len(n.config.PeerList))
			n.toFollower(0)

			// Try joining one of the peers only once. If none can be reached, it just continues
			// operation as a follower anc gradually works its way up to becoming the leader.
			if err := n.tryJoin(); err != nil {
				n.logger.Printf("[ERR] raftify: failed to join cluster: %v\nTrying again...\n", err.Error())
			}
		}

		n.printMemberlist()
		return
	}

	if err := n.tryJoin(); err != nil {
		n.logger.Printf("[ERR] raftify: failed to join cluster: %v\nTrying again...\n", err.Error())
	}
}

// runBootstrap waits for the number of nodes specified in the expect field of the raftify.json
// to join the cluster. This function is called within the runLoop function.
func (n *Node) runBootstrap() {
	select {
	case <-n.events.eventCh:
		n.logger.Printf("[DEBUG] raftify: %v/%v nodes for bootstrap...\n", len(n.memberlist.Members()), n.config.Expect)
		n.printMemberlist()
		n.saveState()

		if len(n.memberlist.Members()) >= n.config.Expect {
			n.logger.Println("[DEBUG] raftify: Successfully bootstrapped cluster ✓")
			n.toFollower(0)

			// Signal successful bootstrap and allow InitNode to return.
			n.bootstrapCh <- true
		}

	case <-time.After(5 * time.Second):
		if err := n.tryJoin(); err != nil {
			n.logger.Printf("[ERR] raftify: failed to join cluster: %v\nTrying again...\n", err.Error())
		}

	case <-n.shutdownCh:
		n.toPreShutdown()
	}
}
