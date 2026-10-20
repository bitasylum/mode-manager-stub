/*
Copyright 2026 Bitasylum

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

// mode-manager-stub is a test stand-in for the mode manager. It writes the
// cluster's mode to a ClusterMode, and serves a page for changing it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/bitasylum/mode-operator/client/clustermode"
	"github.com/bitasylum/mode-operator/client/fingerprint"
	"github.com/bitasylum/mode-operator/client/startup"

	"github.com/bitasylum/mode-manager-stub/internal/manager"
	"github.com/bitasylum/mode-manager-stub/internal/web"
)

// version is set at build time.
var version = "dev"

func main() {
	klog.InitFlags(nil)
	var o options
	flag.StringVar(&o.cfg.ClusterMode, "cluster-mode", "default", "The ClusterMode to write.")
	flag.StringVar(&o.cfg.Definition, "definition", "standard", "The ClusterModeDefinition the ClusterMode follows.")
	flag.StringVar(&o.cfg.WriterLabel, "writer-label", "mode-manager-stub",
		"The value of the ClusterMode's mode.bitasylum.org/writer label.")
	flag.StringVar(&o.cfg.BuiltinDefault, "builtin-default", "standby",
		"The mode to run in without the CRDs or the definition.")
	flag.StringVar(&o.knownModes, "known-modes", "",
		"Comma-separated modes the stub can run in; a stored mode outside them is reset. Empty means any.")
	flag.StringVar(&o.cfg.Namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"The namespace of the fingerprint ConfigMap. Defaults to $POD_NAMESPACE.")
	flag.StringVar(&o.cfg.FingerprintConfigMap, "fingerprint-configmap", "",
		"The fingerprint ConfigMap. Defaults to mode-manager-fingerprint-<cluster-mode>.")
	flag.DurationVar(&o.cfg.DiscoveryInterval, "discovery-interval", time.Minute,
		"How often to check for the CRDs while they aren't installed.")
	flag.DurationVar(&o.cfg.Debounce, "debounce", 2*time.Second,
		"How long to wait after a node change before saving the fingerprint.")
	flag.DurationVar(&o.cfg.MoveStepTimeout, "move-step-timeout", 10*time.Minute,
		"How long a move waits for each mode to settle before it stops. 0 means no limit.")
	flag.StringVar(&o.listen, "listen", ":8080", "The address to serve the page and API on.")
	flag.StringVar(&o.kubeconfig, "kubeconfig", "", "A kubeconfig, when running outside a cluster.")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	log := klog.Background()
	err := run(klog.NewContext(ctx, log), o)
	stop()
	if err != nil {
		log.Error(err, "The mode manager stopped")
		klog.Flush()
		os.Exit(1)
	}
}

type options struct {
	cfg                            manager.Config
	knownModes, listen, kubeconfig string
}

// run runs the stub until ctx is done, or it fails.
func run(ctx context.Context, o options) error {
	log := klog.FromContext(ctx)
	cfg := o.cfg
	for m := range strings.SplitSeq(o.knownModes, ",") {
		if m = strings.TrimSpace(m); m != "" {
			cfg.KnownModes = append(cfg.KnownModes, m)
		}
	}
	if cfg.FingerprintConfigMap == "" {
		cfg.FingerprintConfigMap = fingerprint.DefaultName(cfg.ClusterMode)
	}
	if err := validate(cfg); err != nil {
		return err
	}

	restCfg, err := restConfig(o.kubeconfig)
	if err != nil {
		return fmt.Errorf("loading a Kubernetes config: %w", err)
	}
	restCfg.UserAgent = "mode-manager-stub/" + version
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating a Kubernetes client: %w", err)
	}
	reader, err := clustermode.NewReaderForConfig(restCfg, cfg.ClusterMode)
	if err != nil {
		return fmt.Errorf("creating a ClusterMode client: %w", err)
	}
	recorder, stopRecorder := startup.NewRecorder(ctx, kube, "mode-manager-stub")
	defer stopRecorder()

	// A failed server stops the manager, and the manager stopping stops the
	// server.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := manager.New(ctx, cfg, reader, kube, recorder)
	server := &http.Server{
		Addr:              o.listen,
		Handler:           web.Handler(ctx, m),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
	}
	served := make(chan error, 1)
	go func() {
		log.Info("Serving the page", "address", o.listen, "version", version)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else {
			cancel()
		}
		served <- err
	}()

	runErr := m.Run(ctx)
	shutdown, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelShutdown()
	_ = server.Shutdown(shutdown)
	if err := <-served; err != nil {
		return fmt.Errorf("serving the page: %w", err)
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

// validate checks the flags. A value the API server would refuse would
// otherwise leave the stub retrying for good.
func validate(cfg manager.Config) error {
	var errs []error
	name := func(option, value string, maxLen int) {
		if msgs := validation.IsDNS1123Subdomain(value); len(msgs) > 0 || len(value) > maxLen {
			errs = append(errs, fmt.Errorf("--%s=%q isn't a valid name of at most %d characters", option, value, maxLen))
		}
	}
	mode := func(option, value string) {
		if msgs := validation.IsDNS1123Label(value); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("--%s: %q isn't a valid mode: %s", option, value, strings.Join(msgs, "; ")))
		}
	}
	name("cluster-mode", cfg.ClusterMode, validation.DNS1123LabelMaxLength)
	name("definition", cfg.Definition, validation.DNS1123SubdomainMaxLength)
	name("fingerprint-configmap", cfg.FingerprintConfigMap, validation.DNS1123SubdomainMaxLength)
	if msgs := validation.IsValidLabelValue(cfg.WriterLabel); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("--writer-label: %s", strings.Join(msgs, "; ")))
	}
	mode("builtin-default", cfg.BuiltinDefault)
	for _, m := range cfg.KnownModes {
		mode("known-modes", m)
	}
	if len(cfg.KnownModes) > 0 && !slices.Contains(cfg.KnownModes, cfg.BuiltinDefault) {
		errs = append(errs, fmt.Errorf("--builtin-default=%s isn't in --known-modes", cfg.BuiltinDefault))
	}
	if cfg.Namespace == "" {
		errs = append(errs, errors.New("set --namespace or $POD_NAMESPACE"))
	}
	if cfg.DiscoveryInterval <= 0 {
		errs = append(errs, errors.New("--discovery-interval must be more than 0"))
	}
	if cfg.Debounce < 0 || cfg.MoveStepTimeout < 0 {
		errs = append(errs, errors.New("--debounce and --move-step-timeout can't be negative"))
	}
	return errors.Join(errs...)
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}
