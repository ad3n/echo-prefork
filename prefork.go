package prefork

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/log"
	"github.com/libp2p/go-reuseport"
)

const (
	preforkKey = "AD3N_PREFORK_CHILD"
	preforkVal = "1"
)

var childs = make(map[int]*exec.Cmd)

type prefork struct {
	engine *echo.Echo
}

func New(engine *echo.Echo) *prefork {
	return &prefork{engine: engine}
}

func IsChild() bool {
	return os.Getenv(preforkKey) == preforkVal
}

func (p prefork) StartTLS(address string, tlsConfig *tls.Config) error {
	return fork(p.engine, address, tlsConfig)
}

func (p prefork) Start(address string) error {
	return fork(p.engine, address, nil)
}

func TotalChild() int {
	return len(childs)
}

func KillChilds() {
	for _, proc := range childs {
		if err := proc.Process.Kill(); err != nil {
			if !errors.Is(err, os.ErrProcessDone) {
				log.Errorf("prefork: failed to kill child: %s", err.Error())
			}
		}
	}
}

func fork(engine *echo.Echo, address string, tlsConfig *tls.Config) error {
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

	maxProcs := runtime.GOMAXPROCS(0)
	channel := make(chan child, maxProcs)

	defer func() {
		for _, proc := range childs {
			if err = proc.Process.Kill(); err != nil {
				if !errors.Is(err, os.ErrProcessDone) {
					log.Errorf("prefork: failed to kill child: %s", err.Error())
				}
			}
		}

		var p *os.Process
		p, err = os.FindProcess(os.Getpid())
		if err != nil {
			log.Errorf("prefork: failed to get parent process: %s", err.Error())
		}

		p.Kill()
	}()

	pids := make([]int, 0, maxProcs)
	for range maxProcs {
		cmd := exec.Command(os.Args[0], os.Args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		env := strings.Builder{}
		env.WriteString(preforkKey)
		env.WriteString("=")
		env.WriteString(preforkVal)

		cmd.Env = append(os.Environ(), env.String())

		if err = cmd.Start(); err != nil {
			return fmt.Errorf("failed to start a child prefork process, error: %s", err.Error())
		}

		pid := cmd.Process.Pid
		childs[pid] = cmd
		pids = append(pids, pid)

		go func() {
			channel <- child{pid: pid, err: cmd.Wait()}
		}()
	}

	return (<-channel).err
}

func watchMaster() {
	if runtime.GOOS == "windows" {
		p, err := os.FindProcess(os.Getppid())
		if err == nil {
			_, _ = p.Wait()
		}

		os.Exit(1)
	}

	for range time.NewTicker(500 * time.Second).C {
		if os.Getppid() == 1 {
			os.Exit(1)
		}
	}
}
