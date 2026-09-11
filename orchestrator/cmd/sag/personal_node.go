package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/supervise"
)

// The inference node the desktop carries.
//
// It ships always and runs always, rather than being fetched when somebody asks
// for it: a person opens Machines, downloads a model and it runs. That costs
// almost nothing while idle, because the engine instance is created lazily and a
// node with no models loaded holds no weights (KB/35).
//
// # Why there are two binaries
//
// candle pins cudarc with `dynamic-linking` and `default-features = false`, so
// the CUDA libraries have to resolve when the PROCESS LOADS. A GPU build on a
// machine with no NVIDIA driver therefore does not start and detect anything: it
// dies at exec, before any of its own code runs. Making one binary handle both
// would mean forking candle to flip that feature.
//
// So both are shipped and one is chosen here, by trying the GPU build and
// watching what happens.
const (
	gpuNodeBinary = "sag-inference"
	cpuNodeBinary = "sag-inference-cpu"
	// How long the GPU build gets to prove it can run. It is answering a
	// question about dynamic linking, which is settled in milliseconds; a build
	// still thinking after this is one we are not going to rely on.
	probeTimeout = 20 * time.Second
)

type nodeChoice struct {
	binary string
	// Accelerated is what the person is told on the Machines screen, and Reason
	// is why. A model that runs a hundred times slower than expected should not
	// be a mystery.
	accelerated bool
	reason      string
}

// chooseNode picks the build this machine can actually run.
//
// The probe runs the GPU build's own `check` command rather than looking for
// nvcuda.dll or libcuda.so.1 in the places they are usually kept. Presence on
// disk is not the question: the question is whether THIS binary loads on THIS
// machine, and a driver too old for the build it was compiled against passes a
// file check and fails an exec. Running it answers exactly what is being asked.
//
// It is deliberately conservative. Anything other than a clean success falls
// back, because a false negative costs speed while a false positive costs a node
// that will not start.
// goos is a parameter rather than runtime.GOOS so the platform branch can be
// exercised from a test on any machine. The one that matters most (a Windows box
// with no NVIDIA driver) is not a machine this is developed on.
func chooseNode(ctx context.Context, dir, goos string, logger zerolog.Logger) (nodeChoice, error) {
	gpu := filepath.Join(dir, exeNameFor(gpuNodeBinary, goos))
	cpu := filepath.Join(dir, exeNameFor(cpuNodeBinary, goos))

	gpuExists := exists(gpu)
	cpuExists := exists(cpu)
	if !gpuExists && !cpuExists {
		return nodeChoice{}, fmt.Errorf("no inference node was installed in %s", dir)
	}

	// macOS is the simple case: Metal is on every Mac we support, so there is
	// one build and nothing to choose between.
	if goos == "darwin" {
		if gpuExists {
			return nodeChoice{binary: gpu, accelerated: true, reason: "running on the graphics processor"}, nil
		}
		return nodeChoice{binary: cpu, reason: "running on the processor"}, nil
	}

	if !gpuExists {
		return nodeChoice{binary: cpu, reason: "this installation has no graphics build"}, nil
	}
	if !cpuExists {
		return nodeChoice{binary: gpu, accelerated: true, reason: "running on the graphics processor"}, nil
	}

	probe, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probe, gpu, "check") //nolint:gosec // a path from our own installation
	supervise.HideConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nodeChoice{binary: gpu, accelerated: true, reason: "running on the graphics processor"}, nil
	}

	logger.Debug().Err(err).Str("output", lastLines(string(out), 3)).
		Msg("the graphics build did not run on this machine")
	return nodeChoice{
		binary: cpu,
		reason: "no usable NVIDIA driver was found, so models run on the processor",
	}, nil
}

// startLocalNode brings the node up beside everything else and hands back the
// way to stop it.
//
// It gets a supervisor of its OWN, and that is the important part. The database
// giving up must take the application down, because there is no application
// without it. The node giving up must not: a machine with a broken graphics
// build should lose local inference and keep its chat, its cloud models and its
// history. So this supervisor's failure is logged and the process carries on.
func startLocalNode(ctx context.Context, a *app.App, cfg *config.Config, logger zerolog.Logger) func() {
	// A platform we ship no engine for has no node to start, and this is the one
	// place that decides it. The console learns the same thing from the posture,
	// which reads the same constant, so the two cannot drift into an
	// installation that offers local models and never starts anything.
	if !config.EngineBundled {
		return func() {}
	}
	stop, err := launchLocalNode(ctx, a, cfg, logger)
	if err != nil {
		logger.Warn().Err(err).Msg("local models are not available on this computer")
		return func() {}
	}
	return stop
}

func launchLocalNode(ctx context.Context, a *app.App, cfg *config.Config, logger zerolog.Logger) (func(), error) {
	bundle, err := personalBundleDir(cfg)
	if err != nil {
		return nil, err
	}
	state, err := personalStateDir(cfg)
	if err != nil {
		return nil, err
	}

	choice, err := chooseNode(ctx, filepath.Dir(bundle), runtime.GOOS, logger)
	if err != nil {
		return nil, err
	}
	cfg.PersonalAccelerated, cfg.PersonalAcceleratorReason = choice.accelerated, choice.reason
	logger.Info().Bool("accelerated", choice.accelerated).
		Str("binary", filepath.Base(choice.binary)).Msg(choice.reason)

	// The machine registers itself the way every machine does (KB/35), over
	// loopback. Nothing here is a special case: it mints its own id and key, it
	// gets a certificate from our authority, and it appears on the Machines
	// screen as an ordinary machine that happens to be this computer.
	//
	// A fresh token on every start, in the owner's name, and this computer spends
	// it seconds later. The second start does not need one at all: by then this
	// machine has a key of its own and re-enrols with that.
	owner, err := a.Store.Users().GetByEmail(ctx, personalOwnerEmail)
	if err != nil {
		return nil, fmt.Errorf("find the owner to mint a join token for: %w", err)
	}
	token, err := a.MintJoinToken(ctx, owner.ID)
	if err != nil {
		return nil, fmt.Errorf("mint a join token: %w", err)
	}

	models := filepath.Join(state, "models")
	if err := os.MkdirAll(models, 0o700); err != nil {
		return nil, fmt.Errorf("create the model directory: %w", err)
	}
	addr, err := nodeAddress(state)
	if err != nil {
		return nil, err
	}

	// The credential for weights published under terms somebody accepted, if
	// this installation has one.
	//
	// Read at every START rather than once at boot, because the supervisor calls
	// Start again on a restart: a person who sets a credential and restarts the
	// application gets a node that has it, without the gateway needing to reach
	// into a running node to tell it.
	//
	// Its absence is not an error. A deployment that only ever fetches open
	// weights has none, which is the ordinary state, and the node says so on the
	// screen where somebody is choosing a model rather than failing on the wire.
	libraryCredential := func(ctx context.Context) string {
		token, err := a.LibraryCredential(ctx)
		if err != nil {
			logger.Warn().Err(err).Msg("the model library credential could not be read")
			return ""
		}
		return token
	}

	child := supervise.Child{
		Name: "inference",
		// Permanent: it is part of the product, not an extra. What keeps a
		// broken one from taking the application down is this supervisor being
		// separate, not the child being disposable.
		Restart: supervise.Permanent,
		// Loading an engine is slow and the join retries on its own clock, so
		// readiness here is the process running rather than the node being
		// registered. A node that has not joined yet shows on Machines as what
		// it is; a readiness probe that waited for it would call a healthy node
		// dead.
		Shutdown: 20 * time.Second,
		Start: func(ctx context.Context) (supervise.Process, error) {
			cmd := exec.CommandContext(context.WithoutCancel(ctx), choice.binary, "serve") //nolint:gosec
			supervise.HideConsole(cmd)
			cmd.Env = append(os.Environ(),
				"SAG_NODE_ADDR="+addr,
				"SAG_NODE_ADVERTISE="+addr,
				"SAG_NODE_DATA="+models,
				"SAG_NODE_NAME=This computer",
				"SAG_URL=http://"+cfg.HTTPAddr,
				"SAG_JOIN_TOKEN="+token,
			)
			if credential := libraryCredential(ctx); credential != "" {
				cmd.Env = append(cmd.Env, "SAG_NODE_HUB_TOKEN="+credential)
			}
			log, err := os.OpenFile(filepath.Join(state, "run", "inference.log"), //nolint:gosec // a path this process built
				os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return nil, fmt.Errorf("open the inference log: %w", err)
			}
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				_ = log.Close()
				return nil, fmt.Errorf("start the inference node: %w", err)
			}
			return &nodeProcess{cmd: cmd, log: log}, nil
		},
	}

	sup := supervise.New(logger, supervise.DefaultIntensity)
	sup.Add(child)

	supCtx, stopSup := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan error, 1)
	go func() { done <- sup.Run(supCtx) }()

	return func() {
		stopSup()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn().Err(err).Msg("the inference node did not stop cleanly")
		}
	}, nil
}

type nodeProcess struct {
	cmd *exec.Cmd
	log *os.File
}

func (p *nodeProcess) Stop() error { return supervise.Terminate(p.cmd.Process) }
func (p *nodeProcess) Kill() error { return p.cmd.Process.Kill() }
func (p *nodeProcess) Wait() error {
	err := p.cmd.Wait()
	_ = p.log.Close()
	return err
}

func exeNameFor(name, goos string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

// nodeAddress is where this machine listens, and it is the SAME address every
// start.
//
// A machine registers its address once, when it joins, and keeps its identity on
// disk forever after; it does not re-announce on every boot. So a fresh port
// each time left the row pointing at a port nothing was listening on, and the
// Machines screen said "unreachable" about a node that was running perfectly.
//
// The port is therefore chosen once and kept beside the rest of this
// installation's state. If something else has taken it since, a new one is
// chosen and recorded, which is the only case where the stored address is wrong
// for one start.
func nodeAddress(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "inference.port")
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // a path this process built
		if port, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && port > 0 {
			if free(port) {
				return fmt.Sprintf("127.0.0.1:%d", port), nil
			}
		}
	}
	addr, err := freeLoopbackPort()
	if err != nil {
		return "", err
	}
	_, port, _ := net.SplitHostPort(addr)
	if err := os.WriteFile(path, []byte(port), 0o600); err != nil {
		return "", fmt.Errorf("record the inference port: %w", err)
	}
	return addr, nil
}

// free reports whether we can still have the port we had last time.
func free(port int) bool {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// freeLoopbackPort asks the operating system for a port and hands it back. The
// gap between letting go and the node binding is not closed, and does not need
// to be: a start that loses that race is a start that failed, and the supervisor
// is what retries.
func freeLoopbackPort() (string, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("find a free port for the inference node: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

// lastLines keeps the tail of a process's complaint, which is the part worth
// putting in a log line.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
