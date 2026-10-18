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
// co każdy upgrade charta) pod ruchem HTTPS przez bramę: pętla co
// ConfirmInterval, każde żądanie nowym połączeniem. Przestój — najdłuższa
// przerwa między kolejnymi udanymi odpowiedziami — ma się zmieścić w Z,
// bez ani jednego nieudanego żądania.
func (c *Checker) Rollout(ctx context.Context, deployment string, wait time.Duration) (rep RolloutReport, err error) {
	rep = RolloutReport{Deployment: deployment, Codes: map[string]int{}, Unmet: []string{}}
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
		return rep, cli.Unmet("HTTPS do %s przed rolloutem nie odpowiada 200 (%v)", rep.Hostname, err)
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
	cli.Step("rolling update %s/%s pod ruchem przez %s", c.Cfg.AppNamespace, deployment, rep.Hostname)
	if err := c.Cluster.RestartDeployment(ctx, c.Cfg.AppNamespace, deployment); err != nil {
		return rep, err
	}
	// Najpierw kontroler musi zobaczyć nową generację, potem komplet
	// zaktualizowanych i dostępnych replik.
	err = poll.Until(ctx, wait, time.Second, func(ctx context.Context) (bool, error) {
		r, err := c.Cluster.Rollout(ctx, c.Cfg.AppNamespace, deployment)
		return err == nil && r.Complete() && time.Since(started) > time.Second, err
	})
	if errors.Is(err, poll.ErrTimeout) {
		return rep, cli.Unmet("rolling update %s nie skończył się w %v", deployment, wait)
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
	cli.Step("rollout %.1f s; żądań %d, nieudanych %d, najdłuższa przerwa %v", rep.RolloutSeconds, rep.Requests, rep.Failed, gap.Round(time.Millisecond))
	switch {
	case rep.Requests < contract.RolloutMinRequests:
		return rep, cli.Unmet("tylko %d żądań w trakcie rolloutu (minimum %d)", rep.Requests, contract.RolloutMinRequests)
	case rep.Failed > 0:
		return rep, cli.Unmet("%d z %d żądań nie powiodło się w trakcie rolloutu (kody: %v)", rep.Failed, rep.Requests, rep.Codes)
	case gap > contract.RolloutMaxGap:
		return rep, cli.Unmet("przestój %v w trakcie rolloutu, a Z = %v", gap.Round(time.Millisecond), contract.RolloutMaxGap)
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
