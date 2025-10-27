package registry

import (
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

//
// Since we are not yet using build tags to determine which transports are included, we register them all here...
//

var GlobalTransportRegistry = &TransportRegistryImpl{
	mu:         &sync.RWMutex{},
	transports: map[string]types.Transport{},
	creators:   map[types.TransportType]TransportCreatorFunc{},
}

func init() {
	// TODO add registrations here
}
