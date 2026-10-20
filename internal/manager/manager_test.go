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

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/bitasylum/mode-operator/client/clustermode"
	"github.com/bitasylum/mode-operator/client/fingerprint"
)

// crdDiscovery serves the ClusterMode CRDs while installed is true.
type crdDiscovery struct {
	*discoveryfake.FakeDiscovery
	installed atomic.Bool
}

func (d *crdDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	if gv == clustermode.Group+"/"+clustermode.Version && d.installed.Load() {
		return &metav1.APIResourceList{GroupVersion: gv, APIResources: []metav1.APIResource{
			{Name: "clustermodes"}, {Name: "clustermodedefinitions"},
		}}, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: clustermode.Group}, gv)
}

// fakeCluster is a cluster with two nodes and the definition "standard".
// Server-side apply of a ClusterMode creates it or sets the fields sent.
type fakeCluster struct {
	t         *testing.T
	dynamic   *dynamicfake.FakeDynamicClient
	discovery *crdDiscovery
	kube      *kubefake.Clientset
	// failApplies is how many ClusterMode applies fail before one succeeds,
	// and applies counts those that succeeded.
	failApplies, applies atomic.Int32
	// gone makes lists and watches of ClusterModes fail as they do once the
	// CRDs are removed.
	gone atomic.Bool

	mu      sync.Mutex
	watches []*watch.RaceFreeFakeWatcher
}

func newFakeCluster(t *testing.T, installed bool) *fakeCluster {
	t.Helper()
	def := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": clustermode.Group + "/" + clustermode.Version,
		"kind":       "ClusterModeDefinition",
		"metadata":   map[string]any{"name": "standard"},
		"spec": map[string]any{
			"modes":       []any{"normal", "peak", "standby"},
			"defaultMode": "standby",
		},
	}}
	lists := map[schema.GroupVersionResource]string{
		clustermode.ClusterModeResource: "ClusterModeList",
		clustermode.DefinitionResource:  "ClusterModeDefinitionList",
	}
	c := &fakeCluster{
		t:         t,
		dynamic:   dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, def),
		discovery: &crdDiscovery{FakeDiscovery: &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}},
		kube:      kubefake.NewClientset(node("node-a", "boot-a"), node("node-b", "boot-b")),
	}
	c.discovery.installed.Store(installed)
	c.dynamic.PrependReactor("patch", "clustermodes", c.apply)
	c.dynamic.PrependReactor("list", "clustermodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if c.gone.Load() {
			return true, nil, apierrors.NewNotFound(clustermode.ClusterModeResource.GroupResource(), "")
		}
		return false, nil, nil
	})
	c.dynamic.PrependWatchReactor("clustermodes", func(a clienttesting.Action) (bool, watch.Interface, error) {
		// Under the lock, so removeCRDs can't miss a watch opened meanwhile.
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.gone.Load() {
			return true, nil, apierrors.NewNotFound(clustermode.ClusterModeResource.GroupResource(), "")
		}
		w, err := c.dynamic.Tracker().Watch(clustermode.ClusterModeResource, "")
		if err != nil {
			return true, nil, err
		}
		if fw, ok := w.(*watch.RaceFreeFakeWatcher); ok {
			c.watches = append(c.watches, fw)
		}
		return true, w, nil
	})
	return c
}

// restrict gives the definition transitions, standby → normal → peak, so a
// mode must settle before it's left; on the fake cluster none ever does.
func (c *fakeCluster) restrict() {
	c.t.Helper()
	res := c.dynamic.Resource(clustermode.DefinitionResource)
	def, err := res.Get(context.Background(), "standard", metav1.GetOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	transitions := []any{
		map[string]any{"from": "standby", "to": []any{"normal"}},
		map[string]any{"from": "normal", "to": []any{"peak"}},
		map[string]any{"from": "peak", "to": []any{"normal"}},
	}
	if err := unstructured.SetNestedSlice(def.Object, transitions, "spec", "transitions"); err != nil {
		c.t.Fatal(err)
	}
	if _, err := res.Update(context.Background(), def, metav1.UpdateOptions{}); err != nil {
		c.t.Fatal(err)
	}
}

func node(name, bootID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: bootID}},
	}
}

func (c *fakeCluster) apply(a clienttesting.Action) (bool, runtime.Object, error) {
	p, ok := a.(clienttesting.PatchAction)
	if !ok || p.GetPatchType() != types.ApplyPatchType {
		return false, nil, nil
	}
	if c.failApplies.Load() > 0 {
		c.failApplies.Add(-1)
		return true, nil, apierrors.NewInternalError(errors.New("injected"))
	}
	applied := &unstructured.Unstructured{}
	if err := json.Unmarshal(p.GetPatch(), &applied.Object); err != nil {
		return true, nil, err
	}
	tracker, gvr := c.dynamic.Tracker(), clustermode.ClusterModeResource
	existing, err := tracker.Get(gvr, "", p.GetName())
	switch {
	case apierrors.IsNotFound(err):
		err = tracker.Create(gvr, applied, "")
	case err == nil:
		u := existing.(*unstructured.Unstructured).DeepCopy()
		spec, _, _ := unstructured.NestedMap(applied.Object, "spec")
		for k, v := range spec {
			if err := unstructured.SetNestedField(u.Object, v, "spec", k); err != nil {
				return true, nil, err
			}
		}
		u.SetLabels(applied.GetLabels())
		err = tracker.Update(gvr, u, "")
	}
	if err != nil {
		return true, nil, err
	}
	c.applies.Add(1)
	obj, err := tracker.Get(gvr, "", p.GetName())
	return true, obj, err
}

// removeCRDs makes the CRDs disappear: discovery stops serving them, lists
// and watches fail, and the open watches end.
func (c *fakeCluster) removeCRDs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.discovery.installed.Store(false)
	c.gone.Store(true)
	for _, w := range c.watches {
		w.Stop()
	}
	c.watches = nil
}

func (c *fakeCluster) installCRDs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gone.Store(false)
	c.discovery.installed.Store(true)
}

// storedMode is the ClusterMode's spec.mode, or "" if it doesn't exist.
func (c *fakeCluster) storedMode() string {
	u, err := c.dynamic.Resource(clustermode.ClusterModeResource).Get(context.Background(), "default",
		metav1.GetOptions{})
	if err != nil {
		return ""
	}
	mode, _, _ := unstructured.NestedString(u.Object, "spec", "mode")
	return mode
}

func (c *fakeCluster) manager(cfg Config) *Manager {
	cfg.ClusterMode, cfg.Definition, cfg.WriterLabel = "default", "standard", "test"
	cfg.BuiltinDefault, cfg.Namespace, cfg.FingerprintConfigMap = "standby", "stub", "fingerprint"
	cfg.DiscoveryInterval, cfg.Debounce = 20*time.Millisecond, 10*time.Millisecond
	reader := clustermode.NewReader(c.dynamic, c.discovery, "default")
	return New(c.t.Context(), cfg, reader, c.kube, record.NewFakeRecorder(100))
}

// start runs m until the test ends, and fails the test if Run returns an
// error other than the test ending.
func start(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (m *Manager) phase() Phase { return m.State().Phase }

func TestRunningReturnsWhenTheCRDsAreRemoved(t *testing.T) {
	c := newFakeCluster(t, false)
	c.gone.Store(true)
	m := c.manager(Config{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := m.running(ctx, fingerprint.Record{ClusterMode: "default"})
	if ctx.Err() != nil {
		t.Fatalf("running returned only when the test timed out: %v", err)
	}
	if clustermode.ReasonOf(err) != clustermode.ReasonNotInstalled {
		t.Errorf("running = %v, want NotInstalled", err)
	}
	if m.State().ClusterMode != nil || m.keeper != nil {
		t.Errorf("running left the ClusterMode or the keeper behind")
	}
}

// The mode set while the CRDs are missing is the one written once they're
// installed, after a removal as well as on first start.
func TestTheCRDsComeAndGo(t *testing.T) {
	c := newFakeCluster(t, false)
	m := c.manager(Config{})
	start(t, m)
	waitFor(t, "NoCRDs", func() bool { return m.phase() == PhaseNoCRDs })
	if err := m.SetMode(t.Context(), "peak", true); err != nil {
		t.Fatalf("SetMode without the CRDs: %v", err)
	}

	c.installCRDs()
	waitFor(t, "Running", func() bool { return m.phase() == PhaseRunning })
	if got := c.storedMode(); got != "peak" {
		t.Errorf("written mode = %q, want the mode held in memory, peak", got)
	}

	c.removeCRDs()
	waitFor(t, "NoCRDs after the removal", func() bool { return m.phase() == PhaseNoCRDs })
	if m.State().ClusterMode != nil {
		t.Errorf("the removed ClusterMode is still shown")
	}
	if err := m.SetMode(t.Context(), "normal", false); err != nil {
		t.Fatalf("SetMode after the removal: %v", err)
	}
	if err := c.dynamic.Resource(clustermode.ClusterModeResource).Delete(t.Context(), "default",
		metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	c.installCRDs()
	waitFor(t, "Running again", func() bool { return m.phase() == PhaseRunning })
	waitFor(t, "normal written", func() bool { return c.storedMode() == "normal" })
}

func TestRecreatesADeletedClusterMode(t *testing.T) {
	c := newFakeCluster(t, true)
	m := c.manager(Config{})
	start(t, m)
	waitFor(t, "Running", func() bool { return m.phase() == PhaseRunning && m.State().ClusterMode != nil })
	if err := m.SetMode(t.Context(), "normal", true); err != nil {
		t.Fatal(err)
	}

	// The first attempt fails, so the ClusterMode is back only after a
	// retry.
	c.failApplies.Store(1)
	if err := c.dynamic.Resource(clustermode.ClusterModeResource).Delete(t.Context(), "default",
		metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed attempt shown", func() bool { return m.State().LastError != "" })
	waitFor(t, "the ClusterMode recreated in normal", func() bool { return c.storedMode() == "normal" })
	waitFor(t, "the error cleared", func() bool { return m.State().LastError == "" })
}

// When the mode is written once the CRDs appear but the fingerprint isn't,
// the mode isn't written again, which would undo changes made meanwhile;
// the keeper saves the fingerprint.
func TestPersistWithAFailedFingerprint(t *testing.T) {
	c := newFakeCluster(t, false)
	saves := atomic.Int32{}
	c.kube.PrependReactor("patch", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		if saves.Add(1) == 1 {
			return true, nil, apierrors.NewInternalError(errors.New("injected"))
		}
		return false, nil, nil
	})
	m := c.manager(Config{})
	start(t, m)
	waitFor(t, "NoCRDs", func() bool { return m.phase() == PhaseNoCRDs })
	c.installCRDs()
	waitFor(t, "Running", func() bool { return m.phase() == PhaseRunning })
	waitFor(t, "the fingerprint saved", func() bool {
		cm, err := c.kube.CoreV1().ConfigMaps("stub").Get(context.Background(), "fingerprint", metav1.GetOptions{})
		return err == nil && cm.Data["bootIDs"] != ""
	})
	if n := c.applies.Load(); n != 1 {
		t.Errorf("the ClusterMode was applied %d times, want once", n)
	}
}

// A move waits for the current mode to settle, which on the fake cluster it
// never does, until it's stopped or a step times out.
func TestStoppingAMove(t *testing.T) {
	for _, c := range []struct {
		name    string
		timeout time.Duration
		stop    bool
	}{
		{name: "stopped", stop: true},
		{name: "timed out", timeout: 100 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeCluster(t, true)
			fake.restrict()
			m := fake.manager(Config{MoveStepTimeout: c.timeout})
			start(t, m)
			waitFor(t, "Running", func() bool { return m.phase() == PhaseRunning && m.State().ClusterMode != nil })
			if err := m.MoveTo(t.Context(), "peak"); err != nil {
				t.Fatal(err)
			}
			if c.stop {
				waitFor(t, "the move waiting", func() bool {
					s := m.State()
					return s.Move != nil && len(s.Move.Steps) > 0
				})
				if !m.StopMove() {
					t.Fatal("StopMove found no move")
				}
			}
			waitFor(t, "the move done", func() bool { s := m.State(); return s.Move.Done })
			if s := m.State(); s.Move.Error == "" || s.Mode != "standby" {
				t.Errorf("after the move, state = %+v, move %+v; want an error, still in standby", s, s.Move)
			}
			if m.StopMove() {
				t.Errorf("StopMove found a move after it ended")
			}
		})
	}
}

func TestSetModeChecks(t *testing.T) {
	c := newFakeCluster(t, false)
	m := c.manager(Config{KnownModes: []string{"standby", "peak"}})
	if err := m.SetMode(t.Context(), "peak", true); !errors.Is(err, ErrNotReady) {
		t.Errorf("SetMode while starting = %v, want ErrNotReady", err)
	}
	if err := m.MoveTo(t.Context(), "peak"); !errors.Is(err, ErrNotReady) {
		t.Errorf("MoveTo while starting = %v, want ErrNotReady", err)
	}

	start(t, m)
	waitFor(t, "NoCRDs", func() bool { return m.phase() == PhaseNoCRDs })
	for _, mode := range []string{"", "Peak", "normal"} {
		if err := m.SetMode(t.Context(), mode, false); !errors.Is(err, ErrInvalidMode) {
			t.Errorf("SetMode(%q) without the CRDs = %v, want ErrInvalidMode", mode, err)
		}
	}
	if m.State().Mode != "standby" || m.State().LastError == "" {
		t.Errorf("after refused modes, state = %+v; want standby, with the error shown", m.State())
	}
	if err := m.MoveTo(t.Context(), "peak"); !errors.Is(err, ErrNotReady) {
		t.Errorf("MoveTo without the CRDs = %v, want ErrNotReady", err)
	}
}

func TestOneMoveAtATime(t *testing.T) {
	c := newFakeCluster(t, true)
	m := c.manager(Config{})
	m.update(func(s *State) {
		s.Phase = PhaseRunning
		s.Move = &MoveInfo{Target: "peak", Steps: []StepInfo{}}
	})
	if err := m.MoveTo(t.Context(), "normal"); !errors.Is(err, ErrBusy) {
		t.Errorf("MoveTo during a move = %v, want ErrBusy", err)
	}
	if err := m.SetMode(t.Context(), "normal", true); !errors.Is(err, ErrBusy) {
		t.Errorf("SetMode during a move = %v, want ErrBusy", err)
	}
}

// The page needs lists, not nulls, before the keeper's first check and for
// a move that takes no steps.
func TestSnapshotHasNoNulls(t *testing.T) {
	c := newFakeCluster(t, true)
	m := c.manager(Config{})
	start(t, m)
	waitFor(t, "Running", func() bool { return m.phase() == PhaseRunning && m.State().ClusterMode != nil })
	// A move to the current mode takes no steps.
	if err := m.MoveTo(t.Context(), "standby"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the move done", func() bool { return m.State().Move.Done })
	m.mu.Lock()
	m.keeper = fingerprint.NewKeeper(c.kube, fingerprint.KeeperOptions{Saver: m.store})
	m.mu.Unlock()
	data, err := json.Marshal(m.Snapshot(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"modes", "next", "fingerprint"} {
		if got[key] == nil {
			t.Errorf("%s is null: %s", key, data)
		}
	}
	if move, _ := got["move"].(map[string]any); move["steps"] == nil {
		t.Errorf("move.steps is null: %s", data)
	}
}
