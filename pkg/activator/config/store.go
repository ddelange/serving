/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"context"
	"sync/atomic"

	netcfg "knative.dev/networking/pkg/config"
	"knative.dev/pkg/configmap"
)

type cfgKey struct{}

// Config is the configuration for the activator.
type Config struct {
	Network *netcfg.Config
}

// FromContext obtains a Config injected into the passed context.
func FromContext(ctx context.Context) *Config {
	return ctx.Value(cfgKey{}).(*Config)
}

// Store loads/unloads our untyped configuration.
type Store struct {
	*configmap.UntypedStore

	// current is the current Config.
	current atomic.Value
}

// NewStore creates a new configuration Store.
func NewStore(logger configmap.Logger, onAfterStore ...func(name string, value interface{})) *Store {
	s := &Store{}

	// Append an update function to run after a ConfigMap has updated to update the
	// current state of the Config.
	onAfterStore = append(onAfterStore, func(_ string, _ interface{}) {
		c := &Config{}
		// this allows dynamic updating of the config-network
		// this is necessary for not requiring activator restart for system-internal-tls in the future
		// however, current implementation is not there yet.
		// see https://github.com/knative/serving/issues/13754
		network := s.UntypedLoad(netcfg.ConfigMapName)
		if network != nil {
			c.Network = network.(*netcfg.Config).DeepCopy()
		}
		s.current.Store(c)
	})
	s.UntypedStore = configmap.NewUntypedStore(
		"activator",
		logger,
		configmap.Constructors{
			netcfg.ConfigMapName: netcfg.NewConfigFromConfigMap,
		},
		onAfterStore...,
	)
	return s
}

// ToContext stores the configuration Store in the passed context.
func (s *Store) ToContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, cfgKey{}, s.current.Load())
}
