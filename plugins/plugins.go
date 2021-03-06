// Copyright 2018 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

// Package plugins implements plugin management for the policy engine.
package plugins

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/config"
	"github.com/open-policy-agent/opa/plugins/rest"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/util"
)

// Factory defines the interface OPA uses to instantiate your plugin.
//
// When OPA processes it's configuration it looks for factories that
// have been registered by calling runtime.RegisterPlugin. Factories
// are registered to a name which is used to key into the
// configuration blob. If your plugin has not been configured, your
// factory will not be invoked.
//
//   plugins:
//     my_plugin1:
//       some_key: foo
//     # my_plugin2:
//     #   some_key2: bar
//
// If OPA was started with the configuration above and received two
// calls to runtime.RegisterPlugins (one with NAME "my_plugin1" and
// one with NAME "my_plugin2"), it would only invoke the factory for
// for my_plugin1.
//
// OPA instantiates and reconfigures plugins in two steps. First, OPA
// will call Validate to check the configuration. Assuming the
// configuration is valid, your factory should return a configuration
// value that can be used to construct your plugin. Second, OPA will
// call New to instantiate your plugin providing the configuration
// value returned from the Validate call.
//
// Validate receives a slice of bytes representing plugin
// configuration and returns a configuration value that can be used to
// instantiate your plugin. The manager is provided to give access to
// the OPA's compiler, storage layer, adn global configuration. Your
// Validate function will typically:
//
//  1. Deserialize the raw config bytes
//  2. Validate the deserialized config for semantic errors
//  3. Inject default values
//  4. Return a deserialized/parsed config
//
// New receives a valid configuration for your plugin and returns a
// plugin object. Your New function will typically:
//
//  1. Cast the config value to it's own type
//  2. Instantiate a plugin object
//  3. Return the plugin object
type Factory interface {
	Validate(manager *Manager, config []byte) (interface{}, error)
	New(manager *Manager, config interface{}) Plugin
}

// Plugin defines the interface OPA uses to manage your plugin.
//
// When OPA starts it will start all of the plugins it was configured
// to instantiate. Each time a new plugin is configured (via
// discovery), OPA will start it. You can use the Start call to spawn
// additional goroutines or perform initialization tasks.
//
// Currently OPA will not call Stop on plugins.
//
// When OPA receives new configuration for your plugin via discovery
// it will first Validate the configuration using your factory and
// then call Reconfigure.
type Plugin interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context)
	Reconfigure(ctx context.Context, config interface{})
}

// Manager implements lifecycle management of plugins and gives plugins access
// to engine-wide components like storage.
type Manager struct {
	Store  storage.Store
	Config *config.Config
	Info   *ast.Term
	ID     string

	compiler           *ast.Compiler
	compilerMux        sync.RWMutex
	services           map[string]rest.Client
	plugins            []namedplugin
	registeredTriggers []func(txn storage.Transaction)
	mtx                sync.Mutex
}

type namedplugin struct {
	name   string
	plugin Plugin
}

// Info sets the runtime information on the manager. The runtime information is
// propagated to opa.runtime() built-in function calls.
func Info(term *ast.Term) func(*Manager) {
	return func(m *Manager) {
		m.Info = term
	}
}

// New creates a new Manager using config.
func New(raw []byte, id string, store storage.Store, opts ...func(*Manager)) (*Manager, error) {

	parsedConfig, err := config.ParseConfig(raw, id)
	if err != nil {
		return nil, err
	}

	services, err := parseServicesConfig(parsedConfig.Services)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		Store:    store,
		Config:   parsedConfig,
		ID:       id,
		services: services,
	}

	for _, f := range opts {
		f(m)
	}

	return m, nil
}

// Labels returns the set of labels from the configuration.
func (m *Manager) Labels() map[string]string {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	return m.Config.Labels
}

// Register adds a plugin to the manager. When the manager is started, all of
// the plugins will be started.
func (m *Manager) Register(name string, plugin Plugin) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.plugins = append(m.plugins, namedplugin{
		name:   name,
		plugin: plugin,
	})
}

// Plugins returns the list of plugins registered with the manager.
func (m *Manager) Plugins() []string {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	result := make([]string, len(m.plugins))
	for i := range m.plugins {
		result[i] = m.plugins[i].name
	}
	return result
}

// Plugin returns the plugin registered with name or nil if name is not found.
func (m *Manager) Plugin(name string) Plugin {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	for i := range m.plugins {
		if m.plugins[i].name == name {
			return m.plugins[i].plugin
		}
	}
	return nil
}

// GetCompiler returns the manager's compiler.
func (m *Manager) GetCompiler() *ast.Compiler {
	m.compilerMux.RLock()
	defer m.compilerMux.RUnlock()
	return m.compiler
}

func (m *Manager) setCompiler(compiler *ast.Compiler) {
	m.compilerMux.Lock()
	defer m.compilerMux.Unlock()
	m.compiler = compiler
}

// RegisterCompilerTrigger registers for change notifications when the compiler
// is changed.
func (m *Manager) RegisterCompilerTrigger(f func(txn storage.Transaction)) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.registeredTriggers = append(m.registeredTriggers, f)
}

// Start starts the manager.
func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}

	err := storage.Txn(ctx, m.Store, storage.TransactionParams{}, func(txn storage.Transaction) error {
		compiler, err := loadCompilerFromStore(ctx, m.Store, txn)
		if err != nil {
			return err
		}
		m.setCompiler(compiler)
		return nil
	})

	if err != nil {
		return err
	}

	if err := func() error {
		m.mtx.Lock()
		defer m.mtx.Unlock()

		for _, p := range m.plugins {
			if err := p.plugin.Start(ctx); err != nil {
				return err
			}
		}

		return nil
	}(); err != nil {
		return err
	}

	config := storage.TriggerConfig{OnCommit: m.onCommit}

	return storage.Txn(ctx, m.Store, storage.WriteParams, func(txn storage.Transaction) error {
		_, err := m.Store.Register(ctx, txn, config)
		return err
	})
}

// Stop stops the manager, stopping all the plugins registered with it
func (m *Manager) Stop(ctx context.Context) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	for _, p := range m.plugins {
		p.plugin.Stop(ctx)
	}
}

// Reconfigure updates the configuration on the manager.
func (m *Manager) Reconfigure(config *config.Config) error {
	services, err := parseServicesConfig(config.Services)
	if err != nil {
		return err
	}
	m.mtx.Lock()
	defer m.mtx.Unlock()
	config.Labels = m.Config.Labels // don't overwrite labels
	m.Config = config
	for name, client := range services {
		m.services[name] = client
	}
	return nil
}

func (m *Manager) onCommit(ctx context.Context, txn storage.Transaction, event storage.TriggerEvent) {
	if event.PolicyChanged() {
		compiler, _ := loadCompilerFromStore(ctx, m.Store, txn)
		m.setCompiler(compiler)
		for _, f := range m.registeredTriggers {
			f(txn)
		}
	}
}

func loadCompilerFromStore(ctx context.Context, store storage.Store, txn storage.Transaction) (*ast.Compiler, error) {
	policies, err := store.ListPolicies(ctx, txn)
	if err != nil {
		return nil, err
	}
	modules := map[string]*ast.Module{}

	for _, policy := range policies {
		bs, err := store.GetPolicy(ctx, txn, policy)
		if err != nil {
			return nil, err
		}
		module, err := ast.ParseModule(policy, string(bs))
		if err != nil {
			return nil, err
		}
		modules[policy] = module
	}

	compiler := ast.NewCompiler()
	compiler.Compile(modules)
	return compiler, nil
}

// Client returns a client for communicating with a remote service.
func (m *Manager) Client(name string) rest.Client {
	return m.services[name]
}

// Services returns a list of services that m can provide clients for.
func (m *Manager) Services() []string {
	s := make([]string, 0, len(m.services))
	for name := range m.services {
		s = append(s, name)
	}
	return s
}

// parseServicesConfig returns a set of named service clients. The service
// clients can be specified either as an array or as a map. Some systems (e.g.,
// Helm) do not have proper support for configuration values nested under
// arrays, so just support both here.
func parseServicesConfig(raw json.RawMessage) (map[string]rest.Client, error) {

	services := map[string]rest.Client{}

	var arr []json.RawMessage
	var obj map[string]json.RawMessage

	if err := util.Unmarshal(raw, &arr); err == nil {
		for _, s := range arr {
			client, err := rest.New(s)
			if err != nil {
				return nil, err
			}
			services[client.Service()] = client
		}
	} else if util.Unmarshal(raw, &obj) == nil {
		for k := range obj {
			client, err := rest.New(obj[k], rest.Name(k))
			if err != nil {
				return nil, err
			}
			services[client.Service()] = client
		}
	} else {
		// Return error from array decode as that is the default format.
		return nil, err
	}

	return services, nil
}
