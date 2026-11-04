package dvrowner

import (
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/node"
)

// Timeline values used below (UTC ms). Only their order matters.
const (
	t1030 = int64(1_793_000_000_000)
	t1045 = t1030 + 15*60_000
	t1000 = t1030 - 30*60_000
)

func selfA(publishing bool, since, dvrEnd int64) candidate {
	return candidate{id: "id-a", name: "origin-a", priority: 100, self: true,
		publishing: publishing, sinceMS: since, dvrEndMS: dvrEnd}
}

func selfB(publishing bool, since, dvrEnd int64) candidate {
	return candidate{id: "id-b", name: "origin-b", priority: 100, self: true,
		publishing: publishing, sinceMS: since, dvrEndMS: dvrEnd}
}

// peer builds origin-x's report of the stream. dvrEnd 0 = no recording.
func peer(name string, fresh, known, publishing bool, since, dvrEnd int64) node.PeerDVR {
	p := node.PeerDVR{NodeID: "id-" + name[len(name)-1:], NodeName: name, Priority: 100, Fresh: fresh, Known: known}
	p.State = node.DVRState{App: "live", Stream: "cam", Publishing: publishing, PublishSinceMS: since}
	if dvrEnd > 0 {
		p.State.DVRStartMS, p.State.DVREndMS = dvrEnd-3600_000, dvrEnd
	}
	return p
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name   string
		self   candidate
		peers  []node.PeerDVR
		policy string
		ready  bool
		want   Verdict
		owner  string
	}{
		// 2/3: encoder on A, B has history → A serves, B 404s.
		{"live on A, seen from A", selfA(true, t1030, t1030), []node.PeerDVR{peer("origin-b", true, true, false, 0, t1000)}, "", true, Owner, "origin-a"},
		{"live on A, seen from B", selfB(false, 0, t1000), []node.PeerDVR{peer("origin-a", true, true, true, t1030, t1030)}, "", true, NotOwner, "origin-a"},
		{"live on B, seen from B", selfB(true, t1030, t1030), []node.PeerDVR{peer("origin-a", true, true, false, 0, t1000)}, "", true, Owner, "origin-b"},
		{"live on B, seen from A", selfA(false, 0, t1000), []node.PeerDVR{peer("origin-b", true, true, true, t1030, t1030)}, "", true, NotOwner, "origin-b"},

		// 4/5: a stale archive newer than the live node's recording still
		// loses to the live publisher.
		{"A stale DVR newer than live B's", selfA(false, 0, t1045), []node.PeerDVR{peer("origin-b", true, true, true, t1030, t1030)}, "", true, NotOwner, "origin-b"},
		{"B stale DVR newer than live A's", selfA(true, t1030, t1030), []node.PeerDVR{peer("origin-b", true, true, false, 0, t1045)}, "", true, Owner, "origin-a"},

		// 6/7: encoder gone everywhere → latest recording end wins.
		{"offline, A latest, seen from A", selfA(false, 0, t1045), []node.PeerDVR{peer("origin-b", true, true, false, 0, t1030)}, "", true, Owner, "origin-a"},
		{"offline, A latest, seen from B", selfB(false, 0, t1030), []node.PeerDVR{peer("origin-a", true, true, false, 0, t1045)}, "", true, NotOwner, "origin-a"},
		{"offline, B latest, seen from A", selfA(false, 0, t1030), []node.PeerDVR{peer("origin-b", true, true, false, 0, t1045)}, "", true, NotOwner, "origin-b"},
		{"offline, B latest, seen from B", selfB(false, 0, t1045), []node.PeerDVR{peer("origin-a", true, true, false, 0, t1030)}, "", true, Owner, "origin-b"},

		// 8: equal ends → priority, then name: origin-a < origin-b.
		{"equal ends, A by name", selfA(false, 0, t1030), []node.PeerDVR{peer("origin-b", true, true, false, 0, t1030)}, "", true, Owner, "origin-a"},
		{"equal ends, B sees A win", selfB(false, 0, t1030), []node.PeerDVR{peer("origin-a", true, true, false, 0, t1030)}, "", true, NotOwner, "origin-a"},

		// 9/10: empty archive / nothing recorded → 404 everywhere.
		{"nothing anywhere", selfA(false, 0, 0), []node.PeerDVR{peer("origin-b", true, false, false, 0, 0)}, "", true, NotOwner, ""},
		{"peer lists stream without recording", selfA(false, 0, 0), []node.PeerDVR{peer("origin-b", true, true, false, 0, 0)}, "", true, NotOwner, ""},

		// 12/13: unreachable peer is never assumed offline.
		{"offline, A latest, B unreachable", selfA(false, 0, t1045), []node.PeerDVR{peer("origin-b", false, true, false, 0, t1030)}, "", true, Unknown, "origin-b"},
		{"offline, B unreachable and never listed it", selfA(false, 0, t1045), []node.PeerDVR{peer("origin-b", false, false, false, 0, 0)}, "", true, Unknown, "origin-b"},
		{"live on A, B unreachable not publishing", selfA(true, t1030, t1030), []node.PeerDVR{peer("origin-b", false, true, false, 0, t1000)}, "", true, Owner, "origin-a"},
		{"live on A, B unreachable but was first publisher", selfA(true, t1030, t1030), []node.PeerDVR{peer("origin-b", false, true, true, t1000, t1030)}, "", true, Unknown, "origin-b"},
		{"fresh C live beats us whatever unreachable B says", selfA(false, 0, t1045), []node.PeerDVR{peer("origin-b", false, true, false, 0, t1045), peer("origin-c", true, true, true, t1030, t1030)}, "", true, NotOwner, "origin-c"},
		{"view not ready, recording only", selfA(false, 0, t1045), nil, "", false, Unknown, ""},
		{"view not ready, publishing", selfA(true, t1030, t1030), nil, "", false, Owner, "origin-a"},

		// 14: both nodes receive the stream.
		{"both live, first_publisher, A first", selfA(true, t1000, t1030), []node.PeerDVR{peer("origin-b", true, true, true, t1030, t1030)}, PolicyFirstPublisher, true, Owner, "origin-a"},
		{"both live, first_publisher, B first", selfA(true, t1030, t1030), []node.PeerDVR{peer("origin-b", true, true, true, t1000, t1030)}, PolicyFirstPublisher, true, NotOwner, "origin-b"},
		{"both live, same start, name tie-break", selfB(true, t1030, t1030), []node.PeerDVR{peer("origin-a", true, true, true, t1030, t1030)}, PolicyFirstPublisher, true, NotOwner, "origin-a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.policy
			if policy == "" {
				policy = PolicyFirstPublisher
			}
			d := decide(tc.self, tc.peers, policy, tc.ready)
			if d.Verdict != tc.want || d.Owner != tc.owner {
				t.Errorf("decide = %v owner %q (%s), want %v owner %q", d.Verdict, d.Owner, d.Reason, tc.want, tc.owner)
			}
		})
	}
}

// TestDecidePriorityPolicy: under "priority" the lower node_priority wins
// a dual publish even if it connected later, and priority also breaks
// equal DVR end times.
func TestDecidePriorityPolicy(t *testing.T) {
	primary := peer("origin-b", true, true, true, t1030, t1030)
	primary.Priority = 1
	self := selfA(true, t1000, t1030) // connected first, priority 100

	if d := decide(self, []node.PeerDVR{primary}, PolicyPriority, true); d.Verdict != NotOwner || d.Owner != "origin-b" {
		t.Errorf("priority policy: %v %q, want origin-b", d.Verdict, d.Owner)
	}
	if d := decide(self, []node.PeerDVR{primary}, PolicyFirstPublisher, true); d.Verdict != Owner {
		t.Errorf("first_publisher policy: %v, want owner", d.Verdict)
	}

	rec := peer("origin-b", true, true, false, 0, t1030)
	rec.Priority = 1
	if d := decide(selfA(false, 0, t1030), []node.PeerDVR{rec}, PolicyFirstPublisher, true); d.Verdict != NotOwner {
		t.Errorf("equal DVR ends: priority 1 should beat 100, got %v", d.Verdict)
	}
}

// TestDecideEncoderSwitch walks case 11: the encoder moves from A to B.
// Every step, exactly one node is the owner and both agree who it is.
func TestDecideEncoderSwitch(t *testing.T) {
	type st struct {
		pub    bool
		since  int64
		dvrEnd int64
	}
	steps := []struct {
		name  string
		a, b  st
		owner string
	}{
		{"live on A", st{true, t1000, t1030}, st{false, 0, t1000 - 1}, "origin-a"},
		{"A dropped, B not yet", st{false, 0, t1030}, st{false, 0, t1000 - 1}, "origin-a"},
		{"B connected", st{false, 0, t1030}, st{true, t1030 + 5000, t1030 + 4000}, "origin-b"},
		{"B dropped too, B recorded later", st{false, 0, t1030}, st{false, 0, t1045}, "origin-b"},
	}
	for _, s := range steps {
		a := selfA(s.a.pub, s.a.since, s.a.dvrEnd)
		b := selfB(s.b.pub, s.b.since, s.b.dvrEnd)
		fromA := decide(a, []node.PeerDVR{peer("origin-b", true, true, s.b.pub, s.b.since, s.b.dvrEnd)}, PolicyFirstPublisher, true)
		fromB := decide(b, []node.PeerDVR{peer("origin-a", true, true, s.a.pub, s.a.since, s.a.dvrEnd)}, PolicyFirstPublisher, true)
		owners := 0
		for _, d := range []Decision{fromA, fromB} {
			if d.Verdict == Owner {
				owners++
			}
			if d.Owner != s.owner {
				t.Errorf("%s: owner %q, want %q", s.name, d.Owner, s.owner)
			}
		}
		if owners != 1 {
			t.Errorf("%s: %d nodes think they own it (A %v, B %v)", s.name, owners, fromA.Verdict, fromB.Verdict)
		}
	}
}

// TestCombine covers case 15: an ABR master follows its variants' owner.
func TestCombine(t *testing.T) {
	own := Decision{Verdict: Owner, Owner: "origin-a"}
	not := Decision{Verdict: NotOwner, Owner: "origin-a"}
	nobody := Decision{Verdict: NotOwner}
	unk := Decision{Verdict: Unknown, Owner: "origin-b"}
	bypass := Decision{Verdict: Bypass}

	tests := []struct {
		name string
		in   []Decision
		want Verdict
	}{
		{"no nDVR variants", []Decision{bypass, bypass}, Bypass},
		{"all owned", []Decision{own, own, own}, Owner},
		{"owned, one variant offline everywhere", []Decision{nobody, own, own}, Owner},
		{"all elsewhere", []Decision{not, not}, NotOwner},
		{"nobody has any", []Decision{nobody, nobody}, NotOwner},
		{"one unknown", []Decision{own, unk}, Unknown},
		{"mixed: first variant decides", []Decision{not, own}, NotOwner},
		{"non-nDVR variant ignored", []Decision{bypass, own}, Owner},
	}
	for _, tc := range tests {
		if got := Combine(tc.in).Verdict; got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
