package ingress

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
)

// RolloutReport — rolling update aplikacji pod ruchem przez bramę (§5.7
// kontraktu, decyzja 60).
type RolloutReport struct {
	Action         string         `json:"action"`
	Hostname       string         `json:"hostname"`
	Deployment     string         `json:"deployment"`
	Requests       int            `json:"requests"`
	Failed         int            `json:"failed"`
	Codes          map[string]int `json:"codes"`
	MaxGapSeconds  float64        `json:"max_gap_seconds"`
	RolloutSeconds float64        `json:"rollout_seconds"`
	Unmet          []string       `json:"unmet"`
}

// sample — jedno żądanie pętli.
type sample struct {
	at time.Time
	ok bool
}

// Rollout restartuje Deployment aplikacji (rolling update — ta sama ścieżka
// co każdy upgrade charta) pod ruchem przez bramę (UnderTraffic).
func (c *Checker) Rollout(ctx context.Context, deployment string, wait time.Duration) (RolloutReport, error) {
	return c.UnderTraffic(ctx, "rolling update", deployment, wait, func(ctx context.Context) error {
		return c.Cluster.RestartDeployment(ctx, c.Cfg.AppNamespace, deployment)
	})
}

// UnderTraffic wykonuje action pod ruchem HTTPS przez bramę: pętla co
// ConfirmInterval, każde żądanie nowym połączeniem; po akcji czeka, aż
// Deployment ma komplet zaktualizowanych i dostępnych replik. Przestój —
// najdłuższa przerwa między kolejnymi udanymi odpowiedziami — ma się
// zmieścić w Z, bez ani jednego nieudanego żądania (§5.7). what — nazwa
// akcji w komunikatach: rolling update, upgrade, rollback (decyzje 60, 79).
func (c *Checker) UnderTraffic(ctx context.Context, what, deployment string, wait time.Duration, action func(context.Context) error) (rep RolloutReport, err error) {
	rep = RolloutReport{Action: what, Deployment: deployment, Codes: map[string]int{}, Unmet: []string{}}
	defer func() {
		if cli.ExitCode(err) == cli.ExitUnmet {
			rep.Unmet = append(rep.Unmet, err.Error())
		}
	}()
	gw, err := c.Cluster.Gateway(ctx, c.Cfg.GatewayNamespace, c.Cfg.Gateway)
	if err != nil {
		return rep, err
	}
	rep.Hostname = gw.HTTPSHostname
	if rep.Hostname == "" {
		return rep, cli.Unmet("Gateway %s nie ma listenera https z nazwą hosta", c.Cfg.Gateway)
	}
	trust, err := c.trust(ctx, gw.Issuer)
	if err != nil {
		return rep, err
	}
	req := c.https(rep.Hostname, trust)
	if resp, err := c.Edge.Get(ctx, req); err != nil || resp.Status != 200 {
		return rep, cli.Unmet("%s: HTTPS do %s przed akcją nie odpowiada 200 (%v)", what, rep.Hostname, err)
	}

	var mu sync.Mutex
	var samples []sample
	loopCtx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for loopCtx.Err() == nil {
			resp, err := c.Edge.Get(loopCtx, req)
			if loopCtx.Err() != nil {
				return
			}
			code := "błąd"
			if err == nil {
				code = fmt.Sprint(resp.Status)
			}
			mu.Lock()
			samples = append(samples, sample{at: time.Now(), ok: err == nil && resp.Status == 200})
			rep.Codes[code]++
			mu.Unlock()
			_ = poll.Sleep(loopCtx, c.Cfg.confirmInterval())
		}
	}()

	if err := poll.Sleep(ctx, c.Cfg.settle()); err != nil {
		return rep, err
	}
	started := time.Now()
	cli.Step("%s: %s/%s pod ruchem przez %s", what, c.Cfg.AppNamespace, deployment, rep.Hostname)
	if err := action(ctx); err != nil {
		return rep, err
	}
	// Najpierw kontroler musi zobaczyć nową generację, potem komplet
	// zaktualizowanych i dostępnych replik.
	err = poll.Until(ctx, wait, time.Second, func(ctx context.Context) (bool, error) {
		r, err := c.Cluster.Rollout(ctx, c.Cfg.AppNamespace, deployment)
		return err == nil && r.Complete() && time.Since(started) > time.Second, err
	})
	if errors.Is(err, poll.ErrTimeout) {
		return rep, cli.Unmet("%s: Deployment %s nie skończył rolloutu w %v", what, deployment, wait)
	}
	if err != nil {
		return rep, err
	}
	rep.RolloutSeconds = time.Since(started).Seconds()
	if err := poll.Sleep(ctx, c.Cfg.settle()); err != nil {
		return rep, err
	}
	stop()
	wg.Wait()

	gap, failed := maxGap(samples)
	rep.Requests, rep.Failed, rep.MaxGapSeconds = len(samples), failed, gap.Seconds()
	cli.Step("%s: %.1f s; żądań %d, nieudanych %d, najdłuższa przerwa %v", what, rep.RolloutSeconds, rep.Requests, rep.Failed, gap.Round(time.Millisecond))
	switch {
	case rep.Requests < contract.RolloutMinRequests:
		return rep, cli.Unmet("%s: tylko %d żądań w trakcie (minimum %d)", what, rep.Requests, contract.RolloutMinRequests)
	case rep.Failed > 0:
		return rep, cli.Unmet("%s: %d z %d żądań nie powiodło się (kody: %v)", what, rep.Failed, rep.Requests, rep.Codes)
	case gap > contract.RolloutMaxGap:
		return rep, cli.Unmet("%s: przestój %v, a Z = %v", what, gap.Round(time.Millisecond), contract.RolloutMaxGap)
	}
	return rep, nil
}

// maxGap — najdłuższa przerwa między kolejnymi udanymi odpowiedziami
// i liczba nieudanych.
func maxGap(samples []sample) (time.Duration, int) {
	var gap time.Duration
	var last time.Time
	failed := 0
	for _, s := range samples {
		if !s.ok {
			failed++
			continue
		}
		if !last.IsZero() && s.at.Sub(last) > gap {
			gap = s.at.Sub(last)
		}
		last = s.at
	}
	return gap, failed
}
