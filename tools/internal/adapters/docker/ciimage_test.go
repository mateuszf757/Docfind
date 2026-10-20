package docker

import (
	"context"
	"io"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

func TestCIImage(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"docker image inspect --format {{.Id}} jest:1":                        {Stdout: "sha256:abc"},
		"docker image inspect --format {{.Id}} brak:1":                        {Code: 1, Stderr: "No such image"},
		"docker build -f /r/ci/images/Dockerfile --target light -t brak:1 /r": {},
		"docker run --rm brak:1 true":                                         {Code: 3},
	}}
	c := Client{Runner: f}
	ctx := context.Background()
	if ok, err := c.ImageExists(ctx, "jest:1"); !ok || err != nil {
		t.Errorf("obraz jest: %v, %v", ok, err)
	}
	if ok, err := c.ImageExists(ctx, "brak:1"); ok || err != nil {
		t.Errorf("obrazu brak: %v, %v", ok, err)
	}
	if err := c.BuildTarget(ctx, "/r/ci/images/Dockerfile", "light", "brak:1", "/r", io.Discard, io.Discard); err != nil {
		t.Errorf("build: %v", err)
	}
	if err := c.RunAttached(ctx, []string{"run", "--rm", "brak:1", "true"}, io.Discard, io.Discard); proc.ExitCode(err) != 3 {
		t.Errorf("kod wyjścia kontenera: %v", err)
	}
}
