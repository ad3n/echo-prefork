package prefork

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
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
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	for pid := range p.childs {
		pgid := -pid
		if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
			fmt.Printf("prefork: failed to kill child group %d: %s\n", pid, err.Error())
		}
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

			engine.Listener = ln

			go watchMaster()

			return engine.Server.Serve(engine.Listener)
		}

		engine.Listener = ln

		go watchMaster()

		return engine.Start(address)
	}

	type child struct {
		err error
		pid int
	}

	channel := make(chan child, workers)

	defer func() {
		p.KillChilds()

		close(channel)
	}()

	pids := make([]int, 0, workers)
	for range workers {
		cmd := exec.Command(os.Args[0], os.Args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true,
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
		defer p.mutex.Unlock()

		p.childs[pid] = cmd
		pids = append(pids, pid)

		go func() {
			channel <- child{pid: pid, err: cmd.Wait()}
		}()
	}

	return (<-channel).err
}

func watchMaster() {
	for range time.NewTicker(5 * time.Second).C {
		if os.Getppid() == 1 {
			os.Exit(1)
		}
	}
}
