package registry

import (
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

//
// Since we are not yet using build tags to determine which connections are included, we register them all here...
//

var GlobalConnectionRegistry = &ConnectionRegistryImpl{
	mu:          &sync.RWMutex{},
	connections: map[string]types.Connection{},
	creators:    map[types.TransportType]ConnectionCreatorFunc{},
}

func init() {
	// TODO add registrations here
}
