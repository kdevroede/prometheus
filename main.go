// Copyright 2013 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/extraction"
	registry "github.com/prometheus/client_golang/prometheus"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/notification"
	"github.com/prometheus/prometheus/retrieval"
	"github.com/prometheus/prometheus/rules/manager"
	"github.com/prometheus/prometheus/storage/metric/ng"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/storage/remote/opentsdb"
	"github.com/prometheus/prometheus/web"
	"github.com/prometheus/prometheus/web/api"
)

const deletionBatchSize = 100

// Commandline flags.
var (
	configFile         = flag.String("configFile", "prometheus.conf", "Prometheus configuration file name.")
	metricsStoragePath = flag.String("metricsStoragePath", "/tmp/metrics", "Base path for metrics storage.")

	alertmanagerUrl = flag.String("alertmanager.url", "", "The URL of the alert manager to send notifications to.")

	remoteTSDBUrl     = flag.String("storage.remote.url", "", "The URL of the OpenTSDB instance to send samples to.")
	remoteTSDBTimeout = flag.Duration("storage.remote.timeout", 30*time.Second, "The timeout to use when sending samples to OpenTSDB.")

	samplesQueueCapacity      = flag.Int("storage.queue.samplesCapacity", 4096, "The size of the unwritten samples queue.")
	diskAppendQueueCapacity   = flag.Int("storage.queue.diskAppendCapacity", 1000000, "The size of the queue for items that are pending writing to disk.")
	memoryAppendQueueCapacity = flag.Int("storage.queue.memoryAppendCapacity", 10000, "The size of the queue for items that are pending writing to memory.")

	deleteInterval = flag.Duration("delete.interval", 11*time.Hour, "The amount of time between deletion of old values.")

	deleteAge = flag.Duration("delete.ageMaximum", 15*24*time.Hour, "The relative maximum age for values before they are deleted.")

	arenaFlushInterval = flag.Duration("arena.flushInterval", 15*time.Minute, "The period at which the in-memory arena is flushed to disk.")
	arenaTTL           = flag.Duration("arena.ttl", 10*time.Minute, "The relative age of values to purge to disk from memory.")

	notificationQueueCapacity = flag.Int("alertmanager.notificationQueueCapacity", 100, "The size of the queue for pending alert manager notifications.")

	concurrentRetrievalAllowance = flag.Int("concurrentRetrievalAllowance", 15, "The number of concurrent metrics retrieval requests allowed.")

	printVersion = flag.Bool("version", false, "print version information")

	shutdownTimeout = flag.Duration("shutdownGracePeriod", 0*time.Second, "The amount of time Prometheus gives background services to finish running when shutdown is requested.")
)

type prometheus struct {
	unwrittenSamples chan *extraction.Result

	ruleManager     manager.RuleManager
	targetManager   retrieval.TargetManager
	notifications   chan notification.NotificationReqs
	storage         storage_ng.Storage
	remoteTSDBQueue *remote.TSDBQueueManager

	closeOnce sync.Once
}

func (p *prometheus) interruptHandler() {
	notifier := make(chan os.Signal)
	signal.Notify(notifier, os.Interrupt, syscall.SIGTERM)

	<-notifier

	glog.Warning("Received SIGINT/SIGTERM; Exiting gracefully...")

	p.Close()

	os.Exit(0)
}

func (p *prometheus) Close() {
	p.closeOnce.Do(p.close)
}

func (p *prometheus) close() {
	// The "Done" remarks are a misnomer for some subsystems due to lack of
	// blocking and synchronization.
	glog.Info("Shutdown has been requested; subsytems are closing:")
	p.targetManager.Stop()
	glog.Info("Remote Target Manager: Done")
	p.ruleManager.Stop()
	glog.Info("Rule Executor: Done")

	glog.Infof("Waiting %s for background systems to exit and flush before finalizing (DO NOT INTERRUPT THE PROCESS) ...", *shutdownTimeout)

	// Wart: We should have a concrete form of synchronization for this, not a
	//       hokey sleep statement.
	time.Sleep(*shutdownTimeout)

	close(p.unwrittenSamples)

	p.storage.Close()
	glog.Info("Local Storage: Done")

	if p.remoteTSDBQueue != nil {
		p.remoteTSDBQueue.Close()
		glog.Info("Remote Storage: Done")
	}

	close(p.notifications)
	glog.Info("Sundry Queues: Done")
	glog.Info("See you next time!")
}

func main() {
	// TODO(all): Future additions to main should be, where applicable, glumped
	// into the prometheus struct above---at least where the scoping of the entire
	// server is concerned.
	flag.Parse()

	versionInfoTmpl.Execute(os.Stdout, BuildInfo)

	if *printVersion {
		os.Exit(0)
	}

	conf, err := config.LoadFromFile(*configFile)
	if err != nil {
		glog.Fatalf("Error loading configuration from %s: %v", *configFile, err)
	}

	persistence, err := storage_ng.NewDiskPersistence(*metricsStoragePath)
	if err != nil {
		glog.Fatal("Error opening disk persistence: ", err)
	}
	memStorage := storage_ng.NewMemorySeriesStorage(persistence)
	//registry.MustRegister(memStorage)

	var remoteTSDBQueue *remote.TSDBQueueManager
	if *remoteTSDBUrl == "" {
		glog.Warningf("No TSDB URL provided; not sending any samples to long-term storage")
	} else {
		openTSDB := opentsdb.NewClient(*remoteTSDBUrl, *remoteTSDBTimeout)
		remoteTSDBQueue = remote.NewTSDBQueueManager(openTSDB, 512)
		registry.MustRegister(remoteTSDBQueue)
		go remoteTSDBQueue.Run()
	}

	unwrittenSamples := make(chan *extraction.Result, *samplesQueueCapacity)
	ingester := &retrieval.MergeLabelsIngester{
		Labels:          conf.GlobalLabels(),
		CollisionPrefix: clientmodel.ExporterLabelPrefix,

		Ingester: retrieval.ChannelIngester(unwrittenSamples),
	}

	// Queue depth will need to be exposed
	targetManager := retrieval.NewTargetManager(ingester, *concurrentRetrievalAllowance)
	targetManager.AddTargetsFromConfig(conf)

	notifications := make(chan notification.NotificationReqs, *notificationQueueCapacity)

	// Queue depth will need to be exposed
	ruleManager := manager.NewRuleManager(&manager.RuleManagerOptions{
		Results:            unwrittenSamples,
		Notifications:      notifications,
		EvaluationInterval: conf.EvaluationInterval(),
		Storage:            memStorage,
		PrometheusUrl:      web.MustBuildServerUrl(),
	})
	if err := ruleManager.AddRulesFromConfig(conf); err != nil {
		glog.Fatal("Error loading rule files: ", err)
	}
	go ruleManager.Run()

	notificationHandler := notification.NewNotificationHandler(*alertmanagerUrl, notifications)
	registry.MustRegister(notificationHandler)
	go notificationHandler.Run()

	flags := map[string]string{}

	flag.VisitAll(func(f *flag.Flag) {
		flags[f.Name] = f.Value.String()
	})

	prometheusStatus := &web.PrometheusStatusHandler{
		BuildInfo:   BuildInfo,
		Config:      conf.String(),
		RuleManager: ruleManager,
		TargetPools: targetManager.Pools(),
		Flags:       flags,
		Birth:       time.Now(),
	}

	alertsHandler := &web.AlertsHandler{
		RuleManager: ruleManager,
	}

	consolesHandler := &web.ConsolesHandler{
		Storage: memStorage,
	}

	databasesHandler := &web.DatabasesHandler{
		Provider:        nil,
		RefreshInterval: 5 * time.Minute,
	}

	metricsService := &api.MetricsService{
		Config:        &conf,
		TargetManager: targetManager,
		Storage:       memStorage,
	}

	prometheus := &prometheus{
		unwrittenSamples: unwrittenSamples,

		ruleManager:     ruleManager,
		targetManager:   targetManager,
		notifications:   notifications,
		storage:         memStorage,
		remoteTSDBQueue: remoteTSDBQueue,
	}
	defer prometheus.Close()

	webService := &web.WebService{
		StatusHandler:    prometheusStatus,
		MetricsHandler:   metricsService,
		DatabasesHandler: databasesHandler,
		ConsolesHandler:  consolesHandler,
		AlertsHandler:    alertsHandler,

		QuitDelegate: prometheus.Close,
	}

	/* TODO: implement serving storage.
	storageStarted := make(chan bool)
	go ts.Serve(storageStarted)
	<-storageStarted
	*/
	go memStorage.Serve()

	go prometheus.interruptHandler()

	go func() {
		err := webService.ServeForever()
		if err != nil {
			glog.Fatal(err)
		}
	}()

	// TODO(all): Migrate this into prometheus.serve().
	for block := range unwrittenSamples {
		if block.Err == nil && len(block.Samples) > 0 {
			memStorage.AppendSamples(block.Samples)
			if remoteTSDBQueue != nil {
				remoteTSDBQueue.Queue(block.Samples)
			}
		}
	}
}
