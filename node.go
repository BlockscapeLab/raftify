package raftify

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/hashicorp/memberlist"
)

// Node contains core attributes that every node has regardless of node state.
type Node struct {
	// The node's version info.
	versionInfo VersionInfo

	// The state the node is currently in; can be Follower, PreCandidate, Candidate
	// or Leader.
	state State

	// The term the node is currently at. Increases monotonically.
	currentTerm uint64

	// The number of nodes making up the majority of nodes in the cluster needed to agree
	// on a decision to make it binding, e.g. the election of a leader.
	quorum int

	// The logger used to log messages for raftify.
	logger *log.Logger

	// The directory in which the raftify.json is contained and to which the state.json
	// is written.
	workingDir string

	// The node's configuration. See the Config struct for more information.
	config *Config

	// The secret encryption key used to encrypt messages exchanges between nodes.
	secretKey []byte

	// The local list of cluster members which is used to coordinate cluster membership
	// and failure detection.
	memberlist *memberlist.Memberlist

	// Delegate for messages.
	messages *MessageDelegate

	// Delegate for join and leave updates.
	events *ChannelEventDelegate

	// Channel used to signal successful bootstrap.
	bootstrapCh chan bool

	// Channel used for shutdown.
	shutdownCh chan error

	// The node a follower has voted for during a candidacy. A node can only vote for one
	// candidate during a term.
	votedFor string

	// The list of prevotes, keeping track of prevotes received and pending ones.
	preVoteList *VoteList

	// The timer used for the heartbeat and election timeout of followers, precandidates and
	// candidates.
	timeoutTimer *time.Timer

	// The list of votes, keeping track of votes received and pending ones.
	voteList *VoteList

	// The ticker used for periodically sending out vote requests and heartbeats.
	messageTicker *time.Ticker

	// List of heartbeat IDs, keeping track of which heartbeats were sent out and which ones
	// have gotten a response in the respective cycle.
	heartbeatIDList *HeartbeatIDList
}

// createMemberlist creates and returns a new local and already configured memberlist.
func (n *Node) createMemberlist() error {
	config := memberlist.DefaultWANConfig()
	config.Name = n.config.ID
	config.BindAddr = n.config.BindAddr
	config.BindPort = n.config.BindPort
	config.AdvertisePort = n.config.BindPort
	config.TCPTimeout = 3 * time.Second
	config.Logger = n.logger
	config.Delegate = n.messages
	config.Events = n.events

	if secretKey, err := hexToByte(n.config.Encrypt); err == nil {
		config.SecretKey = secretKey
	}

	// On memberlist creation, a join event is immediately fired for the local node.
	// At this point in time, the main loop is not running though, so the creation
	// would block the entire application. This anonymous go routine prevents this
	// from happening by waiting for the first join event to be fired and simply
	// skipping it so the main loop can be started afterwards. All further join and
	// leave events are caught in the main loop after this first occurrence.
	go func() {
		<-n.events.eventCh
	}()

	var err error
	if n.memberlist, err = memberlist.Create(config); err != nil {
		return err
	}
	return nil
}

// tryJoin attempts to join an existing cluster via one of its peers listed in the peerlist.
// If no peers can be reached the node is started and waits to be bootstrapped.
func (n *Node) tryJoin() error {
	n.logger.Println("[DEBUG] raftify: Trying to join existing cluster via peers...")
	numPeers, err := n.memberlist.Join(n.config.PeerList)
	if err != nil {
		return err
	}

	n.logger.Printf("[DEBUG] raftify: %v peers are currently available ✓\n", numPeers)
	return nil
}

// printMemberlist prints out the local memberlist into the console log.
func (n *Node) printMemberlist() {
	n.logger.Printf("[INFO] raftify: The cluster has currently %v members:\n", len(n.memberlist.Members()))
	for _, member := range n.memberlist.Members() {
		n.logger.Printf("[INFO] raftify: - %v [%v]\n", member.Name, member.Addr)
	}
}

// printVersionInfo prints out the Raftify and go version the node is running on.
func (n *Node) printVersionInfo() {
	vi := n.versionInfo.GetVersionInfo()
	n.logger.Printf("[INFO] raftify: Running %v %v (%v)...", vi.Name, vi.Version, vi.GoVersion)
}

// initNode initializes a new raftified node.
func initNode(logger *log.Logger, workingDir string) (*Node, error) {
	node := &Node{
		logger:        logger,
		workingDir:    workingDir,
		timeoutTimer:  time.NewTimer(time.Second),
		messageTicker: time.NewTicker(time.Second),
		bootstrapCh:   make(chan bool),  // This must NEVER be a buffered channel.
		shutdownCh:    make(chan error), // This must NEVER be a buffered channel.
	}

	node.timeoutTimer.Stop()
	node.messageTicker.Stop()

	node.messages = &MessageDelegate{
		logger:    logger,
		messageCh: make(chan []byte),
	}
	node.events = &ChannelEventDelegate{
		logger: logger,
	}
	node.heartbeatIDList = &HeartbeatIDList{
		logger:             logger,
		currentHeartbeatID: 0,
		received:           0,
		pending:            []uint64{},
		subQuorumCycles:    0,
	}
	node.preVoteList = &VoteList{
		logger:              logger,
		received:            0,
		pending:             []*memberlist.Node{},
		missedPrevoteCycles: 0,
	}
	node.voteList = &VoteList{
		logger:              logger,
		received:            0,
		pending:             []*memberlist.Node{},
		missedPrevoteCycles: 0,
	}

	// Print version info.
	node.printVersionInfo()

	// If there is a state.json, it means that the node has not explicitly left the cluster
	// and therefore must have been partitioned out or crashed/timed out. At this point, it
	// is no longer guaranteed its memberlist is up-to-date and it therefore needs to initiate
	// a rejoin to see if there were any changes to the cluster during its absence.
	_, fileErr := os.Stat(workingDir + "/state.json")
	if fileErr == nil { // Found state.json
		node.logger.Println("[DEBUG] raftify: Loading peers from state.json...")

		// If state.json was found, the true passed into the loadConfig method indicated that
		// the memberlist from the state.json is loaded into the config in place of the
		// peerlist from the raftify.json file.
		if err := node.loadConfig(true); err != nil {
			return nil, fmt.Errorf("[ERR] raftify: %v", err.Error())
		}
	} else { // Didn't find state.json
		node.logger.Println("[DEBUG] raftify: Loading peers from raftify.json...")

		// Make sure the file error is not related to the state.json not existing
		if !os.IsNotExist(fileErr) {
			return nil, fmt.Errorf("[DEBUG] raftify: %v", fileErr.Error())
		}

		// Load the config normally
		if err := node.loadConfig(false); err != nil {
			return nil, fmt.Errorf("[ERR] raftify: %v", err.Error())
		}
	}

	// Allocate enough memory for the event channel to accommodate for the self-imposed number
	// of maximum nodes to be run in the cluster.
	node.events.eventCh = make(chan memberlist.NodeEvent, node.config.MaxNodes)

	// Create the local memberlist that initially only contains the local node. It is used to
	// keep track of cluster membership.
	if listErr := node.createMemberlist(); listErr != nil {
		return nil, fmt.Errorf("[ERR] raftify: %v", listErr.Error())
	}

	// The first quorum is determined by the number of expected nodes specified in the raftify.json.
	node.quorum = int(node.config.Expect/2) + 1

	node.logger.Printf("[DEBUG] raftify: %v successfully initialized ✓\n", node.config.ID)

	// Initialize the bootstrap phase
	node.toBootstrap()

	// Run the main loop
	go node.runLoop()

	// Block until cluster has been successfully bootstrapped. toBootstrap is able to unblock.
	// Don't block if expect is set to 1 since that will be bootstrapped immediately.
	if node.config.Expect != 1 {
		<-node.bootstrapCh
	}
	return node, nil
}

// getNodeByName returns the full Node struct from memberlist to the specified name.
func (n *Node) getNodeByName(name string) (*memberlist.Node, error) {
	for _, member := range n.memberlist.Members() {
		if name == member.Name {
			return member, nil
		}
	}
	return nil, fmt.Errorf("couldn't find %v in the local memberlist", name)
}

// resetTimeout resets the internal timeout timer to a random duration measured in milliseconds.
func (n *Node) resetTimeout() {
	n.timeoutTimer.Reset(time.Duration(rand.Intn(MaxTimeout*n.config.Performance-MinTimeout*n.config.Performance)+MinTimeout*n.config.Performance) * time.Millisecond)
}

// startMessageTicker starts the message ticker.
func (n *Node) startMessageTicker() {
	n.messageTicker = time.NewTicker(time.Duration((TickerInterval * n.config.Performance)) * time.Millisecond)
}

// quorumReached checks whether the specified number of votes make up the majority in order
// to reach quorum. Once the quorum is reached, a new quorum is set based on the current size
// of the memberlist. This allows the quorum to change dynamically with the cluster size.
// However, if 50% or more nodes fail at the same time the quorum cannot be reached anymore.
func (n *Node) quorumReached(votes int) bool {
	if votes < n.quorum {
		var msg string
		switch n.state {
		case PreCandidate:
			msg = "prevotes"
		case Candidate:
			msg = "votes"
		case Leader:
			msg = "heartbeat responses"
		}

		n.logger.Printf("[DEBUG] raftify: Couldn't reach %v quorum: not enough %v (%v/%v)\n", n.state.toString(), msg, votes, n.quorum)
		return false
	}

	// Once a quorum is reached, a new quorum is set according to the cluster size at that
	// particular point in time. This makes sure that when the memberlists are truncated during
	// a network partition, the quorum of the previous cluster size needs to be reached and thus
	// no two leaders can exist simultaneously in both partitions. The larger partition will have
	// a leader, the smaller one won't.
	n.logger.Printf("[DEBUG] raftify: %v quorum reached: (%v/%v)\n", n.state.toString(), votes, n.quorum)
	n.quorum = int(len(n.memberlist.Members())/2) + 1
	return true
}

// runLoop runs the routine for the node's current state.
func (n *Node) runLoop() {
	for {
		switch n.state {
		case Bootstrap:
			n.runBootstrap()
		case Rejoin:
			n.runRejoin()
		case Follower:
			n.runFollower()
		case PreCandidate:
			n.runPreCandidate()
		case Candidate:
			n.runCandidate()
		case Leader:
			n.runLeader()
		case PreShutdown:
			n.runPreShutdown()
		case Shutdown:
			n.runShutdown()
			return // exit loop and kill goroutine after shutdown
		default:
			panic(fmt.Sprintf("invalid node state: %v", n.state))
		}
	}
}

// MessageDelegate is the interface that clients must implement if they want to hook into the gossip
// layer of Memberlist.
type MessageDelegate struct {
	logger    *log.Logger
	messageCh chan []byte
}

// NotifyMsg implements the Delegate interface.
func (d *MessageDelegate) NotifyMsg(msg []byte) {
	d.messageCh <- msg
}

// NodeMeta implements the Delegate interface.
func (d *MessageDelegate) NodeMeta(limit int) []byte {
	return []byte("") // Not used.
}

// LocalState implements the Delegate interface.
func (d *MessageDelegate) LocalState(join bool) []byte {
	return []byte("") // Not used.
}

// GetBroadcasts implements the Delegate interface.
func (d *MessageDelegate) GetBroadcasts(overhead, limit int) [][]byte {
	return nil // Not used.
}

// MergeRemoteState implements the Delegate interface.
func (d *MessageDelegate) MergeRemoteState(buf []byte, join bool) {} // Not used.

// ChannelEventDelegate is a simpler delegate that is used only to receive notifications about members
// joining and leaving.
type ChannelEventDelegate struct {
	logger  *log.Logger
	eventCh chan memberlist.NodeEvent
}

// NotifyJoin implements the EventDelegate interface.
func (d *ChannelEventDelegate) NotifyJoin(newNode *memberlist.Node) {
	d.logger.Printf("[INFO] raftify: ->[] %s [%s] joined the cluster.\n", newNode.Name, newNode.Address())
	d.eventCh <- memberlist.NodeEvent{
		Event: memberlist.NodeJoin,
		Node:  newNode,
	}
}

// NotifyLeave implements the EventDelegate interface.
func (d *ChannelEventDelegate) NotifyLeave(oldNode *memberlist.Node) {
	d.logger.Printf("[INFO] raftify: []-> %s [%s] left the cluster.\n", oldNode.Name, oldNode.Address())
	d.eventCh <- memberlist.NodeEvent{
		Event: memberlist.NodeLeave,
		Node:  oldNode,
	}
}

// NotifyUpdate implements the EventDelegate interface.
func (d *ChannelEventDelegate) NotifyUpdate(updatedNode *memberlist.Node) {} // Not used.
