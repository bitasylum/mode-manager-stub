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

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/bitasylum/mode-operator/client/clustermode"
	"github.com/bitasylum/mode-operator/client/fingerprint"

	"github.com/bitasylum/mode-manager-stub/internal/manager"
)

// definitionValues installs the README's example definition with the chart:
// standby is the default, and maintenance has no rule.
const definitionValues = `definitions:
  - name: standard
    modes: [normal, peak, offpeak, maintenance, standby]
    defaultMode: standby
    transitions:
      - from: normal
        to: [peak, offpeak]
      - from: peak
        to: [normal]
      - from: offpeak
        to: [normal]
      - from: standby
        to: [normal, maintenance]
`

// workload follows the ClusterMode: two replicas in normal, none otherwise.
const workload = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  labels:
    mode.bitasylum.org/clustermode: default
  annotations:
    mode.bitasylum.org/replicas: "normal=2,*=0"
spec:
  replicas: 0
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a
`

// agnhost's connect command tries a TCP connection, and prints TIMEOUT when
// nothing answers.
const agnhostImage = "registry.k8s.io/e2e-test-images/agnhost:2.53" +
	"@sha256:99c6b4bb4a1e1df3f0b3752168c89358794d02258ebebc26bf21c29399011a85"

// reader reads the ClusterMode as the cluster's administrator.
func reader() *clustermode.Reader {
	GinkgoHelper()
	r, err := clustermode.NewReaderForConfig(restConfig, clusterModeName)
	Expect(err).NotTo(HaveOccurred())
	return r
}

func clusterMode() (*clustermode.ClusterMode, error) { return reader().Get(ctx) }

// readyReason is the ClusterMode's Ready condition, as the stub reports it.
func readyReason() string {
	s, err := state()
	if err != nil || s.ClusterMode == nil {
		return ""
	}
	return s.ClusterMode.Ready
}

func settledIn(mode string) func(Gomega) {
	return func(g Gomega) {
		s, err := state()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(s.ClusterMode).NotTo(BeNil())
		g.Expect(s.ClusterMode.Mode).To(Equal(mode))
		g.Expect(s.ClusterMode.Settled).To(BeTrue())
	}
}

func webReplicas() int32 {
	d, err := kube.AppsV1().Deployments(workloadNamespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil || d.Spec.Replicas == nil {
		return -1
	}
	return *d.Spec.Replicas
}

func fingerprintData() map[string]string {
	cm, err := kube.CoreV1().ConfigMaps(stubNamespace).Get(ctx, fingerprint.DefaultName(clusterModeName),
		metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return cm.Data
}

// liveBootIDs are the nodes' boot IDs, as the fingerprint stores them.
func liveBootIDs() string {
	GinkgoHelper()
	ids, err := fingerprint.Take(ctx, kube)
	Expect(err).NotTo(HaveOccurred())
	return ids.String()
}

// eventSince reports whether the stub recorded an event with reason on the
// ClusterMode since t.
func eventSince(reason string, t time.Time) func() bool {
	return func() bool {
		list, err := kube.CoreV1().Events("default").List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, e := range list.Items {
			if e.InvolvedObject.Name == clusterModeName && e.Reason == reason &&
				e.Source.Component == "mode-manager-stub" && !e.LastTimestamp.Time.Before(t.Truncate(time.Second)) {
				return true
			}
		}
		return false
	}
}

// restartStub stops the stub, runs between, and starts it again.
func restartStub(between func()) {
	GinkgoHelper()
	kubectl("-n", stubNamespace, "scale", "deploy/"+stubRelease, "--replicas=0")
	kubectl("-n", stubNamespace, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/name=mode-manager-stub",
		"--timeout=2m")
	between()
	kubectl("-n", stubNamespace, "scale", "deploy/"+stubRelease, "--replicas=1")
	kubectl("-n", stubNamespace, "rollout", "status", "deploy/"+stubRelease, "--timeout=2m")
}

// polledState is the stub's state inside a polled function, where an
// unreachable stub is retried rather than failing the spec.
func polledState(g Gomega) manager.Snapshot {
	s, err := state()
	g.Expect(err).NotTo(HaveOccurred())
	return s
}

func phase(g Gomega) manager.Phase { return polledState(g).Phase }

func startupCase() string {
	s, err := state()
	if err != nil || s.Startup == nil {
		return ""
	}
	return s.Startup.Case
}

var _ = Describe("mode-manager-stub with mode-operator", Ordered, func() {
	It("holds the mode in memory before the CRDs exist", func() {
		kubectl("create", "namespace", stubNamespace)
		kubectl("label", "namespace", stubNamespace, "pod-security.kubernetes.io/enforce=restricted")
		repo, tag := imageParts(stubImage)
		helm("install", stubRelease, filepath.Join("..", "..", "chart"), "-n", stubNamespace,
			"--set", "image.repository="+repo, "--set", "image.tag="+tag, "--set", "image.pullPolicy=Never",
			"--set", "discoveryInterval=3s", "--set", "debounce=1s", "-f", apiServerPeers(),
			"--wait", "--timeout", "3m")

		Eventually(func(g Gomega) {
			s, err := state()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(s.Phase).To(Equal(manager.PhaseNoCRDs))
			g.Expect(s.Mode).To(Equal("standby"))
			g.Expect(s.Startup).NotTo(BeNil())
			g.Expect(s.Startup.Case).To(Equal("NotInstalled"))
		}).Should(Succeed())
	})

	It("keeps other pods away from the page and API", func() {
		out, err := tryRun(kubectlBin, "-n", "default", "run", "netpol-probe", "--rm", "-i", "--quiet",
			"--restart=Never", "--pod-running-timeout=2m", "--image="+agnhostImage, "--",
			"connect", "--timeout=5s", fmt.Sprintf("%s.%s.svc:8080", stubRelease, stubNamespace))
		Expect(err).To(HaveOccurred(), "a pod reached the stub: %s", out)
		Expect(out).To(ContainSubstring("TIMEOUT"))
		_, err = state()
		Expect(err).NotTo(HaveOccurred(), "the API server's service proxy reaches it")
	})

	It("writes the mode once mode-operator is installed", func() {
		kubectl("create", "namespace", operatorNamespace)
		kubectl("label", "namespace", operatorNamespace, "pod-security.kubernetes.io/enforce=restricted")
		repo, tag := imageParts(operatorImage)
		helm("install", operatorRelease, operatorChart, "-n", operatorNamespace,
			"--set", "image.repository="+repo, "--set", "image.tag="+tag, "--set", "image.pullPolicy=Never",
			"--wait", "--timeout", "3m")
		values := filepath.Join(GinkgoT().TempDir(), "definitions.yaml")
		Expect(os.WriteFile(values, []byte(definitionValues), 0o600)).To(Succeed())
		helm("upgrade", operatorRelease, operatorChart, "-n", operatorNamespace, "--reuse-values",
			"-f", values, "--wait", "--timeout", "3m")

		Eventually(func(g Gomega) {
			cm, err := clusterMode()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cm.Mode).To(Equal("standby"))
			g.Expect(cm.Definition).To(Equal("standard"))
			g.Expect(cm.Labels).To(HaveKeyWithValue(clustermode.WriterLabel, "mode-manager-stub"))
			g.Expect(cm.ModeOwner).To(Equal(clustermode.ModeManagerFieldManager))
		}).Should(Succeed())
		Eventually(phase).Should(Equal(manager.PhaseRunning))
		Eventually(func() string { return fingerprintData()["bootIDs"] }).Should(Equal(liveBootIDs()))
		Expect(strings.Count(liveBootIDs(), "\n")).To(Equal(2), "the cluster has two nodes")
	})

	It("creates a missing ClusterMode in the default mode when it starts", func() {
		start := time.Now()
		restartStub(func() {
			kubectl("delete", "clustermode", clusterModeName)
			kubectl("-n", stubNamespace, "delete", "configmap", fingerprint.DefaultName(clusterModeName))
		})
		Eventually(startupCase).Should(Equal("ClusterModeNotFound"))
		Expect(mustState().Startup.Mode).To(Equal("standby"))
		Eventually(eventSince("ModeReset", start)).Should(BeTrue())
		Eventually(settledIn("standby")).Should(Succeed())
	})

	It("changes the mode when asked, and the workload follows", func() {
		kubectl("create", "namespace", workloadNamespace)
		applyYAML(workload)
		// Settled with the workload counted, so the change isn't checked
		// against a status from before the operator saw it.
		Eventually(func(g Gomega) {
			s := polledState(g)
			g.Expect(s.ClusterMode).NotTo(BeNil())
			g.Expect(s.ClusterMode.Mode).To(Equal("standby"))
			g.Expect(s.ClusterMode.Settled).To(BeTrue())
			g.Expect(s.ClusterMode.Workloads.Total).To(Equal(int32(1)))
		}).Should(Succeed())

		Expect(setMode("normal", true)).To(okResult())
		Eventually(webReplicas).Should(Equal(int32(2)))
		Eventually(settledIn("normal")).Should(Succeed())
		Expect(readyReason()).To(Equal("True/AllSettled"))
		Expect(mustState().Next).To(ConsistOf("peak", "offpeak", "standby"))
	})

	It("refuses changes the transitions don't allow, and moves in steps", func() {
		res := setMode("maintenance", true)
		Expect(res.OK).To(BeFalse())
		Expect(res.Reason).To(Equal(string(clustermode.ReasonTransitionNotAllowed)))
		// Written without the check, the API server rejects it, and the
		// library says why.
		res = setMode("maintenance", false)
		Expect(res.OK).To(BeFalse())
		Expect(res.Reason).To(Equal(string(clustermode.ReasonTransitionNotAllowed)))

		Expect(post("api/move", map[string]any{"target": "maintenance"})).To(okResult())
		Eventually(func(g Gomega) {
			s := polledState(g)
			g.Expect(s.Move).NotTo(BeNil())
			g.Expect(s.Move.Done).To(BeTrue())
			g.Expect(s.Move.Error).To(BeEmpty())
		}).Should(Succeed())
		steps := make([]string, 0, len(mustState().Move.Steps))
		for _, st := range mustState().Move.Steps {
			steps = append(steps, st.From+">"+st.To+":"+st.Phase)
		}
		Expect(steps).To(Equal([]string{
			"normal>standby:Written", "standby>maintenance:Waiting", "standby>maintenance:Written",
		}))
		Eventually(settledIn("maintenance")).Should(Succeed())
		Expect(webReplicas()).To(Equal(int32(0)))
	})

	It("adopts a maintainer's change without writing it back", func() {
		kubectl("patch", "clustermode", clusterModeName, "--type=merge", "-p", `{"spec":{"mode":"standby"}}`)
		Eventually(func(g Gomega) {
			s := polledState(g)
			g.Expect(s.Mode).To(Equal("standby"))
			g.Expect(s.LastAdoption).NotTo(BeNil())
			g.Expect(s.LastAdoption.By).To(Equal("kubectl-patch"))
		}).Should(Succeed())
		cm, err := clusterMode()
		Expect(err).NotTo(HaveOccurred())
		Consistently(func(g Gomega) {
			now, err := clusterMode()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(now.Generation).To(Equal(cm.Generation))
			g.Expect(now.ModeOwner).To(Equal("kubectl-patch"))
		}).Should(Succeed())
	})

	It("adopts a change by another writer, such as an orchestration server", func() {
		// Only what another writer needs: no create, and no definitions.
		kubectl("-n", workloadNamespace, "create", "serviceaccount", "orchestrator")
		kubectl("create", "clusterrole", "orchestrator", "--verb=get,list,watch,patch",
			"--resource=clustermodes."+clustermode.Group, "--resource-name="+clusterModeName)
		kubectl("create", "clusterrolebinding", "orchestrator", "--clusterrole=orchestrator",
			"--serviceaccount="+workloadNamespace+":orchestrator")
		cfg := rest.CopyConfig(restConfig)
		cfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + workloadNamespace + ":orchestrator"}
		r, err := clustermode.NewReaderForConfig(cfg, clusterModeName)
		Expect(err).NotTo(HaveOccurred())
		orchestrator := clustermode.NewWriter(r, "orchestrator")

		settled, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		_, err = r.WaitSettled(settled, "standby")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error { _, err := orchestrator.Set(ctx, "normal"); return err }).Should(Succeed())
		Eventually(func(g Gomega) {
			s := polledState(g)
			g.Expect(s.Mode).To(Equal("normal"))
			g.Expect(s.LastAdoption).NotTo(BeNil())
			g.Expect(s.LastAdoption.By).To(Equal("orchestrator"))
		}).Should(Succeed())
		Eventually(webReplicas).Should(Equal(int32(2)))
	})

	It("writes the mode again when the ClusterMode is deleted", func() {
		kubectl("delete", "clustermode", clusterModeName)
		Eventually(func(g Gomega) {
			cm, err := clusterMode()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cm.Mode).To(Equal("normal"))
			g.Expect(cm.ModeOwner).To(Equal(clustermode.ModeManagerFieldManager))
		}).Should(Succeed())
		Eventually(settledIn("normal")).Should(Succeed())
		Expect(webReplicas()).To(Equal(int32(2)))
	})

	It("holds a mode set while the CRDs are removed, and writes it once they're back", func() {
		kubectl("delete", "crd", "clustermodes."+clustermode.Group, "--timeout=2m")
		Eventually(phase).Should(Equal(manager.PhaseNoCRDs))
		Expect(setMode("standby", false)).To(okResult())
		Expect(mustState().Mode).To(Equal("standby"))

		// The upgrade puts the deleted CRD back.
		helm("upgrade", operatorRelease, operatorChart, "-n", operatorNamespace, "--reuse-values",
			"--wait", "--timeout", "3m")
		Eventually(phase).Should(Equal(manager.PhaseRunning))
		Eventually(func(g Gomega) {
			cm, err := clusterMode()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cm.Mode).To(Equal("standby"))
		}).Should(Succeed())
		Eventually(webReplicas).Should(Equal(int32(0)))

		// Back to normal for the restart that follows.
		Eventually(func(g Gomega) { g.Expect(setMode("normal", true)).To(okResult()) }).Should(Succeed())
		Eventually(settledIn("normal")).Should(Succeed())
		Eventually(webReplicas).Should(Equal(int32(2)))
	})

	It("keeps the mode when it restarts and the nodes stayed up", func() {
		Eventually(settledIn("normal")).Should(Succeed())
		start := time.Now()
		kubectl("-n", stubNamespace, "rollout", "restart", "deploy/"+stubRelease)
		kubectl("-n", stubNamespace, "rollout", "status", "deploy/"+stubRelease, "--timeout=2m")
		Eventually(startupCase).Should(Equal("Overlap"))
		s := mustState()
		Expect(s.Startup.Restored).To(BeTrue())
		Expect(s.Mode).To(Equal("normal"))
		Eventually(eventSince("ModeRestored", start)).Should(BeTrue())
		Expect(fingerprintData()["restoredOnStartup"]).To(Equal("true"))
		Expect(webReplicas()).To(Equal(int32(2)))
	})

	// kind nodes report the host's boot ID, which a container restart
	// doesn't change, so the test stands in for a full reboot by storing
	// boot IDs no node has.
	It("resets to the default mode after a full cluster reboot", func() {
		start := time.Now()
		restartStub(func() {
			patch := fmt.Sprintf(`{"data":{"bootIDs":%q}}`, "node-a=not-booted-1\nnode-b=not-booted-2\n")
			kubectl("-n", stubNamespace, "patch", "configmap", fingerprint.DefaultName(clusterModeName),
				"--type=merge", "-p", patch)
		})
		Eventually(startupCase).Should(Equal("FullReboot"))
		s := mustState()
		Expect(s.Startup.Restored).To(BeFalse())
		Expect(s.Mode).To(Equal("standby"))
		Eventually(eventSince("ModeReset", start)).Should(BeTrue())
		Expect(fingerprintData()["restoredOnStartup"]).To(Equal("false"))
		Expect(fingerprintData()["bootIDs"]).To(Equal(liveBootIDs()))
		cm, err := clusterMode()
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Mode).To(Equal("standby"))
		Eventually(webReplicas).Should(Equal(int32(0)))
	})
})

func okResult() types.GomegaMatcher { return HaveField("OK", BeTrue()) }

func applyYAML(manifest string) {
	GinkgoHelper()
	f := filepath.Join(GinkgoT().TempDir(), "manifest.yaml")
	Expect(os.WriteFile(f, []byte(manifest), 0o600)).To(Succeed())
	kubectl("apply", "-f", f)
}
