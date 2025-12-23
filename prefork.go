package prefork

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/libp2p/go-reuseport"
)

const (
	preforkKey = "AD3N_PREFORK_CHILD"
	preforkVal = "1"
)

type Prefork struct {
	engine *echo.Echo
	childs map[int]*exec.Cmd
	mutex  sync.RWMutex
}

func New(engine *echo.Echo) *Prefork {
	return &Prefork{
		engine: engine,
		childs: make(map[int]*exec.Cmd),
		mutex:  sync.RWMutex{},
	}
}

func IsChild() bool {
	return os.Getenv(preforkKey) == preforkVal
}

func (p *Prefork) StartTLS(workers int, address string, tlsConfig *tls.Config) error {
	return p.fork(p.engine, workers, address, tlsConfig)
}

func (p *Prefork) Start(workers int, address string) error {
	return p.fork(p.engine, workers, address, nil)
}

func (p *Prefork) KillChilds() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	for pid, cmd := range p.childs {
		err := syscall.Kill(-pid, syscall.SIGTERM)
		if err != nil {
			fmt.Printf("prefork: failed to kill PGID %d: %s\n", pid, err.Error())

			if cmd != nil && cmd.Process != nil {
				err = cmd.Process.Kill()
				if err != nil {
					fmt.Printf("prefork: failed to directly kill PID %d: %s\n", pid, err.Error())
				} else {
					fmt.Printf("prefork: directly killed PID %d\n", pid)
				}
			}
		} else {
			fmt.Printf("prefork: sent SIGTERM to PGID %d\n", pid)
		}
	}

	p.reapZombies()
}

func (p *Prefork) reapZombies() {
	for {
		var status syscall.WaitStatus
		var rusage syscall.Rusage
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, &rusage)
		if pid <= 0 || err != nil {
			break
		}

		fmt.Printf("reaped zombie child PID %d\n", pid)
	}
}

func (p *Prefork) fork(engine *echo.Echo, workers int, address string, tlsConfig *tls.Config) error {
	var ln net.Listener
	var err error

	if IsChild() {
		runtime.GOMAXPROCS(1)

		ln, err = reuseport.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("prefork: %s", err.Error())
		}

		if tlsConfig != nil {
			ln = tls.NewListener(ln, tlsConfig)
		}

		engine.Listener = ln

		sigCh := make(chan os.Signal, 1)

		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			<-sigCh

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			engine.Shutdown(ctx)
			os.Exit(0)
		}()

		go watchMaster()

		return engine.Server.Serve(ln)
	}

	type child struct {
		err error
		pid int
	}

	channel := make(chan child, workers)

	defer func() {
		p.KillChilds()
	}()

	pids := make([]int, 0, workers)
	for range workers {
		cmd := exec.Command(os.Args[0], os.Args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true,
			Pgid:    0,
		}

		env := strings.Builder{}
		env.WriteString(preforkKey)
		env.WriteString("=")
		env.WriteString(preforkVal)

		cmd.Env = append(os.Environ(), env.String())

		if err = cmd.Start(); err != nil {
			return fmt.Errorf("failed to start a child prefork process, error: %s", err.Error())
		}

		pid := cmd.Process.Pid

		p.mutex.Lock()
		p.childs[pid] = cmd
		p.mutex.Unlock()

		pids = append(pids, pid)

		go func() {
			channel <- child{pid: pid, err: cmd.Wait()}
		}()
	}

	printPreforkBanner(os.Getpid(), workers, pids, address)

	return (<-channel).err
}

func printPreforkBanner(masterPID, workers int, pids []int, address string) {
	green := "\033[32m"
	blue := "\033[34m"
	reset := "\033[0m"
	bold := "\033[1m"

	banner := fmt.Sprintf(`
%s    ______     __
   / ____/____/ /_  ____
  / __/ / ___/ __ \/ __ \
 / /___/ /__/ / / / /_/ /
/_____/\___/_/ /_/\____/%s%sPrefork%s
`, green, blue, bold, reset)

	fmt.Print(banner)

	fmt.Println(bold + "=======================================" + reset)
	fmt.Printf("%sMaster PID   : %s%d%s\n", bold, green, masterPID, reset)
	fmt.Printf("%sListening on : %s%s%s\n", bold, green, address, reset)
	fmt.Printf("%sWorkers      : %s%d%s\n", bold, green, workers, reset)
	fmt.Println(bold + "Child PIDs   :" + reset)
	for _, pid := range pids {
		fmt.Printf("    - %s%d%s\n", blue, pid, reset)
	}

	fmt.Println()
}

func watchMaster() {
	for range time.NewTicker(5 * time.Second).C {
		if os.Getppid() == 1 {
			os.Exit(1)
		}
	}
}
