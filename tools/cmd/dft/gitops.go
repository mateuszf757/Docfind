package main

import (
	"context"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/selfheal"
)

// runCheckSelfHeal — warunek Etapu 5: ręczna zmiana zasobu aplikacji wraca
// do stanu z charta bez człowieka (stages/selfheal, decyzja 46). Raport
// w .cache/reports/selfheal-<środowisko>.json.
func runCheckSelfHeal(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check selfheal"); err != nil {
		return err
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	if !def.GitOps() {
		return cli.Unmet("środowisko %s nie ma Argo CD (delivery.argocd) — zmiany ręcznej nie ma kto przywrócić", def.Name)
	}
	if err := def.Require(environment.OpSelfHeal); err != nil {
		return err
	}
	c, err := w.connect(def)
	if err != nil {
		return err
	}
	api := def.App.Release + "-api"
	gate := selfheal.Gate{
		Cluster: c, ArgoNamespace: environment.ArgoCDNamespace, Application: def.App.Release,
		Namespace: def.App.Namespace, Deployment: api, PDB: api,
		// Argo widzi zmianę od razu (obserwuje zasoby aplikacji); samonaprawa
		// po 2 s, każda kolejna w serii trzy razy później (Argo CD 3.5:
		// --self-heal-backoff-*) — druga zmiana tej bramki to ok. 6 s.
		// Minuta to zapas na synchronizację.
		Budget: time.Minute, Interval: time.Second,
	}
	rep, err := gate.Run(ctx)
	return w.withReport("selfheal-"+def.Name, rep, err)
}
