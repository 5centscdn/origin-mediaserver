package dvrowner

import "github.com/5centscdn/origin-mediaserver/internal/node"

// Verdict is what a node should answer for an nDVR stream's playlists.
type Verdict int

const (
	// Bypass: not an nDVR stream, or cluster mode is off. Serve as usual.
	Bypass Verdict = iota
	// Owner: this node is the selected origin. Serve as usual.
	Owner
	// NotOwner: another node is the selected origin, or nobody has the
	// stream. Answer 404 so the CDN tries the next origin.
	NotOwner
	// Unknown: the answer depends on a peer this node can't currently
	// hear from, or this node's cluster view isn't ready. Answer 503.
	Unknown
)

func (v Verdict) String() string {
	switch v {
	case Bypass:
		return "bypass"
	case Owner:
		return "owner"
	case NotOwner:
		return "not_owner"
	case Unknown:
		return "unknown"
	}
	return "?"
}

// Decision is a Verdict plus who owns the stream (node name, when known)
// and why, for logs, headers and the API.
type Decision struct {
	Verdict Verdict
	Owner   string
	Reason  string
}

// Policies for choosing between nodes that are all publishing the stream.
const (
	PolicyFirstPublisher = "first_publisher"
	PolicyPriority       = "priority"
)

// candidate is one node's state for one stream.
type candidate struct {
	id, name   string
	priority   int
	self       bool
	publishing bool
	sinceMS    int64
	dvrEndMS   int64 // 0 = no valid recording
}

func (c candidate) has() bool { return c.publishing || c.dvrEndMS > 0 }

// beats reports whether a should own the stream rather than b:
//
//  1. a node publishing the stream beats one that isn't;
//  2. between two publishers, the ownership policy decides:
//     first_publisher = earlier publish start, priority = lower
//     node_priority;
//  3. between two nodes that aren't publishing, the later recording end
//     wins (the recording's own timeline, never file times);
//  4. ties: lower node_priority, then node name, then node ID, so every
//     node reaches the same answer from the same state.
func beats(a, b candidate, policy string) bool {
	if a.publishing != b.publishing {
		return a.publishing
	}
	if a.publishing {
		if policy == PolicyPriority {
			if a.priority != b.priority {
				return a.priority < b.priority
			}
			if a.sinceMS != b.sinceMS {
				return a.sinceMS < b.sinceMS
			}
		} else {
			if a.sinceMS != b.sinceMS {
				return a.sinceMS < b.sinceMS
			}
		}
	} else if a.dvrEndMS != b.dvrEndMS {
		return a.dvrEndMS > b.dvrEndMS
	}
	if a.priority != b.priority {
		return a.priority < b.priority
	}
	if a.name != b.name {
		return a.name < b.name
	}
	return a.id < b.id
}

// decide picks this node's verdict for one stream from its own state and
// its peers' last reported states. ready is false while this node's
// cluster view can't be trusted (not connected to NATS, or just
// connected).
//
// Peers that stopped heartbeating but aren't forgotten yet (PeerDVR.Fresh
// false) are never treated as offline, which is what makes the answer
// Unknown rather than a guess:
//
//   - Fresh peers alone already beat this node → NotOwner. More candidates
//     can only add competition, so the silent peers can't change that.
//   - This node has nothing (not publishing, no recording) → NotOwner:
//     whoever owns the stream, it can't be served from here.
//   - This node is publishing → Owner, unless a silent peer was last seen
//     publishing too and would win the conflict.
//   - This node only has a recording → Owner only if every peer is
//     reachable and the view is ready: a silent peer may have picked up
//     the encoder or recorded more since.
func decide(self candidate, peers []node.PeerDVR, policy string, ready bool) Decision {
	best := self
	var silent []candidate
	for _, p := range peers {
		c := candidate{id: p.NodeID, name: p.NodeName, priority: p.Priority}
		if p.Known {
			c.publishing = p.State.Publishing
			c.sinceMS = p.State.PublishSinceMS
			if p.State.DVREndMS > p.State.DVRStartMS && p.State.DVRStartMS > 0 {
				c.dvrEndMS = p.State.DVREndMS
			}
		}
		if !p.Fresh {
			silent = append(silent, c)
			continue
		}
		if c.has() && beats(c, best, policy) {
			best = c
		}
	}

	if !best.self {
		return Decision{Verdict: NotOwner, Owner: best.name, Reason: reasonFor(best)}
	}
	if !self.has() {
		return Decision{Verdict: NotOwner, Reason: "no live publisher or recording on this node"}
	}
	if self.publishing {
		for _, c := range silent {
			if c.publishing && beats(c, self, policy) {
				return Decision{Verdict: Unknown, Owner: c.name,
					Reason: "peer " + c.name + " was publishing with precedence and is unreachable"}
			}
		}
		return Decision{Verdict: Owner, Owner: self.name, Reason: "publishing here"}
	}
	if !ready {
		return Decision{Verdict: Unknown, Reason: "cluster view not ready (NATS not connected or still syncing)"}
	}
	if len(silent) > 0 {
		return Decision{Verdict: Unknown, Owner: silent[0].name,
			Reason: "peer " + silent[0].name + " is unreachable; it may be live or have a newer recording"}
	}
	return Decision{Verdict: Owner, Owner: self.name, Reason: "latest recording, no publisher in the cluster"}
}

func reasonFor(c candidate) string {
	if c.publishing {
		return "publishing on " + c.name
	}
	return "latest recording is on " + c.name
}

// Combine merges the decisions for an ABR group's variants into one for
// the group's master playlist, so the master and its variants resolve to
// the same node:
//
//   - variants without nDVR (Bypass) and variants nobody has (NotOwner
//     with no owner) are ignored; if nothing is left it's the first
//     non-Bypass decision, or Bypass;
//   - any Unknown → Unknown;
//   - otherwise every remaining variant normally has the same owner (one
//     encoder publishes all renditions), and the first remaining variant
//     decides when they don't.
func Combine(ds []Decision) Decision {
	var first *Decision
	var rest []Decision
	for i := range ds {
		d := ds[i]
		if d.Verdict == Bypass {
			continue
		}
		if first == nil {
			first = &ds[i]
		}
		if d.Verdict == NotOwner && d.Owner == "" {
			continue
		}
		rest = append(rest, d)
	}
	if len(rest) == 0 {
		if first == nil {
			return Decision{Verdict: Bypass}
		}
		return *first
	}
	for _, d := range rest {
		if d.Verdict == Unknown {
			return d
		}
	}
	return rest[0]
}
