// SPDX-License-Identifier:Apache-2.0

package k8s

import (
	"sync"

	"github.com/go-kit/kit/log"
	"go.universe.tf/metallb/pkg/config"
	"go.universe.tf/metallb/pkg/k8s/controllers"
	"go.universe.tf/metallb/pkg/k8s/epslices"
	v1 "k8s.io/api/core/v1"
)

type Listener struct {
	sync.Mutex
	ServiceChanged func(log.Logger, string, *v1.Service, epslices.EpsOrSlices) controllers.SyncState
	ConfigChanged  func(log.Logger, *config.Config) controllers.SyncState
	NodeChanged    func(log.Logger, *v1.Node) controllers.SyncState
}

func (l *Listener) ServiceHandler(logger log.Logger, serviceName string, svc *v1.Service, endpointsOrSlices epslices.EpsOrSlices) controllers.SyncState {
	l.Lock()
	defer l.Unlock()
	return l.ServiceChanged(logger, serviceName, svc, endpointsOrSlices)
}
func (l *Listener) ConfigHandler(logger log.Logger, config *config.Config) controllers.SyncState {
	l.Lock()
	defer l.Unlock()
	return l.ConfigChanged(logger, config)
}

func (l *Listener) NodeHandler(logger log.Logger, node *v1.Node) controllers.SyncState {
	l.Lock()
	defer l.Unlock()
	return l.NodeChanged(logger, node)
}
