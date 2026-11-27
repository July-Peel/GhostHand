package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"ghosthand/proto"
)

type host struct {
	mu            sync.Mutex
	desktop       string
	helper        string
	battery       *int
	charging      *bool
	sampledAt     time.Time
	sentPct       *int
	sentAC        *bool
	ipc           *helperLink
	overlayMu     sync.Mutex
	taskOverlay   taskOverlay
	controls      chan overlayControl
	wake          chan struct{}
	changed       chan struct{}
	restart       chan chan error
	sleeping      atomic.Bool
	awakeDesired  atomic.Bool
	awakeChanged  chan struct{}
	sessionEvents chan struct{}
}

func newHost(desktop, helper string) *host {
	battery, charging := powerStatus()
	return &host{desktop: desktop, helper: helper, battery: battery, charging: charging, sampledAt: time.Now(),
		sentPct: battery, sentAC: charging, controls: make(chan overlayControl, 8),
		wake: make(chan struct{}, 1), changed: make(chan struct{}, 1), restart: make(chan chan error),
		awakeChanged: make(chan struct{}, 1), sessionEvents: make(chan struct{}, 1)}
}

func (p *host) applyDesktopPolicy(policy proto.DesktopPolicy) {
	if p.awakeDesired.Swap(policy.KeepSystemAwake) != policy.KeepSystemAwake {
		signal(p.awakeChanged)
	}
}

func (p *host) runAwakePolicy(ctx context.Context) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-p.awakeChanged:
			if p.awakeDesired.Load() {
				if release == nil {
					release = keepAwake()
				}
			} else if release != nil {
				release()
				release = nil
			}
		}
	}
}

func (p *host) set(desktop, helper string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	if desktop != "" && desktop != p.desktop {
		p.desktop = desktop
		changed = true
	}
	if helper != "" && helper != p.helper {
		p.helper = helper
		changed = true
	}
	if changed {
		signal(p.changed)
	}
}

func (p *host) snapshot() proto.DeviceState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.sampledAt) >= 10*time.Minute {
		p.readBattery()
	}
	return proto.DeviceState{DesktopState: p.desktop, HelperState: p.helper, BatteryPct: p.battery, Charging: p.charging}
}

func (p *host) reportedPower(snapshot proto.DeviceState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sentPct, p.sentAC = snapshot.BatteryPct, snapshot.Charging
	p.notifyPowerChange()
}

func (p *host) readBattery() {
	p.battery, p.charging = powerStatus()
	p.sampledAt = time.Now()
}

func (p *host) powerChanged() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readBattery()
	p.notifyPowerChange()
}

func (p *host) notifyPowerChange() {
	changed := (p.sentPct == nil) != (p.battery == nil)
	if p.sentPct != nil && p.battery != nil {
		changed = changed || *p.battery-*p.sentPct >= 5 || *p.sentPct-*p.battery >= 5
	}
	if changed || (p.sentAC == nil) != (p.charging == nil) ||
		(p.sentAC != nil && p.charging != nil && *p.sentAC != *p.charging) {
		signal(p.changed)
	}
}

func (p *host) restartHelper(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case p.restart <- reply:
	case <-ctx.Done():
		return errors.New("helper supervisor is unavailable")
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return errors.New("helper restart did not finish")
	}
}
