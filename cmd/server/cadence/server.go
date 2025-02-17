// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cadence

import (
	"log"
	"time"

	"go.uber.org/cadence/.gen/go/cadence/workflowserviceclient"

	"github.com/uber/cadence/client"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/archiver"
	"github.com/uber/cadence/common/archiver/provider"
	"github.com/uber/cadence/common/authorization"
	"github.com/uber/cadence/common/blobstore/filestore"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/config"
	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/elasticsearch"
	"github.com/uber/cadence/common/log/loggerimpl"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/messaging/kafka"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/service"
	"github.com/uber/cadence/service/frontend"
	"github.com/uber/cadence/service/history"
	"github.com/uber/cadence/service/matching"
	"github.com/uber/cadence/service/worker"
)

type (
	server struct {
		name   string
		cfg    *config.Config
		doneC  chan struct{}
		daemon common.Daemon
	}
)

const (
	frontendService = "frontend"
	historyService  = "history"
	matchingService = "matching"
	workerService   = "worker"
)

// newServer returns a new instance of a daemon
// that represents a cadence service
func newServer(service string, cfg *config.Config) common.Daemon {
	return &server{
		cfg:   cfg,
		name:  service,
		doneC: make(chan struct{}),
	}
}

// Start starts the server
func (s *server) Start() {
	if _, ok := s.cfg.Services[s.name]; !ok {
		log.Fatalf("`%v` service missing config", s)
	}
	s.daemon = s.startService()
}

// Stop stops the server
func (s *server) Stop() {

	if s.daemon == nil {
		return
	}

	select {
	case <-s.doneC:
	default:
		s.daemon.Stop()
		select {
		case <-s.doneC:
		case <-time.After(time.Minute):
			log.Printf("timed out waiting for server %v to exit\n", s.name)
		}
	}
}

// startService starts a service with the given name and config
func (s *server) startService() common.Daemon {
	params := service.BootstrapParams{}
	params.Name = "cadence-" + s.name

	zapLogger, err := s.cfg.Log.NewZapLogger()
	if err != nil {
		log.Fatal("failed to create the zap logger, err: ", err.Error())
	}
	params.Logger = loggerimpl.NewLogger(zapLogger)
	params.UpdateLoggerWithServiceName(params.Name)
	params.PersistenceConfig = s.cfg.Persistence

	params.DynamicConfig, err = dynamicconfig.NewFileBasedClient(&s.cfg.DynamicConfigClient, params.Logger, s.doneC)
	if err != nil {
		log.Printf("error creating file based dynamic config client, use no-op config client instead. error: %v", err)
		params.DynamicConfig = dynamicconfig.NewNopClient()
	}
	clusterMetadata := s.cfg.ClusterMetadata
	dc := dynamicconfig.NewCollection(
		params.DynamicConfig,
		params.Logger,
		dynamicconfig.ClusterNameFilter(clusterMetadata.CurrentClusterName),
	)

	svcCfg := s.cfg.Services[s.name]
	params.MetricScope = svcCfg.Metrics.NewScope(params.Logger, params.Name)
	params.RPCFactory = svcCfg.RPC.NewFactory(params.Name, params.Logger)
	params.MembershipFactory, err = s.cfg.Ringpop.NewFactory(
		params.RPCFactory.GetDispatcher(),
		params.Name,
		params.Logger,
	)
	if err != nil {
		log.Fatalf("error creating ringpop factory: %v", err)
	}
	params.PProfInitializer = svcCfg.PProf.NewInitializer(params.Logger)

	params.DCRedirectionPolicy = s.cfg.DCRedirectionPolicy

	params.MetricsClient = metrics.NewClient(params.MetricScope, service.GetMetricsServiceIdx(params.Name, params.Logger))

	params.ClusterMetadata = cluster.NewMetadata(
		params.Logger,
		dc.GetBoolProperty(dynamicconfig.EnableGlobalDomain, clusterMetadata.EnableGlobalDomain),
		clusterMetadata.FailoverVersionIncrement,
		clusterMetadata.MasterClusterName,
		clusterMetadata.CurrentClusterName,
		clusterMetadata.ClusterInformation,
	)

	if s.cfg.PublicClient.HostPort != "" {
		params.DispatcherProvider = client.NewDNSYarpcDispatcherProvider(params.Logger, s.cfg.PublicClient.RefreshInterval)
	} else {
		log.Fatalf("need to provide an endpoint config for PublicClient")
	}

	advancedVisMode := dc.GetStringProperty(
		dynamicconfig.AdvancedVisibilityWritingMode,
		common.GetDefaultAdvancedVisibilityWritingMode(params.PersistenceConfig.IsAdvancedVisibilityConfigExist()),
	)()
	isAdvancedVisEnabled := advancedVisMode != common.AdvancedVisibilityWritingModeOff
	if isAdvancedVisEnabled {
		params.MessagingClient = kafka.NewKafkaClient(&s.cfg.Kafka, params.MetricsClient, params.Logger, params.MetricScope, isAdvancedVisEnabled)
	} else {
		params.MessagingClient = nil
	}

	if isAdvancedVisEnabled {
		// verify config of advanced visibility store
		advancedVisStoreKey := s.cfg.Persistence.AdvancedVisibilityStore
		advancedVisStore, ok := s.cfg.Persistence.DataStores[advancedVisStoreKey]
		if !ok {
			log.Fatalf("not able to find advanced visibility store in config: %v", advancedVisStoreKey)
		}

		params.ESConfig = advancedVisStore.ElasticSearch
		params.ESConfig.SetUsernamePassword()
		esClient, err := elasticsearch.NewGenericClient(params.ESConfig, params.Logger)
		if err != nil {
			log.Fatalf("error creating elastic search client: %v", err)
		}
		params.ESClient = esClient

		// verify index name
		indexName, ok := params.ESConfig.Indices[common.VisibilityAppName]
		if !ok || len(indexName) == 0 {
			log.Fatalf("elastic search config missing visibility index")
		}
	}

	dispatcher, err := params.DispatcherProvider.Get(common.FrontendServiceName, s.cfg.PublicClient.HostPort)
	if err != nil {
		log.Fatalf("failed to construct dispatcher: %v", err)
	}
	params.PublicClient = workflowserviceclient.New(dispatcher.ClientConfig(common.FrontendServiceName))

	params.ArchivalMetadata = archiver.NewArchivalMetadata(
		dc,
		s.cfg.Archival.History.Status,
		s.cfg.Archival.History.EnableRead,
		s.cfg.Archival.Visibility.Status,
		s.cfg.Archival.Visibility.EnableRead,
		&s.cfg.DomainDefaults.Archival,
	)

	params.ArchiverProvider = provider.NewArchiverProvider(s.cfg.Archival.History.Provider, s.cfg.Archival.Visibility.Provider)
	params.PersistenceConfig.TransactionSizeLimit = dc.GetIntProperty(dynamicconfig.TransactionSizeLimit, common.DefaultTransactionSizeLimit)
	params.PersistenceConfig.ErrorInjectionRate = dc.GetFloat64Property(dynamicconfig.PersistenceErrorInjectionRate, 0)
	params.Authorizer = authorization.NewNopAuthorizer()
	params.BlobstoreClient, err = filestore.NewFilestoreClient(s.cfg.Blobstore.Filestore)
	if err != nil {
		log.Printf("failed to create file blobstore client, will continue startup without it: %v", err)
		params.BlobstoreClient = nil
	}

	params.Logger.Info("Starting service " + s.name)

	var daemon common.Daemon

	switch s.name {
	case frontendService:
		daemon, err = frontend.NewService(&params)
	case historyService:
		daemon, err = history.NewService(&params)
	case matchingService:
		daemon, err = matching.NewService(&params)
	case workerService:
		daemon, err = worker.NewService(&params)
	}
	if err != nil {
		params.Logger.Fatal("Fail to start "+s.name+" service ", tag.Error(err))
	}

	go execute(daemon, s.doneC)

	return daemon
}

// execute runs the daemon in a separate go routine
func execute(d common.Daemon, doneC chan struct{}) {
	d.Start()
	close(doneC)
}
