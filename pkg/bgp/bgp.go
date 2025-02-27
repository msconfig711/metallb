// SPDX-License-Identifier:Apache-2.0

package bgp // import "go.universe.tf/metallb/pkg/bgp"

import (
	"io"
	"net"
	"reflect"
	"time"

	"github.com/go-kit/kit/log"
	"go.universe.tf/metallb/pkg/config"
)

// Advertisement represents one network path and its BGP attributes.
type Advertisement struct {
	// The prefix being advertised to the peer.
	Prefix *net.IPNet
	// The local preference of this route. Only propagated to IBGP
	// peers (i.e. where the peer ASN matches the local ASN).
	LocalPref uint32
	// BGP communities to attach to the path.
	Communities []uint32
	// The as-path of this route, prepend is the number of local asn appended.
	Prepend uint32
}

// Equal returns true if a and b are equivalent advertisements.
func (a *Advertisement) Equal(b *Advertisement) bool {
	if a.Prefix.String() != b.Prefix.String() {
		return false
	}
	if a.LocalPref != b.LocalPref {
		return false
	}
	if a.Prepend != b.Prepend {
		return false
	}
	return reflect.DeepEqual(a.Communities, b.Communities)
}

type Session interface {
	io.Closer
	Set(advs ...*Advertisement) error
}

type SessionManager interface {
	NewSession(logger log.Logger, addr string, srcAddr net.IP, myASN uint32, routerID net.IP, asn uint32, hold, keepalive time.Duration, password, myNode, bfdProfile string, ebgpMultiHop bool) (Session, error)
	SyncBFDProfiles(profiles map[string]*config.BFDProfile) error
}
