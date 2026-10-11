package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/container"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

func TestRunArgs(t *testing.T) {
	got := strings.Join(runArgs(container.Spec{
		Name: "dft-x", Image: "img:1", Init: true, NoNetwork: true,
		Mounts: []container.Mount{{Source: "/tmp/c", Target: "/app/config"}}, Publish: []int{8000},
		Entrypoint: []string{"/bin/sh", "-c", "exit 0"},
	}), " ")
	want := "--name dft-x --init --network none -v /tmp/c:/app/config:ro -p 127.0.0.1::8000 --entrypoint /bin/sh img:1 -c exit 0"
	if got != want {
		t.Errorf("argumenty:\n%s\noczekiwano:\n%s", got, want)
	}
	if got := strings.Join(runArgs(container.Spec{Image: "img:1"}), " "); got != "img:1" {
		t.Errorf("obraz jak w produkcji: %q", got)
	}
}

// TestRunToExit: kod wyjścia różny od zera to wynik, a niemożność
// uruchomienia — błąd.
func TestRunToExit(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"docker run --rm --name odmowa img:1":    {Stderr: "BŁĄD KONFIGURACJI", Code: 2},
		"docker run --rm --name ok img:1":        {Stdout: "Python 3.14.0"},
		"docker run --rm --name brak-demona img": {Err: context.DeadlineExceeded},
	}}
	c := Client{Runner: f}
	ctx := context.Background()
	exit, err := c.RunToExit(ctx, container.Spec{Name: "odmowa", Image: "img:1"}, 0)
	if err != nil || exit.Code != 2 || exit.Output() != "BŁĄD KONFIGURACJI" || exit.TimedOut {
		t.Errorf("odmowa: %+v, %v", exit, err)
	}
	if exit, err := c.RunToExit(ctx, container.Spec{Name: "ok", Image: "img:1"}, 0); err != nil || exit.Code != 0 || exit.Stdout != "Python 3.14.0" {
		t.Errorf("ok: %+v, %v", exit, err)
	}
	if _, err := c.RunToExit(ctx, container.Spec{Name: "brak-demona", Image: "img"}, 0); err == nil {
		t.Error("awaria uruchomienia jako wynik")
	}
	if _, err := c.RunToExit(ctx, container.Spec{Image: "img"}, time.Second); err == nil {
		t.Error("limit czasu bez nazwy kontenera przyjęty")
	}
}

// TestRunToExitTimeout: po limicie kontener jest usuwany, a wynik mówi, że
// nie zakończył się sam.
func TestRunToExitTimeout(t *testing.T) {
	removed := make(chan struct{})
	runner := proc.RunnerFunc(func(ctx context.Context, c proc.Cmd) (proc.Result, error) {
		switch c.Args[0] {
		case "rm":
			close(removed)
			return proc.Result{}, nil
		case "run":
			<-removed // `docker run` kończy się, gdy kontener zostanie usunięty
			return proc.Result{}, &proc.ExitError{Cmd: c.String(), Code: 137}
		}
		return proc.Result{}, nil
	})
	exit, err := Client{Runner: runner}.RunToExit(context.Background(), container.Spec{Name: "wstal", Image: "img"}, 10*time.Millisecond)
	if err != nil || !exit.TimedOut {
		t.Errorf("po limicie: %+v, %v", exit, err)
	}
}

func TestHostPortAndRemove(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"docker port app 8000/tcp":      {Stdout: "127.0.0.1:49153\n"},
		"docker port zly 8000/tcp":      {Stdout: "0.0.0.0:49153\n"},
		"docker rm --force a b":         {Stderr: "Error response from daemon: No such container: b", Code: 1},
		"docker rm --force zablokowany": {Stderr: "permission denied", Code: 1},
	}}
	c := Client{Runner: f}
	ctx := context.Background()
	if port, err := c.HostPort(ctx, "app", 8000); err != nil || port != 49153 {
		t.Errorf("port: %d, %v", port, err)
	}
	if _, err := c.HostPort(ctx, "zly", 8000); err == nil {
		t.Error("port spoza 127.0.0.1 przyjęty")
	}
	if err := c.Remove(ctx, "a", "b"); err != nil {
		t.Errorf("kontener już usunięty: %v", err)
	}
	if err := c.Remove(ctx, "zablokowany"); err == nil {
		t.Error("błąd sprzątania przemilczany")
	}
}

// TestBuildTestStage: etap testów — wskazany etap Dockerfile, nazwany
// kontekst i wynik tylko w pamięci podręcznej, bez sprawdzania sterownika.
func TestBuildTestStage(t *testing.T) {
	var got []string
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		got = c.Args
		return proc.Result{}, nil
	})
	_, err := Client{Runner: runner}.Build(context.Background(), artifact.BuildRequest{
		ContextDir: "services/api", Target: "test", CacheOnly: true,
		Contexts: []artifact.BuildContext{{Name: "config", Dir: "deploy/config"}},
		Args:     []artifact.BuildArg{{Name: "SOURCE_DATE_EPOCH", Value: "1"}},
	})
	want := "buildx build --target test --build-context config=deploy/config --build-arg SOURCE_DATE_EPOCH=1 --output type=cacheonly --progress plain services/api"
	if err != nil || strings.Join(got, " ") != want {
		t.Errorf("build etapu testów: %q, %v", strings.Join(got, " "), err)
	}
}

// TestEngineAndImages: właściwości demona i operacje na obrazach — z nich
// etap wdrożenia sprawdza warunki wstępne i taguje obraz lokalny.
func TestEngineAndImages(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"docker info --format {{.CgroupVersion}}":                   {Stdout: "2\n"},
		"docker info --format {{.SecurityOptions}}":                 {Stdout: "[name=seccomp,profile=builtin name=rootless name=cgroupns]\n"},
		"docker image inspect img:1 --format {{.Id}}":               {Stdout: "sha256:" + strings.Repeat("a", 64) + "\n"},
		"docker image inspect zly --format {{.Id}}":                 {Stdout: "sha256:abc\n"},
		"docker image inspect img:1 --format {{.Size}}":             {Stdout: "121701125\n"},
		"docker tag img:1 img:local-aaaaaaaaaaaa":                   {},
		"docker run -d --rm --name app -p 127.0.0.1::8000 img:1":    {Stdout: "id\n"},
		"docker stop --timeout 3 app":                               {},
		"docker run -d --rm --name zajety -p 127.0.0.1::8000 img:1": {Code: 125, Stderr: "Conflict"},
	}}
	c := Client{Runner: f}
	ctx := context.Background()
	if v, err := c.CgroupVersion(ctx); err != nil || v != "2" {
		t.Errorf("cgroup: %q, %v", v, err)
	}
	if rl, err := c.Rootless(ctx); err != nil || !rl {
		t.Errorf("rootless: %v, %v", rl, err)
	}
	if id, err := c.ImageID(ctx, "img:1"); err != nil || id != strings.Repeat("a", 64) {
		t.Errorf("identyfikator: %q, %v", id, err)
	}
	if _, err := c.ImageID(ctx, "zly"); err == nil {
		t.Error("skrócony identyfikator przyjęty")
	}
	if size, err := c.ImageSize(ctx, "img:1"); err != nil || size != 121701125 {
		t.Errorf("rozmiar: %d, %v", size, err)
	}
	if err := c.Tag(ctx, "img:1", "img:local-aaaaaaaaaaaa"); err != nil {
		t.Error(err)
	}
	app := container.Spec{Name: "app", Image: "img:1", Publish: []int{8000}}
	if err := c.Start(ctx, app); err != nil {
		t.Error(err)
	}
	if err := c.Stop(ctx, "app", 3*time.Second); err != nil {
		t.Error(err)
	}
	app.Name = "zajety"
	if err := c.Start(ctx, app); err == nil || !strings.Contains(err.Error(), "zajety") {
		t.Errorf("konflikt nazwy: %v", err)
	}
}
