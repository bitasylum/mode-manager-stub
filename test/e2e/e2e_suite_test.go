//go:build e2e

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

// Package e2e tests the stub and mode-operator together on a kind cluster,
// which hack/test-e2e.sh creates.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/bitasylum/mode-manager-stub/internal/manager"
	"github.com/bitasylum/mode-manager-stub/internal/web"
)

const (
	stubNamespace     = "mode-manager"
	stubRelease       = "mode-manager-stub"
	operatorNamespace = "mode-operator-system"
	operatorRelease   = "mode-operator"
	workloadNamespace = "shop"
	clusterModeName   = "default"
)

var (
	ctx                      = context.Background()
	restConfig               *rest.Config
	kube                     kubernetes.Interface
	helmBin, kubectlBin      string
	stubImage, operatorImage string
	operatorChart            string
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "mode-manager-stub e2e")
}

var _ = BeforeSuite(func() {
	// The tests delete and install things cluster-wide, so they run only
	// on the cluster hack/test-e2e.sh created, through its own kubeconfig.
	kubeconfig, cluster := mustEnv("KUBECONFIG"), mustEnv("E2E_CLUSTER")
	raw, err := clientcmd.LoadFromFile(kubeconfig)
	Expect(err).NotTo(HaveOccurred())
	Expect(raw.CurrentContext).To(Equal("kind-"+cluster), "refusing to run against another cluster")
	restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	Expect(err).NotTo(HaveOccurred())
	// No request may hold up a spec until go test's timeout.
	restConfig.Timeout = 30 * time.Second
	kube, err = kubernetes.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred())
	// The script marks the cluster it created with this run's ID.
	ns, err := kube.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(ns.Annotations).To(HaveKeyWithValue(runAnnotation, mustEnv("E2E_RUN_ID")),
		"refusing to run on a cluster hack/test-e2e.sh didn't create")
	helmBin, kubectlBin = envOr("HELM", "helm"), envOr("KUBECTL", "kubectl")
	stubImage, operatorImage = mustEnv("STUB_IMG"), mustEnv("OPERATOR_IMG")
	operatorChart = mustEnv("MODE_OPERATOR_CHART")

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)
	SetDefaultConsistentlyDuration(10 * time.Second)
	SetDefaultConsistentlyPollingInterval(time.Second)
})

// runAnnotation on the default namespace holds the ID of the run that
// created the cluster.
const runAnnotation = "e2e.mode-manager-stub.bitasylum.org/run"

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	Expect(v).NotTo(BeEmpty(), "set %s", name)
	return v
}

// run runs a command and returns its output, failing the test if it fails.
func run(name string, args ...string) string {
	GinkgoHelper()
	out, err := tryRun(name, args...)
	Expect(err).NotTo(HaveOccurred(), "%s %s: %s", name, strings.Join(args, " "), out)
	return out
}

func tryRun(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = commandEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %s %s\n", name, strings.Join(args, " "))
	err := cmd.Run()
	return out.String(), err
}

// commandEnv is the environment for helm and kubectl, without the settings
// that could point them at another cluster than the kubeconfig's.
func commandEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HELM_KUBE") && !strings.HasPrefix(kv, "KUBECTL_KUBERC=") {
			env = append(env, kv)
		}
	}
	return append(env, "KUBECTL_KUBERC=false")
}

func kubectl(args ...string) {
	GinkgoHelper()
	run(kubectlBin, args...)
}

func helm(args ...string) {
	GinkgoHelper()
	run(helmBin, args...)
}

// imageParts splits an image into repository and tag.
func imageParts(image string) (repository, tag string) {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

// apiServerPeers is a values file that lets the API server's service proxy
// through the stub's NetworkPolicy: kind runs the API server on the
// control-plane node's network.
func apiServerPeers() string {
	GinkgoHelper()
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/control-plane",
	})
	Expect(err).NotTo(HaveOccurred())
	var b strings.Builder
	b.WriteString("networkPolicy:\n  from:\n")
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type != corev1.NodeInternalIP {
				continue
			}
			bits := 32
			if strings.Contains(a.Address, ":") {
				bits = 128
			}
			fmt.Fprintf(&b, "    - ipBlock:\n        cidr: %s/%d\n", a.Address, bits)
		}
	}
	Expect(b.String()).To(ContainSubstring("cidr:"), "no control-plane node address")
	f := filepath.Join(GinkgoT().TempDir(), "network-policy.yaml")
	Expect(os.WriteFile(f, []byte(b.String()), 0o600)).To(Succeed())
	return f
}

// stubPath is the stub's API through the API server's service proxy.
func stubPath(path string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services/http:%s:http/proxy/%s", stubNamespace, stubRelease, path)
}

// state returns the stub's state, or an error while it can't be reached.
func state() (manager.Snapshot, error) {
	var s manager.Snapshot
	data, err := kube.CoreV1().RESTClient().Get().AbsPath(stubPath("api/state")).DoRaw(ctx)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}

// mustState returns the stub's state, failing the test if it can't.
func mustState() manager.Snapshot {
	GinkgoHelper()
	s, err := state()
	Expect(err).NotTo(HaveOccurred())
	return s
}

// post sends a change to the stub and returns its result.
func post(path string, body any) web.Result {
	GinkgoHelper()
	data, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	raw, err := kube.CoreV1().RESTClient().Post().AbsPath(stubPath(path)).
		SetHeader("Content-Type", "application/json").Body(data).DoRaw(ctx)
	var res web.Result
	if jerr := json.Unmarshal(raw, &res); jerr != nil {
		Expect(err).NotTo(HaveOccurred())
		Expect(jerr).NotTo(HaveOccurred())
	}
	return res
}

func setMode(mode string, check bool) web.Result {
	GinkgoHelper()
	return post("api/mode", map[string]any{"mode": mode, "check": check})
}
