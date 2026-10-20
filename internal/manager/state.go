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
	"slices"
	"time"

	"github.com/bitasylum/mode-operator/client/clustermode"
	"github.com/bitasylum/mode-operator/client/fingerprint"
)

// State is what the Manager knows. Fields are copied out under the lock.
type State struct {
	Phase Phase
	// Mode is the current mode, as written or adopted.
	Mode         string
	ClusterMode  *clustermode.ClusterMode
	Startup      *StartupInfo
	LastAdoption *Adoption
	Move         *MoveInfo
	LastError    string
}

// StartupInfo is the startup decision.
type StartupInfo struct {
	Case     string    `json:"case"`
	Mode     string    `json:"mode"`
	Restored bool      `json:"restored"`
	Overlap  []string  `json:"overlap,omitempty"`
	Event    string    `json:"event,omitempty"`
	Message  string    `json:"message,omitempty"`
	At       time.Time `json:"at"`
}

// Adoption is a change of mode someone else made.
type Adoption struct {
	Mode string    `json:"mode"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

// MoveInfo is a move started with MoveTo.
type MoveInfo struct {
	Target  string     `json:"target"`
	Started time.Time  `json:"started"`
	Steps   []StepInfo `json:"steps"`
	Done    bool       `json:"done"`
	Error   string     `json:"error,omitempty"`
	Reason  string     `json:"reason,omitempty"`
}

// StepInfo is a step of a move.
type StepInfo struct {
	N     int    `json:"n"`
	Of    int    `json:"of"`
	From  string `json:"from"`
	To    string `json:"to"`
	Phase string `json:"phase"`
}

// State returns a copy of the Manager's state.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	if s.Move != nil {
		move := *s.Move
		move.Steps = slices.Clone(move.Steps)
		s.Move = &move
	}
	return s
}

// Snapshot is the state the web page and API report.
type Snapshot struct {
	Phase     Phase  `json:"phase"`
	Mode      string `json:"mode"`
	LastError string `json:"lastError,omitempty"`

	ClusterMode *ClusterModeView `json:"clusterMode,omitempty"`
	// Modes are the definition's modes, and Next the ones the rules allow
	// from the current mode.
	Modes      []string `json:"modes"`
	Next       []string `json:"next"`
	Restricted bool     `json:"restricted"`
	RulesError string   `json:"rulesError,omitempty"`

	Startup      *StartupInfo        `json:"startup,omitempty"`
	LastAdoption *Adoption           `json:"lastAdoption,omitempty"`
	Move         *MoveInfo           `json:"move,omitempty"`
	Fingerprint  fingerprint.BootIDs `json:"fingerprint"`
}

// ClusterModeView is the ClusterMode as the stub shows it.
type ClusterModeView struct {
	Mode       string                          `json:"mode"`
	ModeOwner  string                          `json:"modeOwner"`
	Generation int64                           `json:"generation"`
	Settled    bool                            `json:"settled"`
	Applied    string                          `json:"applied,omitempty"`
	Ready      string                          `json:"ready,omitempty"`
	Degraded   string                          `json:"degraded,omitempty"`
	Workloads  clustermode.WorkloadCounts      `json:"workloads"`
	Replicas   clustermode.ReplicaCounts       `json:"replicas"`
	Unsettled  []clustermode.UnsettledWorkload `json:"unsettled,omitempty"`
}

// Snapshot returns what the Manager reports, with the modes the rules allow
// from the current mode.
func (m *Manager) Snapshot(ctx context.Context) Snapshot {
	s := m.State()
	snap := Snapshot{
		Phase: s.Phase, Mode: s.Mode, LastError: s.LastError,
		Startup: s.Startup, LastAdoption: s.LastAdoption, Move: s.Move,
		Modes: []string{}, Next: []string{}, Fingerprint: fingerprint.BootIDs{},
	}
	m.mu.Lock()
	keeper := m.keeper
	m.mu.Unlock()
	// The keeper has no fingerprint until its first check.
	if keeper != nil {
		if ids := keeper.Current(); ids != nil {
			snap.Fingerprint = ids
		}
	}

	cm := s.ClusterMode
	if cm == nil {
		return snap
	}
	snap.ClusterMode = &ClusterModeView{
		Mode: cm.Mode, ModeOwner: cm.ModeOwner, Generation: cm.Generation, Settled: cm.Settled(),
		Applied: reason(cm, clustermode.ConditionApplied), Ready: reason(cm, clustermode.ConditionReady),
		Degraded:  reason(cm, clustermode.ConditionDegraded),
		Workloads: cm.Status.Workloads, Replicas: cm.Status.Replicas, Unsettled: cm.Status.Unsettled,
	}
	rules, err := m.reader.Rules(ctx, cm)
	if err != nil {
		snap.RulesError = err.Error()
		return snap
	}
	snap.Modes, snap.Restricted = rules.Modes, rules.Restricted()
	if next := rules.Next(cm.Mode); next != nil {
		snap.Next = next
	}
	return snap
}

func reason(cm *clustermode.ClusterMode, conditionType string) string {
	if c := cm.Condition(conditionType); c != nil {
		return string(c.Status) + "/" + c.Reason
	}
	return ""
}
