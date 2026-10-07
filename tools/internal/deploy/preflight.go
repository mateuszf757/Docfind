package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/docker"
)

// MinInotifyInstances — minimum fs.inotify.max_user_instances. Przy Dockerze
// rootless wszystkie procesy we wszystkich kontenerach — węzły k3s, kubelety,
// containerd, każdy pod — działają na hoście jako jeden użytkownik i dzielą
// jego limit instancji inotify (domyślnie 128). Po jego wyczerpaniu Envoy
// kończy się SIGSEGV (`assert failure: inotify_fd_ >= 0`), a rolling update
// proxy podwaja na chwilę liczbę podów — właśnie wtedy limit pękał. 512 to
// wartość z dokumentacji kind, ~2× zapasu ponad pełny stos z planu.
const MinInotifyInstances = 512

// problem — warunek wstępny, którego nie spełnia maszyna, z naprawą.
type problem struct {
	what string
	fix  string
}

// preflight zbiera problemy maszyny i zgłasza je razem: część z nich
// wymaga restartu WSL, a zgłaszanie po jednym kosztowałoby restart na każdy.
type preflight struct {
	problems []problem
}

func (p *preflight) add(what, fix string) { p.problems = append(p.problems, problem{what, fix}) }

func (p *preflight) err() error {
	if len(p.problems) == 0 {
		return nil
	}
	for _, pr := range p.problems {
		fmt.Fprintf(cli.Err, "BŁĄD: %s\n", pr.what)
		for _, line := range strings.Split(pr.fix, "\n") {
			fmt.Fprintf(cli.Err, "    %s\n", line)
		}
		fmt.Fprintln(cli.Err)
	}
	return cli.Unmet("maszyna nie spełnia %d warunków wstępnych klastra — szczegóły: docs/WYMAGANIA.md", len(p.problems))
}

// checkInotify sprawdza limit i ostrzega przy wysokim zużyciu. Zużycie
// liczone z deskryptorów procesów użytkownika (anon_inode:inotify) —
// cudzych procesów i tak nie da się odczytać.
func checkInotify(p *preflight) {
	raw, err := os.ReadFile("/proc/sys/fs/inotify/max_user_instances")
	if err != nil {
		cli.Warn("nie udało się odczytać limitu inotify: %v", err)
		return
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		cli.Warn("nieczytelny limit inotify %q", raw)
		return
	}
	if limit < MinInotifyInstances {
		p.add(fmt.Sprintf("fs.inotify.max_user_instances=%d — za mało dla klastra w kontenerach (minimum %d); Envoy kończy się SIGSEGV przy wyczerpaniu.", limit, MinInotifyInstances),
			fmt.Sprintf("echo 'fs.inotify.max_user_instances=%d' | sudo tee /etc/sysctl.d/99-inotify.conf\nsudo sysctl --system", MinInotifyInstances))
		return
	}
	used := 0
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		if target, err := os.Readlink(fd); err == nil && target == "anon_inode:inotify" {
			used++
		}
	}
	cli.Step("inotify: %d z %d instancji", used, limit)
	if used*100/limit >= 80 {
		cli.Warn("zużyte %d z %d instancji inotify — rolling update może wyczerpać limit", used, limit)
	}
}

// checkDocker sprawdza cgroup v2 i kontroler cpuset widoczny z kontenera.
// k3d nie diagnozuje, dlaczego węzeł nie wstaje — czeka na „k3s is up and
// running", a po przekroczeniu czasu wycofuje klaster razem z logami węzła,
// czyli z jedynym dowodem przyczyny.
func checkDocker(ctx context.Context, d docker.Client, p *preflight, k3sImage string) error {
	res, err := d.Run(ctx, "info", "--format", "{{.CgroupVersion}}")
	if err != nil {
		return fmt.Errorf("docker info: %w", err)
	}
	if v := strings.TrimSpace(string(res.Stdout)); v != "2" {
		p.add(fmt.Sprintf("Docker działa na cgroup v%s, a kubelet wymaga cgroup v2.", v),
			"W %UserProfile%\\.wslconfig, sekcja [wsl2]: kernelCommandLine = cgroup_no_v1=all\nPotem w PowerShellu: wsl --shutdown")
		return nil
	}
	// Z wnętrza kontenera, bo tylko tam widać, co faktycznie do niego
	// dociera — host może mieć cpuset, a kontener i tak go nie dostać.
	controllers, err := d.ReadFiles(ctx, k3sImage, "/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return err
	}
	if !strings.Contains(" "+strings.TrimSpace(string(controllers))+" ", " cpuset ") {
		p.add(fmt.Sprintf("kontener nie dostaje kontrolera cgroup cpuset (widzi: %s) — k3s kończy się 'failed to find cpuset cgroup (v2)'.", strings.TrimSpace(string(controllers))),
			"Docker rootless: systemd deleguje do sesji użytkownika tylko cpu, memory i pids.\n"+
				"sudo mkdir -p /etc/systemd/system/user@.service.d\n"+
				"printf '[Service]\\nDelegate=cpu cpuset io memory pids\\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf\n"+
				"sudo systemctl daemon-reload\nPotem w PowerShellu: wsl --shutdown")
	}
	return nil
}

// checkPorts sprawdza, że porty klastra na 127.0.0.1 są wolne — konflikt
// k3d zgłosiłby dopiero w połowie tworzenia klastra. Próba związania
// i natychmiastowe zwolnienie: rozstrzyga ten sam mechanizm, który potem
// zawiódłby k3d.
func checkPorts(p *preflight, ports map[string]int) {
	var busy []string
	for name, port := range ports {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				busy = append(busy, fmt.Sprintf("%d (%s)", port, name))
				continue
			}
			busy = append(busy, fmt.Sprintf("%d (%s: %v)", port, name, err))
			continue
		}
		_ = l.Close()
	}
	if len(busy) > 0 {
		p.add("porty hosta z definicji środowiska są zajęte: "+strings.Join(busy, ", ")+".",
			"Sprawdź, kto słucha: ss -ltnp 'sport = :<port>'")
	}
}

// rootless mówi, czy Docker działa bez roota — wtedy kubelet potrzebuje
// bramki KubeletInUserNamespace (decyzja 19).
func rootless(ctx context.Context, d docker.Client) (bool, error) {
	res, err := d.Run(ctx, "info", "--format", "{{.SecurityOptions}}")
	if err != nil {
		return false, fmt.Errorf("docker info: %w", err)
	}
	return strings.Contains(string(res.Stdout), "name=rootless"), nil
}
