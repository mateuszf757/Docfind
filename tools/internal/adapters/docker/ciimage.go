package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// ImageExists mówi, czy obraz jest w magazynie demona.
func (c Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := c.run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	switch {
	case err == nil:
		return true, nil
	case proc.ExitCode(err) > 0:
		return false, nil
	default:
		return false, fmt.Errorf("docker image inspect %s: %w", ref, err)
	}
}

// BuildTarget buduje etap target z dockerfile do obrazu tag, z kontekstem
// dir; wyjście płynie na bieżąco.
func (c Client) BuildTarget(ctx context.Context, dockerfile, target, tag, dir string, out, errOut io.Writer) error {
	_, err := c.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: []string{"build", "-f", dockerfile, "--target", target, "-t", tag, dir}, Stdout: out, Stderr: errOut})
	if err != nil {
		return fmt.Errorf("docker build %s: %w", tag, err)
	}
	return nil
}

// RunAttached uruchamia `docker` z argumentami args i wyjściem na bieżąco;
// kod wyjścia kontenera jest w błędzie (proc.ExitCode).
func (c Client) RunAttached(ctx context.Context, args []string, out, errOut io.Writer) error {
	_, err := c.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: args, Stdout: out, Stderr: errOut})
	return err
}
