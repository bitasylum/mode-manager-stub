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

// Package manager runs the stub mode manager: it decides the mode on
// startup, keeps the node fingerprint current, adopts changes others make to
// the ClusterMode, and changes the mode when asked.
package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/bitasylum/mode-operator/client/clustermode"
	"github.com/bitasylum/mode-operator/client/fingerprint"
	"github.com/bitasylum/mode-operator/client/startup"
)

// Config configures a Manager.
type Config struct {
	// ClusterMode and Definition name the ClusterMode the stub writes and the
	// ClusterModeDefinition it follows.
	ClusterMode string
	Definition  string
	// WriterLabel is the value of the mode.bitasylum.org/writer label.
	WriterLabel string
	// BuiltinDefault is the mode without the CRDs or the definition.
	BuiltinDefault string
	// KnownModes, if set, are the only modes the stub runs in.
	KnownModes []string
	// Namespace and FingerprintConfigMap locate the fingerprint.
	Namespace            string
	FingerprintConfigMap string
	// DiscoveryInterval is how often the stub checks whether the CRDs are
	// installed while they aren't.
	DiscoveryInterval time.Duration
	// Debounce is how long the fingerprint waits after a node change.
	Debounce time.Duration
	// MoveStepTimeout limits how long a move waits for each mode to settle.
	// Zero means no limit.
	MoveStepTimeout time.Duration
}

// Phase is what the Manager is doing.
type Phase string

// Phases.
const (
	// PhaseStarting: the startup decision hasn't been made yet.
	PhaseStarting Phase = "Starting"
	// PhaseNoCRDs: the CRDs aren't installed; the mode is held in memory.
	PhaseNoCRDs Phase = "NoCRDs"
	// PhaseRunning: the ClusterMode is written and watched.
	PhaseRunning Phase = "Running"
)

// Errors that SetMode and MoveTo return before trying a change.
var (
	// ErrBusy: a move is running.
	ErrBusy = errors.New("a move is running")
	// ErrNotReady: the Manager is starting, or, for a move, the CRDs aren't
	// installed.
	ErrNotReady = errors.New("not ready")
	// ErrInvalidMode: the mode can't be held while the CRDs are missing.
	ErrInvalidMode = errors.New("invalid mode")
)

// Manager is the stub mode manager.
type Manager struct {
	cfg      Config
	reader   *clustermode.Reader
	writer   *clustermode.Writer
	kube     kubernetes.Interface
	store    *fingerprint.Store
	recorder record.EventRecorder
	started  time.Time

	// changeMu serializes changes of the mode and the phase: SetMode, the
	// switch from NoCRDs to Running, and recreating a deleted ClusterMode.
	// So a mode set while the CRDs are missing is the one written once they
	// appear. It is taken before mu.
	changeMu sync.Mutex

	mu    sync.Mutex
	state State
	// keeper is the running fingerprint Keeper, while Running.
	keeper *fingerprint.Keeper
	// stopMove cancels the running move.
	stopMove context.CancelFunc
}

// New returns a Manager.
func New(
	ctx context.Context, cfg Config, reader *clustermode.Reader, kube kubernetes.Interface, recorder record.EventRecorder,
) *Manager {
	m := &Manager{
		cfg:      cfg,
		reader:   reader,
		writer:   clustermode.NewModeManagerWriter(reader, cfg.Definition, cfg.WriterLabel),
		kube:     kube,
		store:    fingerprint.NewStore(kube, cfg.Namespace, cfg.FingerprintConfigMap),
		recorder: recorder,
		state:    State{Phase: PhaseStarting},
	}
	// The stub's stand-in for broadcasting: every change of the current
	// mode, its own once written and others' once adopted, in order.
	log := klog.FromContext(ctx)
	m.writer.OnChange(func(c clustermode.Change) {
		log.Info("The current mode changed", "mode", c.Mode, "by", c.By, "adopted", c.Adopted)
		m.update(func(s *State) {
			s.Mode = c.Mode
			if c.Adopted {
				s.LastAdoption = &Adoption{Mode: c.Mode, By: c.By, At: time.Now()}
			}
		})
	})
	return m
}

func (m *Manager) deps() startup.Deps {
	return startup.Deps{
		Writer:       m.writer,
		Fingerprints: m.store,
		Nodes:        func(ctx context.Context) (fingerprint.BootIDs, error) { return fingerprint.Take(ctx, m.kube) },
		Recorder:     m.recorder,
	}
}

// Run runs the Manager until ctx is done.
func (m *Manager) Run(ctx context.Context) error {
	log := klog.FromContext(ctx)
	m.started = time.Now()

	var res startup.Result
	err := m.retry(ctx, func() error {
		var err error
		res, err = startup.Run(ctx, m.deps(), startup.Options{
			ClusterMode: m.cfg.ClusterMode, BuiltinDefault: m.cfg.BuiltinDefault, KnownModes: m.cfg.KnownModes,
		})
		return err
	})
	if err != nil {
		return err
	}
	log.Info("Decided the startup mode", "case", res.Decision.Case, "mode", res.Decision.Mode,
		"restored", res.Decision.Restored)
	saved := res.Fingerprint
	m.update(func(s *State) {
		s.Mode = res.Decision.Mode
		s.Phase = PhaseRunning
		if saved == nil {
			s.Phase = PhaseNoCRDs
		}
		s.Startup = &StartupInfo{
			Case: string(res.Decision.Case), Mode: res.Decision.Mode, Restored: res.Decision.Restored,
			Overlap: res.Decision.Overlap, At: time.Now(),
		}
		if res.Decision.Event != nil {
			s.Startup.Event = res.Decision.Event.Reason
			s.Startup.Message = res.Decision.Event.Message
		}
	})

	for {
		if saved == nil {
			// The CRDs aren't installed: run in the current mode, and write
			// it once they are.
			if saved, err = m.waitForCRDs(ctx); err != nil {
				return err
			}
		}
		err := m.running(ctx, *saved)
		if clustermode.ReasonOf(err) != clustermode.ReasonNotInstalled {
			return err
		}
		log.Info("The ClusterMode CRDs were removed; holding the mode in memory")
		m.changeMu.Lock()
		m.update(func(s *State) { s.Phase = PhaseNoCRDs })
		m.changeMu.Unlock()
		saved = nil
	}
}

// waitForCRDs holds the mode in memory until the CRDs are installed, then
// writes it and the fingerprint, and switches to Running.
func (m *Manager) waitForCRDs(ctx context.Context) (*fingerprint.Record, error) {
	log := klog.FromContext(ctx)
	ticker := time.NewTicker(m.cfg.DiscoveryInterval)
	defer ticker.Stop()
	for {
		ok, err := m.reader.Available(ctx)
		if err != nil {
			log.Error(err, "Could not check whether the ClusterMode CRDs are installed")
		}
		if ok {
			var saved *fingerprint.Record
			err := m.retry(ctx, func() error {
				// Each attempt writes the mode held now, which SetMode may
				// have changed since the last one.
				m.changeMu.Lock()
				defer m.changeMu.Unlock()
				mode := m.State().Mode
				res, err := startup.Persist(ctx, m.deps(), mode, m.started)
				switch {
				case res.ClusterMode == nil:
					return err
				case err != nil:
					// The mode is written. Writing it again would undo any
					// change made meanwhile, so leave the fingerprint to the
					// keeper, which retries, and watch from now on.
					log.Error(err, "Wrote the mode, but not the fingerprint; the keeper will save it")
					saved = &fingerprint.Record{ClusterMode: res.ClusterMode.Name, LastStartupAt: m.started.UTC()}
				default:
					saved = res.Fingerprint
				}
				m.update(func(s *State) { s.Phase = PhaseRunning })
				log.Info("The ClusterMode CRDs are installed; wrote the mode", "mode", mode)
				return nil
			})
			return saved, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// running keeps the fingerprint current and watches the ClusterMode, until
// ctx is done or the CRDs are removed.
func (m *Manager) running(ctx context.Context, base fingerprint.Record) error {
	log := klog.FromContext(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		// Stop the keeper and any recreate before waiting for them.
		cancel()
		wg.Wait()
		m.update(func(s *State) {
			s.ClusterMode = nil
			m.keeper = nil
		})
	}()

	keeper := fingerprint.NewKeeper(m.kube, fingerprint.KeeperOptions{
		Saver: m.store, Base: base, Debounce: m.cfg.Debounce,
		OnChange: func(ids fingerprint.BootIDs) {
			log.Info("Saved the node fingerprint", "nodes", len(ids))
		},
	})
	wg.Go(func() {
		if err := keeper.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error(err, "The fingerprint keeper stopped")
		}
	})
	m.update(func(s *State) { m.keeper = keeper })

	// Watch calls the handler one event at a time, and returns only after
	// the last call, so stopRecreate needs no lock and wg.Go comes before
	// wg.Wait.
	stopRecreate := func() {}
	return m.reader.Watch(runCtx, func(e clustermode.Event) {
		m.update(func(s *State) { s.ClusterMode = e.ClusterMode })
		stopRecreate()
		switch e.Type {
		case clustermode.Deleted, clustermode.Missing:
			// Someone deleted the ClusterMode: write the current mode again,
			// until it's back. The handler doesn't wait, so a slow API server
			// can't hold up the watch.
			recreateCtx, stop := context.WithCancel(runCtx)
			stopRecreate = stop
			wg.Go(func() { m.recreate(runCtx, recreateCtx) })
		default:
			// An adopted change reaches the state through OnChange.
			m.writer.Observe(e.ClusterMode)
		}
	})
}

// recreate writes the current mode until it succeeds or stop is done. A
// write in flight finishes with ctx: the event it causes stops the retries,
// and may arrive before its response.
func (m *Manager) recreate(ctx, stop context.Context) {
	log := klog.FromContext(ctx)
	err := m.retry(stop, func() error {
		m.changeMu.Lock()
		defer m.changeMu.Unlock()
		mode := m.currentMode()
		if _, err := m.writer.Apply(ctx, mode); err != nil {
			return fmt.Errorf("recreating the ClusterMode: %w", err)
		}
		log.Info("Recreated the ClusterMode", "mode", mode)
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error(err, "Stopped recreating the ClusterMode")
	}
}

// currentMode is the mode last written or adopted, or, before either, the
// startup decision.
func (m *Manager) currentMode() string {
	if mode, ok := m.writer.Current(); ok {
		return mode
	}
	return m.State().Mode
}

// SetMode changes the mode: with check, only if the API server would accept
// it, and without, by writing it and letting the API server decide. Without
// the CRDs, it holds the mode in memory.
func (m *Manager) SetMode(ctx context.Context, mode string, check bool) error {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	s := m.State()
	switch {
	case s.Phase == PhaseStarting:
		return fmt.Errorf("%w: the startup decision hasn't been made yet", ErrNotReady)
	case s.Move != nil && !s.Move.Done:
		return ErrBusy
	case s.Phase == PhaseNoCRDs:
		if err := m.validInMemory(mode); err != nil {
			m.setError(err)
			return err
		}
		m.update(func(s *State) { s.Mode, s.LastError = mode, "" })
		return nil
	}
	var err error
	if check {
		_, err = m.writer.Set(ctx, mode)
	} else {
		_, err = m.writer.Apply(ctx, mode)
	}
	if err != nil {
		m.setError(err)
		return err
	}
	// The mode is written, and OnChange has made it the current mode.
	m.update(func(s *State) { s.LastError = "" })
	return nil
}

// validInMemory checks a mode set while the CRDs are missing. Nothing checks
// it against a definition until it's written, so it must at least be a mode
// the CRD accepts, or the write would fail for good.
func (m *Manager) validInMemory(mode string) error {
	if errs := validation.IsDNS1123Label(mode); len(errs) > 0 {
		return fmt.Errorf("%w: %q: %s", ErrInvalidMode, mode, strings.Join(errs, "; "))
	}
	if len(m.cfg.KnownModes) > 0 && !slices.Contains(m.cfg.KnownModes, mode) {
		return fmt.Errorf("%w: %q isn't one of --known-modes", ErrInvalidMode, mode)
	}
	return nil
}

// MoveTo starts moving to target in the background, as many steps as the
// transitions need. StopMove stops it.
func (m *Manager) MoveTo(ctx context.Context, target string) error {
	// Taken first, so a move never starts while SetMode is writing.
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	m.mu.Lock()
	if m.state.Move != nil && !m.state.Move.Done {
		m.mu.Unlock()
		return ErrBusy
	}
	if m.state.Phase != PhaseRunning {
		phase := m.state.Phase
		m.mu.Unlock()
		return fmt.Errorf("%w: can't move while %s", ErrNotReady, phase)
	}
	ctx, stop := context.WithCancel(ctx)
	m.state.Move = &MoveInfo{Target: target, Started: time.Now(), Steps: []StepInfo{}}
	m.stopMove = stop
	m.mu.Unlock()

	go func() {
		defer stop()
		log := klog.FromContext(ctx)
		_, err := m.writer.MoveTo(ctx, target, clustermode.MoveOptions{
			StepTimeout: m.cfg.MoveStepTimeout,
			OnStep: func(s clustermode.Step) error {
				m.update(func(st *State) {
					st.Move.Steps = append(st.Move.Steps, StepInfo{
						N: s.N, Of: s.Of, From: s.From, To: s.To,
						Phase: string(s.Phase),
					})
				})
				log.Info("Move step", "step", s.N, "of", s.Of, "from", s.From, "to", s.To, "phase", s.Phase)
				return nil
			},
		})
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			err = errors.New("stopped")
		}
		m.update(func(st *State) {
			st.Move.Done = true
			m.stopMove = nil
			if err != nil {
				st.Move.Error, st.Move.Reason = err.Error(), string(clustermode.ReasonOf(err))
			}
		})
		if err != nil {
			log.Error(err, "The move stopped", "target", target)
		}
	}()
	return nil
}

// StopMove stops the running move, and reports whether there was one. A
// change it is writing at that moment may still be made; the watch then
// reports it.
func (m *Manager) StopMove() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopMove == nil {
		return false
	}
	m.stopMove()
	return true
}

func (m *Manager) setError(err error) {
	m.update(func(s *State) { s.LastError = err.Error() })
}

func (m *Manager) update(fn func(*State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.state)
}

// retry calls fn until it succeeds or ctx is done, backing off between
// attempts. The page shows the last error until an attempt succeeds.
func (m *Manager) retry(ctx context.Context, fn func() error) error {
	log := klog.FromContext(ctx)
	delay := time.Second
	for failed := false; ; failed = true {
		err := fn()
		if err == nil {
			if failed {
				m.update(func(s *State) { s.LastError = "" })
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Error(err, "Will retry", "after", delay)
		m.setError(err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, 30*time.Second)
	}
}
