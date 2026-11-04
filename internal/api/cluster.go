package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/node"
)

// clusterNode is one row of the panel's Cluster → Servers page: this node
// or a peer discovered over NATS. Servers added by hand (the servers
// table, admin only) are merged in by the panel, matched on URL.
type clusterNode struct {
	node.PeerStatus
	Self bool `json:"self"`
}

type clusterResponse struct {
	Enabled            bool          `json:"enabled"`   // [multinode] enabled
	Connected          bool          `json:"connected"` // to NATS, right now
	Ready              bool          `json:"ready"`     // connected long enough to trust the view
	NATSUrl            string        `json:"nats_url,omitempty"`
	Policy             string        `json:"ownership_policy,omitempty"`
	HeartbeatIntervalS int           `json:"heartbeat_interval_s,omitempty"`
	PeerForgetS        int           `json:"peer_forget_s,omitempty"`
	Nodes              []clusterNode `json:"nodes"`
}

// handleCluster serves GET /api/v1/cluster: multinode status and every
// node this one knows of, including unreachable peers that aren't
// forgotten yet (reachable=false). Read-only, like /nodes. The NATS URL is
// shown without credentials.
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	resp := clusterResponse{Enabled: s.nodeMgr != nil}

	self := clusterNode{Self: true}
	self.NodeName = s.cfg.Server.NodeName
	self.URL = s.cfg.Multinode.AdvertiseURL
	self.Reachable = true
	self.LiveStreams = len(s.registry.All())
	self.LastSeen = time.Now()

	if s.nodeMgr != nil {
		resp.Connected = s.nodeMgr.Connected()
		resp.Ready = s.nodeMgr.Ready()
		resp.NATSUrl = redactURL(s.cfg.Multinode.NATSUrl)
		resp.Policy = s.cfg.Multinode.OwnershipPolicy
		resp.HeartbeatIntervalS = s.cfg.Multinode.HeartbeatIntervalS
		resp.PeerForgetS = s.cfg.Multinode.PeerForgetS
		self.NodeID = s.nodeMgr.NodeID()
		self.Priority = s.cfg.Multinode.NodePriority
		if s.dvrOwner != nil {
			for _, st := range s.dvrOwner.Report() {
				self.DVRStreams++
				if st.Publishing {
					self.Publishing++
				}
			}
		}
	}
	resp.Nodes = append(resp.Nodes, self)
	if s.nodeMgr != nil {
		for _, p := range s.nodeMgr.PeerStatuses() {
			resp.Nodes = append(resp.Nodes, clusterNode{PeerStatus: p})
		}
	}
	data, _ := json.Marshal(resp)
	jsonOK(w, data)
}
