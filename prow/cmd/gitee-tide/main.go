/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/test-infra/prow/interrupts"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"k8s.io/test-infra/pkg/flagutil"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/config/secret"
	prowflagutil "k8s.io/test-infra/prow/flagutil"
	tide "k8s.io/test-infra/prow/gitee-tide"
	tideClient "k8s.io/test-infra/prow/gitee-tide/client"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/metrics"
	"k8s.io/test-infra/prow/pjutil"
)

type options struct {
	port int

	configPath    string
	jobConfigPath string

	syncThrottle   int
	statusThrottle int

	dryRun     bool
	runOnce    bool
	kubernetes prowflagutil.KubernetesOptions
	gitee      prowflagutil.GiteeOptions
	storage    prowflagutil.StorageClientOptions

	maxRecordsPerPool int
	// historyURI where Tide should store its action history.
	// Can be /local/path, gs://path/to/object or s3://path/to/object.
	// GCS writes will use the bucket's default acl for new objects. Ensure both that
	// a) the gcs credentials can write to this bucket
	// b) the default acls do not expose any private info
	historyURI string

	// statusURI where Tide store status update state.
	// Can be a /local/path, gs://path/to/object or s3://path/to/object.
	// GCS writes will use the bucket's default acl for new objects. Ensure both that
	// a) the gcs credentials can write to this bucket
	// b) the default acls do not expose any private info
	statusURI string
}

func (o *options) Validate() error {
	for idx, group := range []flagutil.OptionGroup{&o.kubernetes, &o.gitee, &o.storage} {
		if err := group.Validate(o.dryRun); err != nil {
			return fmt.Errorf("%d: %w", idx, err)
		}
	}
	return nil
}

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	var o options
	fs.IntVar(&o.port, "port", 8888, "Port to listen on.")
	fs.StringVar(&o.configPath, "config-path", "", "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")
	fs.BoolVar(&o.dryRun, "dry-run", true, "Whether to mutate any real-world state.")
	fs.BoolVar(&o.runOnce, "run-once", false, "If true, run only once then quit.")
	for _, group := range []flagutil.OptionGroup{&o.kubernetes, &o.gitee, &o.storage} {
		group.AddFlags(fs)
	}
	fs.IntVar(&o.syncThrottle, "sync-hourly-tokens", 800, "The maximum number of tokens per hour to be used by the sync controller.")
	fs.IntVar(&o.statusThrottle, "status-hourly-tokens", 400, "The maximum number of tokens per hour to be used by the status controller.")
	fs.IntVar(&o.maxRecordsPerPool, "max-records-per-pool", 1000, "The maximum number of history records stored for an individual Tide pool.")
	fs.StringVar(&o.historyURI, "history-uri", "", "The /local/path,gs://path/to/object or s3://path/to/object to store tide action history. GCS writes will use the default object ACL for the bucket")
	fs.StringVar(&o.statusURI, "status-path", "", "The /local/path, gs://path/to/object or s3://path/to/object to store status controller state. GCS writes will use the default object ACL for the bucket.")

	fs.Parse(args)
	return o
}

func main() {
	logrusutil.ComponentInit()

	defer interrupts.WaitForGracefulShutdown()

	pjutil.ServePProf()

	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	opener, err := o.storage.StorageClient(context.Background())
	if err != nil {
		logrus.WithError(err).Fatal("Cannot create opener")
	}

	configAgent := &config.Agent{}
	if err := configAgent.Start(o.configPath, o.jobConfigPath); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}
	cfg := configAgent.Config

	secretAgent := &secret.Agent{}
	if err := secretAgent.Start([]string{o.gitee.TokenPath}); err != nil {
		logrus.WithError(err).Fatal("Error starting secrets agent.")
	}

	gec, err := o.gitee.GiteeClient(secretAgent, o.dryRun)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting Gitee client for sync.")
	}

	giteeSync := tideClient.NewClient(gec)
	giteeStatus := giteeSync

	gitClient, err := o.gitee.GitClient(secretAgent, o.dryRun)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting Git client.")
	}

	kubeCfg, err := o.kubernetes.InfrastructureClusterConfig(o.dryRun)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting kubeconfig.")
	}
	// Do not activate leader election here, as we do not use the `mgr` to control the lifecylcle of our cotrollers,
	// this would just be a no-op.
	mgr, err := manager.New(kubeCfg, manager.Options{Namespace: cfg().ProwJobNamespace, MetricsBindAddress: "0"})
	if err != nil {
		logrus.WithError(err).Fatal("Error constructing mgr.")
	}
	c, err := tide.NewController(giteeSync, giteeStatus, mgr, cfg, gitClient, o.maxRecordsPerPool, opener, o.historyURI, o.statusURI, nil)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating Tide controller.")
	}
	interrupts.Run(func(ctx context.Context) {
		if err := mgr.Start(ctx.Done()); err != nil {
			logrus.WithError(err).Fatal("Mgr failed.")
		}
		logrus.Info("Mgr finished gracefully.")
	})
	mgrSyncCtx, mgrSyncCtxCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer mgrSyncCtxCancel()
	if synced := mgr.GetCache().WaitForCacheSync(mgrSyncCtx.Done()); !synced {
		logrus.Fatal("Timed out waiting for cachesync")
	}
	interrupts.OnInterrupt(func() {
		c.Shutdown()
		if err := gitClient.Clean(); err != nil {
			logrus.WithError(err).Error("Could not clean up git client cache.")
		}
	})
	http.Handle("/", c)
	http.Handle("/history", c.History)
	server := &http.Server{Addr: ":" + strconv.Itoa(o.port)}

	// Push metrics to the configured prometheus pushgateway endpoint or serve them
	metrics.ExposeMetrics("tide", cfg().PushGateway)

	start := time.Now()
	sync(c)
	if o.runOnce {
		return
	}

	// serve data
	interrupts.ListenAndServe(server, 10*time.Second)

	// run the controller, but only after one sync period expires after our first run
	time.Sleep(time.Until(start.Add(cfg().Tide.SyncPeriod.Duration)))
	interrupts.Tick(func() {
		sync(c)
	}, func() time.Duration {
		return cfg().Tide.SyncPeriod.Duration
	})
}

func sync(c *tide.Controller) {
	if err := c.Sync(); err != nil {
		logrus.WithError(err).Error("Error syncing.")
	}
}

func tokensPerIteration(hourlyTokens int, iterPeriod time.Duration) int {
	tokenRate := float64(hourlyTokens) / float64(time.Hour)
	return int(tokenRate * float64(iterPeriod))
}