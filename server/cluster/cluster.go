package cluster

import (
	"io/ioutil"

	"github.com/smancke/guble/protocol"

	log "github.com/Sirupsen/logrus"
	"github.com/hashicorp/memberlist"

	"errors"
	"fmt"
	"net"
	"strconv"
)

// Config is a struct used by the local node when creating and running the guble cluster
type Config struct {
	ID                   int
	Host                 string
	Port                 int
	Remotes              []*net.TCPAddr
	HealthScoreThreshold int
}

type MessageHandler interface {
	HandleMessage(message *protocol.Message) error
}

// Cluster is a struct for managing the `local view` of the guble cluster, as seen by a node.
type Cluster struct {
	// Pointer to a Config struct, based on which the Cluster node is created and runs.
	Config *Config

	// MessageHandler is used for dispatching messages received by this node.
	// Should be set after the node is created with New(), and before Start().
	MessageHandler MessageHandler

	name       string
	memberlist *memberlist.Memberlist
	broadcasts [][]byte

	numJoins   int
	numLeaves  int
	numUpdates int
}

//New returns a new instance of the cluster, created using the given Config.
func New(config *Config) (*Cluster, error) {
	c := &Cluster{Config: config, name: fmt.Sprintf("%d", config.ID)}

	memberlistConfig := memberlist.DefaultLANConfig()
	memberlistConfig.Name = c.name
	memberlistConfig.BindAddr = config.Host
	memberlistConfig.BindPort = config.Port

	//TODO Cosmin temporarily disabling any logging from memberlist, we might want to enable it again using logrus?
	memberlistConfig.LogOutput = ioutil.Discard

	memberlist, err := memberlist.Create(memberlistConfig)
	if err != nil {
		logger.WithField("error", err).Error("Error when creating the internal memberlist of the cluster")
		return nil, err
	}
	c.memberlist = memberlist
	memberlistConfig.Delegate = c
	memberlistConfig.Conflict = c
	memberlistConfig.Events = c
	return c, nil
}

// Start the cluster module.
func (cluster *Cluster) Start() error {
	logger.WithField("remotes", cluster.Config.Remotes).Debug("Starting Cluster")
	if cluster.MessageHandler == nil {
		errorMessage := "There should be a valid MessageHandler already set-up"
		logger.Error(errorMessage)
		return errors.New(errorMessage)
	}
	num, err := cluster.memberlist.Join(cluster.remotesAsStrings())
	if err != nil {
		logger.WithField("error", err).Error("Error when this node wanted to join the cluster")
		return err
	}
	if num == 0 {
		errorMessage := "No remote hosts were successfuly contacted when this node wanted to join the cluster"
		logger.Error(errorMessage)
		return errors.New(errorMessage)
	}
	logger.Debug("Started Cluster")
	return nil
}

// Stop the cluster module.
func (cluster *Cluster) Stop() error {
	return cluster.memberlist.Shutdown()
}

// Check returns a non-nil error if the health status of the cluster (as seen by this node) is not perfect.
func (cluster *Cluster) Check() error {
	if healthScore := cluster.memberlist.GetHealthScore(); healthScore > cluster.Config.HealthScoreThreshold {
		errorMessage := "Cluster Health Score is not perfect"
		logger.WithField("healthScore", healthScore).Error(errorMessage)
		return errors.New(errorMessage)
	}
	return nil
}

// BroadcastString broadcasts a string to all the other nodes in the guble cluster
func (cluster *Cluster) BroadcastString(sMessage *string) error {
	logger.WithField("string", sMessage).Debug("BroadcastString")
	cMessage := &message{
		NodeID: cluster.Config.ID,
		Type:   stringMessage,
		Body:   []byte(*sMessage),
	}
	return cluster.broadcastClusterMessage(cMessage)
}

// BroadcastMessage broadcasts a guble-protocol-message to all the other nodes in the guble cluster
func (cluster *Cluster) BroadcastMessage(pMessage *protocol.Message) error {
	logger.WithField("message", pMessage).Debug("BroadcastMessage")
	cMessage := &message{
		NodeID: cluster.Config.ID,
		Type:   gubleMessage,
		Body:   pMessage.Bytes(),
	}
	return cluster.broadcastClusterMessage(cMessage)
}

// ===================================
// memberslist.Delegate implementation
// ===================================

// NotifyMsg is invoked each time a message is received by this node of the cluster; it decodes and dispatches the messages.
func (cluster *Cluster) NotifyMsg(msg []byte) {
	logger.WithField("msgAsBytes", msg).Debug("NotifyMsg")

	cmsg, err := decode(msg)
	if err != nil {
		logger.WithField("err", err).Error("Decoding of cluster message failed")
		return
	}
	logger.WithFields(log.Fields{
		"senderNodeID": cmsg.NodeID,
		"type":         cmsg.Type,
		"body":         string(cmsg.Body),
	}).Debug("NotifyMsg: Received cluster message")

	if cluster.MessageHandler != nil && cmsg.Type == gubleMessage {
		message, err := protocol.ParseMessage(cmsg.Body)
		if err != nil {
			logger.WithField("err", err).Error("Parsing of guble-message contained in cluster-message failed")
			return
		}
		cluster.MessageHandler.HandleMessage(message)
	}
}

func (cluster *Cluster) GetBroadcasts(overhead, limit int) [][]byte {
	b := cluster.broadcasts
	cluster.broadcasts = nil
	return b
}

func (cluster *Cluster) NodeMeta(limit int) []byte { return nil }

func (cluster *Cluster) LocalState(join bool) []byte { return nil }

func (cluster *Cluster) MergeRemoteState(s []byte, join bool) {}

// ===================================
// memberlist.EventDelegate implementation for cluster structure
// ===================================

func (cluster *Cluster) NotifyJoin(node *memberlist.Node) {
	cluster.numJoins++
	cluster.log(node, "Cluster Node Join")
}

func (cluster *Cluster) NotifyLeave(node *memberlist.Node) {
	cluster.numLeaves++
	cluster.log(node, "Cluster Node Leave")
}

func (cluster *Cluster) NotifyUpdate(node *memberlist.Node) {
	cluster.numUpdates++
	cluster.log(node, "Cluster Node Update")
}

// ==========================================
// memberlist.ConflictDelegate implementation
// ==========================================

func (cluster *Cluster) NotifyConflict(existing, other *memberlist.Node) {
	logger.WithFields(log.Fields{
		"existing": *existing,
		"other":    *other,
	}).Panic("NotifyConflict")
}

func (cluster *Cluster) log(node *memberlist.Node, message string) {
	logger.WithFields(log.Fields{
		"node":       *node,
		"numJoins":   cluster.numJoins,
		"numLeaves":  cluster.numLeaves,
		"numUpdates": cluster.numUpdates,
	}).Debug(message)
}

func (cluster *Cluster) broadcastClusterMessage(cMessage *message) error {
	if cMessage == nil {
		errorMessage := "Could not broadcast a nil cluster-message"
		logger.Error(errorMessage)
		return errors.New(errorMessage)
	}
	cMessageBytes, err := cMessage.encode()
	if err != nil {
		logger.WithField("err", err).Error("Could not encode and broadcast cluster-message")
		return err
	}
	for _, node := range cluster.memberlist.Members() {
		if cluster.name == node.Name {
			continue
		}
		go cluster.sendToNode(node, cMessageBytes)
	}
	return nil
}

func (cluster *Cluster) sendToNode(node *memberlist.Node, msgBytes []byte) {
	logger.WithField("nodeName", node.Name).Debug("Sending cluster-message to a node")
	err := cluster.memberlist.SendToTCP(node, msgBytes)
	if err != nil {
		logger.WithFields(log.Fields{
			"err":  err,
			"node": node,
		}).Error("Error sending cluster-message to a node")
	}
}

func (cluster *Cluster) remotesAsStrings() (strings []string) {
	for _, remote := range cluster.Config.Remotes {
		strings = append(strings, remote.IP.String()+":"+strconv.Itoa(remote.Port))
	}
	return
}